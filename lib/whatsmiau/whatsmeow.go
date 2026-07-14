package whatsmiau

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/verbeux-ai/whatsmiau/env"
	"github.com/verbeux-ai/whatsmiau/interfaces"
	"github.com/verbeux-ai/whatsmiau/lib/storage/gcs"
	"github.com/verbeux-ai/whatsmiau/lib/storage/local"
	"github.com/verbeux-ai/whatsmiau/models"
	"github.com/verbeux-ai/whatsmiau/repositories/instances"
	"github.com/verbeux-ai/whatsmiau/services"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.uber.org/zap"
	"golang.org/x/net/context"
)

// ChatKeyCache stores remoteJid and remoteLid for a chat, so events that arrive only with LID can be resolved to full key data.
type ChatKeyCache struct {
	RemoteJid string
	RemoteLid string
}

type Whatsmiau struct {
	clients          *xsync.Map[string, *whatsmeow.Client]
	container        *sqlstore.Container
	logger           waLog.Logger
	repo             interfaces.InstanceRepository
	qrCache          *xsync.Map[string, string]
	pairingCache     *xsync.Map[string, string]
	observerRunning  *xsync.Map[string, *whatsmeow.Client]
	instanceCache    *xsync.Map[string, models.Instance]
	lockConnection   *xsync.Map[string, *sync.Mutex]
	emitter          chan emitter
	httpClient       *http.Client
	mediaHTTPClient  *http.Client // longer timeout for document/image downloads (e.g. large PDFs)
	fileStorage      interfaces.Storage
	handlerSemaphore chan struct{}
	// chatKeyCache: key "instanceID:lid" -> ChatKeyCache, so we can resolve LID to remoteJid when an event has no remoteJid.
	chatKeyCache *xsync.Map[string, ChatKeyCache]
	// outdatedRecovery guards the 405 ClientOutdated recovery so only one runs at a time.
	outdatedRecovery atomic.Bool
}

// newWAClient creates a whatsmeow client with the settings every whatsmiau client needs.
func newWAClient(device *store.Device, logger waLog.Logger) *whatsmeow.Client {
	client := whatsmeow.NewClient(device, logger)
	// Only auto-download history sync blobs when enabled; the emitter turns them into
	// webhook events (see handleHistorySyncEvent). Otherwise notifications are ignored.
	client.ManualHistorySyncDownload = !env.Env.HistorySyncEnabled
	// Ask the primary phone to re-send messages that arrive undecryptable/unavailable
	// (broken Signal session): the recovered message is dispatched as a normal Message event.
	client.AutomaticMessageRerequestFromPhone = true
	return client
}

var instance *Whatsmiau
var mu = &sync.Mutex{}

func Get() *Whatsmiau {
	mu.Lock()
	defer mu.Unlock()
	return instance
}

func LoadMiau(ctx context.Context, container *sqlstore.Container) {
	mu.Lock()
	defer mu.Unlock()
	deviceStore, err := container.GetAllDevices(ctx)
	if err != nil {
		panic(err)
	}

	level := "INFO"
	if env.Env.DebugWhatsmeow {
		level = "DEBUG"
	}

	repo := instances.NewRedis(services.Redis())
	instanceList, err := repo.List(ctx, "")
	if err != nil {
		zap.L().Fatal("failed to list instances", zap.Error(err))
	}

	instanceByRemoteJid := make(map[string]models.Instance)
	for _, inst := range instanceList {
		if len(inst.RemoteJID) <= 0 {
			continue
		}

		instanceByRemoteJid[inst.RemoteJID] = inst
	}

	clients := xsync.NewMap[string, *whatsmeow.Client]()

	// Fetch the current WA web version before connecting: a stale baked-in version
	// makes WhatsApp reject every handshake with 405 ClientOutdated.
	if _, err := RefreshWAVersion(ctx); err != nil {
		zap.L().Warn("failed to fetch latest WA version, starting with built-in version",
			zap.Error(err), zap.String("version", store.GetWAVersion().String()))
	}

	clientLog := waLog.Stdout("Client", level, false)
	for _, device := range deviceStore {
		client := newWAClient(device, clientLog)
		if client.Store.ID == nil {
			zap.L().Error("device without id on db", zap.Any("device", device))
			continue
		}

		instanceFound, ok := instanceByRemoteJid[client.Store.ID.String()]
		if ok {
			configProxy(client, instanceFound.InstanceProxy)
			clients.Store(instanceFound.ID, client)
			if err := client.Connect(); err != nil {
				zap.L().Error("failed to connect connected device", zap.Error(err), zap.String("jid", client.Store.ID.String()))
			}
			continue
		}

		if err := client.Logout(context.TODO()); err != nil {
			zap.L().Error("failed to logout", zap.Error(err), zap.String("jid", client.Store.ID.String()))
		}
		if client.Store != nil && client.Store.ID != nil {
			if err := container.DeleteDevice(context.Background(), client.Store); err != nil {
				zap.L().Error("failed to delete device", zap.Error(err))
			}
		}
	}

	var storage interfaces.Storage
	if env.Env.GCSEnabled {
		storage, err = gcs.New(env.Env.GCSBucket)
		if err != nil {
			zap.L().Panic("failed to create GCS storage", zap.Error(err))
		}
	} else if env.Env.LocalMediaPath != "" {
		baseURL := env.Env.MediaPublicURL
		if baseURL == "" {
			baseURL = "http://localhost:" + env.Env.Port
		}
		storage, err = local.New(env.Env.LocalMediaPath, baseURL)
		if err != nil {
			zap.L().Panic("failed to create local media storage", zap.Error(err))
		}
		zap.L().Info("local media storage enabled", zap.String("dir", env.Env.LocalMediaPath), zap.String("baseURL", baseURL))
	}

	instance = &Whatsmiau{
		clients:         clients,
		container:       container,
		logger:          clientLog,
		repo:            repo,
		qrCache:         xsync.NewMap[string, string](),
		pairingCache:    xsync.NewMap[string, string](),
		instanceCache:   xsync.NewMap[string, models.Instance](),
		observerRunning: xsync.NewMap[string, *whatsmeow.Client](),
		lockConnection:  xsync.NewMap[string, *sync.Mutex](),
		emitter:         make(chan emitter, env.Env.EmitterBufferSize),
		httpClient: &http.Client{
			Timeout: time.Second * 30, // TODO: load from env
		},
		mediaHTTPClient: &http.Client{
			Timeout: time.Minute * 5, // large PDFs/images need more time to download
		},
		fileStorage:      storage,
		handlerSemaphore: make(chan struct{}, env.Env.HandlerSemaphoreSize),
		chatKeyCache:     xsync.NewMap[string, ChatKeyCache](),
	}

	go instance.startEmitter()
	go instance.runStaleInstancesCleanup()
	go instance.runWAVersionRefresher()

	clients.Range(func(id string, client *whatsmeow.Client) bool {
		zap.L().Info("starting event handler", zap.String("jid", client.Store.ID.String()))
		client.AddEventHandler(instance.Handle(id))
		return true
	})

}

func (s *Whatsmiau) Connect(ctx context.Context, id string, phoneNumber string) (qrCode string, pairingCode string, err error) {
	client, err := s.generateClient(ctx, id)
	if err != nil {
		return "", "", err
	}
	if client == nil {
		return "", "", nil
	}

	if qr, ok := s.qrCache.Load(id); ok {
		pc, _ := s.pairingCache.Load(id)
		return qr, pc, nil
	}

	return s.observeAndQrCode(ctx, id, client, phoneNumber)
}

func (s *Whatsmiau) generateClient(ctx context.Context, id string) (*whatsmeow.Client, error) {
	lock, _ := s.lockConnection.LoadOrStore(id, &sync.Mutex{})
	lock.Lock()
	defer lock.Unlock()

	// Drop in-memory client if it no longer matches Redis (e.g. session deleted server-side but this
	// process still holds the old logged-in client — next Connect would reuse the previous number).
	s.reconcileInMemoryClientWithRepo(ctx, id)

	client, ok := s.clients.Load(id)
	if !ok {
		device := s.container.NewDevice()
		client = newWAClient(device, s.logger)
		s.clients.Store(id, client)
	}

	// trying recover existent connection
	if s.hasSomeDevice(client) {
		if instanceFound := s.getInstanceCached(id); instanceFound != nil {
			configProxy(client, instanceFound.InstanceProxy)
		}

		if client.IsLoggedIn() {
			return nil, nil
		}

		if err := client.Connect(); err != nil {
			// "already connected" during the post-scan 515 restart: login completes in a
			// few seconds, so wait for it instead of failing the caller's poll.
			if waitForLogin(ctx, client, 8*time.Second) {
				return nil, nil
			}
			return nil, err
		}

		// The socket may still be mid-handshake (e.g. the 515 restart right after a QR
		// scan): deleting the device now would log out a healthy session and start a new
		// QR pairing. Give login a few seconds before treating the session as dead.
		if waitForLogin(ctx, client, 8*time.Second) {
			return nil, nil
		}
		if ctx.Err() != nil {
			// Caller gave up while we waited; don't destroy a possibly-healthy session.
			return nil, ctx.Err()
		}

		s.clients.Delete(id)
		if err := s.deleteDeviceIfExists(ctx, client); err != nil {
			zap.L().Error("failed to hard logout", zap.Error(err))
			return nil, err
		}

		device := s.container.NewDevice()
		client = newWAClient(device, s.logger)
		s.clients.Store(id, client) // replaces old client
	}

	return client, nil
}

// reconcileInMemoryClientWithRepo removes a cached client whose paired JID disagrees with instance metadata
// in Redis (or Redis has no RemoteJID while memory still has a device id). Skips while observeConnection
// is still running for that same client to avoid racing a pairing that has not written RemoteJID yet.
func (s *Whatsmiau) reconcileInMemoryClientWithRepo(ctx context.Context, id string) {
	cli, ok := s.clients.Load(id)
	if !ok || cli == nil || cli.Store == nil || cli.Store.ID == nil {
		return
	}

	if obs, obsOK := s.observerRunning.Load(id); obsOK && obs == cli {
		return
	}

	memJID := cli.Store.ID.String()
	list, err := s.repo.List(ctx, id)
	if err != nil {
		zap.L().Warn("reconcile client: list instance failed", zap.String("id", id), zap.Error(err))
		return
	}

	redisJID := ""
	if len(list) > 0 {
		redisJID = strings.TrimSpace(list[0].RemoteJID)
	}

	needDrop := false
	switch {
	case redisJID == "":
		// Instance not paired in Redis but we still have a device id in RAM → stale after delete/recreate.
		needDrop = true
	case !jidsMatchForInstance(memJID, redisJID):
		needDrop = true
	}
	if !needDrop {
		return
	}

	zap.L().Warn("dropping stale WhatsApp client: Redis instance metadata does not match in-memory session",
		zap.String("id", id),
		zap.String("memory_jid", memJID),
		zap.String("redis_remote_jid", redisJID),
	)

	s.clients.Delete(id)
	s.instanceCache.Delete(id)
	if err := s.deleteDeviceIfExists(ctx, cli); err != nil {
		zap.L().Error("reconcile: failed to delete stale device", zap.String("id", id), zap.Error(err))
	}
}

func jidsMatchForInstance(a, b string) bool {
	if a == b {
		return true
	}
	ja, ea := types.ParseJID(a)
	jb, eb := types.ParseJID(b)
	if ea != nil || eb != nil {
		return false
	}
	return ja.User == jb.User && ja.Server == jb.Server
}

// waitForLogin polls IsLoggedIn up to timeout. Returns early when the caller's ctx is done.
func waitForLogin(ctx context.Context, client *whatsmeow.Client, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if client.IsLoggedIn() {
			return true
		}
		select {
		case <-ctx.Done():
			return client.IsLoggedIn()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return client.IsLoggedIn()
}

func (s *Whatsmiau) hasSomeDevice(client *whatsmeow.Client) bool {
	noStore := client.Store == nil
	if noStore {
		return false
	}

	noDevice := client.Store.ID == nil
	if noDevice {
		return false
	}

	return true
}

func (s *Whatsmiau) observeConnection(client *whatsmeow.Client, id string, phoneNumber string) {
	existingClient, loaded := s.observerRunning.LoadOrStore(id, client)
	if loaded {
		if existingClient == client {
			zap.L().Debug("observer connection already running for this client", zap.String("id", id))
			return
		}
		zap.L().Warn("replacing stale observer connection", zap.String("id", id))
		s.observerRunning.Store(id, client)
	}

	zap.L().Debug("starting observer connection", zap.String("id", id))
	defer func() {
		zap.L().Debug("stopping observer connection", zap.String("id", id))
		if currentClient, ok := s.observerRunning.Load(id); ok && currentClient == client {
			s.observerRunning.Delete(id)
			s.qrCache.Delete(id)
			s.pairingCache.Delete(id)
		}
	}()

	ctx, cancel := context.WithTimeout(context.TODO(), time.Minute*2)
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		zap.L().Error("failed to observe QR Code", zap.Error(err))
		return
	}

	if instanceFound := s.getInstance(id); instanceFound != nil {
		configProxy(client, instanceFound.InstanceProxy)
	}
	if err := client.Connect(); err != nil {
		zap.L().Error("failed to connect connected device", zap.Error(err))
		return
	}

	zap.L().Debug("waiting for QR channel event", zap.String("id", id))
	emittedConnecting := false
	pairingRequested := false
	for {
		select {
		case <-ctx.Done(): // QR code expiration
			zap.L().Debug("context ", zap.String("id", id), zap.Error(ctx.Err()))
			if err := s.deleteDeviceIfExists(context.TODO(), client); err != nil {
				zap.L().Error("failed to hard logout", zap.String("id", id), zap.Error(err))
			}
			s.clients.Delete(id)
			return
		case evt, ok := <-qrChan:
			if !ok || evt.Event == "error" || evt.Event == "timeout" { // closed qr chan
				zap.L().Debug("QR channel closed", zap.String("id", id), zap.Any("evt", evt))
				cancel()
				continue
			}
			zap.L().Debug("received QR channel event", zap.String("id", id), zap.Any("evt", evt))
			if evt.Event == "code" {
				if !emittedConnecting {
					s.emitConnectionUpdate(id, "connecting", 0)
					emittedConnecting = true
				}
				s.qrCache.Store(id, evt.Code)

				if phoneNumber != "" && !pairingRequested {
					pairingRequested = true
					code, err := client.PairPhone(ctx, phoneNumber, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
					if err != nil {
						zap.L().Error("failed to request pairing code", zap.String("id", id), zap.Error(err))
					} else {
						s.pairingCache.Store(id, code)
					}
				}
				continue
			}

			if evt.Event == "success" || evt.Event == "logged_in" {
				if client.Store.ID == nil {
					zap.L().Error("jid is nil after login", zap.String("id", id), zap.Any("evt", evt))
					cancel()
					continue
				}

				zap.L().Info("device connected successfully", zap.String("id", id))
				client.RemoveEventHandlers()
				client.AddEventHandler(s.Handle(id))
				if _, err := s.repo.Update(context.Background(), id, &models.Instance{
					RemoteJID: client.Store.ID.String(),
				}); err != nil {
					zap.L().Error("failed to update instance after login", zap.Error(err))
				}
				s.qrCache.Delete(id)
				s.pairingCache.Delete(id)
				return
			}

			zap.L().Error("unknown event", zap.String("id", id), zap.Any("evt", evt))
		}
	}
}

func (s *Whatsmiau) observeAndQrCode(ctx context.Context, id string, client *whatsmeow.Client, phoneNumber string) (string, string, error) {
	ctx, c := context.WithTimeout(ctx, 15*time.Second)
	defer c()

	zap.L().Debug("starting observe and qr code", zap.String("id", id))
	go s.observeConnection(client, id, phoneNumber)

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Device may have been paired in observeConnection; return without QR so API reports "already connected"
			if client.IsLoggedIn() {
				zap.L().Debug("client logged in while waiting for QR, returning without QR", zap.String("id", id))
				return "", "", nil
			}
			qrCode, ok := s.qrCache.Load(id)
			if ok && len(qrCode) > 0 {
				zap.L().Debug("got qr code from cache", zap.String("id", id))
				if phoneNumber != "" {
					// wait a bit more for pairing code to be generated
					pc, pcOk := s.pairingCache.Load(id)
					if pcOk {
						return qrCode, pc, nil
					}
					continue
				}
				return qrCode, "", nil
			}
		case <-ctx.Done():
			zap.L().Debug("observe and qr code context done", zap.String("id", id), zap.Error(ctx.Err()))
			// return whatever we have so far
			qr, _ := s.qrCache.Load(id)
			pc, _ := s.pairingCache.Load(id)
			if qr != "" {
				if phoneNumber != "" && pc == "" {
					return qr, "", ctx.Err()
				}
				return qr, pc, nil
			}
			return "", "", ctx.Err()
		}
	}
}

func (s *Whatsmiau) deleteDeviceIfExists(ctx context.Context, client *whatsmeow.Client) error {
	// Detach handlers and close the socket before discarding the client. Otherwise a client
	// removed from s.clients can keep its live connection and keep firing Handle(id) events
	// ("ghost client"): those events then fail s.clients.Load and the messages are dropped.
	client.RemoveEventHandlers()

	if client.IsLoggedIn() {
		if err := client.Logout(ctx); err != nil {
			zap.L().Error("failed to logout", zap.Error(err))
			return err
		}
	} else if client.IsConnected() {
		client.Disconnect()
	}

	if client.Store != nil && client.Store.ID != nil {
		if err := s.container.DeleteDevice(ctx, client.Store); err != nil {
			zap.L().Error("failed to delete device", zap.Error(err))
			return err
		}
	}

	return nil
}

func (s *Whatsmiau) Status(id string) (Status, error) {
	client, ok := s.clients.Load(id)
	if !ok {
		return Closed, nil
	}

	if client.IsConnected() && client.IsLoggedIn() {
		return Connected, nil
	}

	// If not connected, but we have a QR code, the state is QrCode
	if _, ok := s.qrCache.Load(id); ok && client.IsConnected() {
		return QrCode, nil
	}

	if client.IsLoggedIn() {
		return Connecting, nil
	}

	return Closed, nil
}

func (s *Whatsmiau) Logout(ctx context.Context, id string) error {
	client, ok := s.clients.Load(id)
	if !ok {
		zap.L().Warn("logout: client does not exist", zap.String("id", id))
		return nil
	}

	s.clients.Delete(id)
	return s.deleteDeviceIfExists(ctx, client)
}

// Disconnect tears down the instance completely: device store, Redis, route and webhook.
// The number is removed as if it had never existed.
func (s *Whatsmiau) Disconnect(id string) error {
	s.TeardownInstance(id)
	return nil
}

// DisconnectClient closes only the WebSocket for an instance without deleting session data.
// whatsmeow will attempt automatic reconnection. Use when the session may recover (e.g. WA error 463)
// rather than on permanent logout. The Disconnected event fires the connection.update:close webhook.
func (s *Whatsmiau) DisconnectClient(id string) {
	client, ok := s.clients.Load(id)
	if !ok {
		return
	}
	client.Disconnect()
}

// PhoneE164 returns the paired WhatsApp phone number (digits) if known: in-memory client first, then Redis RemoteJID.
func (s *Whatsmiau) PhoneE164(ctx context.Context, id string) string {
	if client, ok := s.clients.Load(id); ok && client != nil && client.Store != nil && client.Store.ID != nil {
		return jidToPhoneE164(client.Store.ID.String())
	}
	list, err := s.repo.List(ctx, id)
	if err != nil || len(list) == 0 || list[0].RemoteJID == "" {
		return ""
	}
	return jidToPhoneE164(list[0].RemoteJID)
}

func jidToPhoneE164(jid string) string {
	beforeAt := strings.Split(jid, "@")[0]
	if idx := strings.Index(beforeAt, ":"); idx != -1 {
		return beforeAt[:idx]
	}
	return beforeAt
}

// runStaleInstancesCleanup runs periodically and removes instances that have not sent any webhook event in STALE_INSTANCE_DAYS.
func (s *Whatsmiau) runStaleInstancesCleanup() {
	if env.Env.StaleInstanceDays <= 0 {
		return
	}
	olderThan := time.Duration(env.Env.StaleInstanceDays) * 24 * time.Hour
	// First run after 5 minutes so startup is not blocked
	time.Sleep(5 * time.Minute)
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		redisRepo, ok := s.repo.(*instances.RedisInstance)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		stale, err := redisRepo.ListStaleInstances(ctx, olderThan)
		cancel()
		if err != nil {
			zap.L().Warn("stale instances cleanup: list failed", zap.Error(err))
			continue
		}
		for _, id := range stale {
			zap.L().Info("stale instances cleanup: removing instance with no webhook activity", zap.String("instance", id))
			s.TeardownInstance(id)
		}
	}
}

func (s *Whatsmiau) GetJidLid(ctx context.Context, id string, jid types.JID) (string, string) {
	newJid, newLid := s.extractJidLid(ctx, id, jid)
	if strings.HasSuffix(newJid, "@lid") {
		newLid = newJid
	}

	return newJid, newLid
}

func (s *Whatsmiau) extractJidLid(ctx context.Context, id string, jid types.JID) (string, string) {
	client, ok := s.clients.Load(id)
	if !ok || client == nil || client.Store == nil || client.Store.LIDs == nil {
		return jid.ToNonAD().String(), ""
	}
	if client.Store == nil || client.Store.LIDs == nil {
		return jid.ToNonAD().String(), ""
	}

	if jid.Server == types.DefaultUserServer {
		lid, err := client.Store.LIDs.GetLIDForPN(ctx, jid)
		if err != nil {
			zap.L().Warn("failed to get lid from store", zap.String("id", id), zap.Error(err))
		}

		return jid.ToNonAD().String(), lid.ToNonAD().String()
	}

	if jid.Server == types.HiddenUserServer {
		lidString := jid.ToNonAD().String()
		pnJID, err := client.Store.LIDs.GetPNForLID(ctx, jid)
		if err != nil {
			zap.L().Warn("failed to get pn for lid", zap.Stringer("lid", jid), zap.Error(err))
			return jid.ToNonAD().String(), lidString
		}

		if !pnJID.IsEmpty() {
			return pnJID.ToNonAD().String(), lidString
		}

		return lidString, lidString
	}

	return jid.ToNonAD().String(), ""
}

// StoreChatKey saves the chat key (remoteJid, remoteLid) for the given instance and LID.
// When an event later arrives with only LID, ResolveChatKey can return the full key data.
func (s *Whatsmiau) StoreChatKey(instanceID, lid, remoteJid, remoteLid string) {
	if lid == "" {
		return
	}
	key := instanceID + ":" + lid
	s.chatKeyCache.Store(key, ChatKeyCache{RemoteJid: remoteJid, RemoteLid: remoteLid})
}

// ResolveChatKey returns the stored remoteJid and remoteLid for the given instance and LID.
// ok is false if the LID was never stored (e.g. no event from that chat yet).
func (s *Whatsmiau) ResolveChatKey(instanceID, lid string) (remoteJid, remoteLid string, ok bool) {
	if lid == "" {
		return "", "", false
	}
	key := instanceID + ":" + lid
	cache, ok := s.chatKeyCache.Load(key)
	if !ok {
		return "", "", false
	}
	return cache.RemoteJid, cache.RemoteLid, true
}

// ClearChatKeyCache removes all stored chat keys for the given instance (e.g. on teardown).
func (s *Whatsmiau) ClearChatKeyCache(instanceID string) {
	prefix := instanceID + ":"
	s.chatKeyCache.Range(func(key string, _ ChatKeyCache) bool {
		if strings.HasPrefix(key, prefix) {
			s.chatKeyCache.Delete(key)
		}
		return true
	})
}

// ResolveChatKeyFromStore resolves remoteJid from the client's LID store when we have only lid (e.g. event key without remoteJid).
// Uses GetPNForLID. On success, stores the result in chatKeyCache for next time.
func (s *Whatsmiau) ResolveChatKeyFromStore(ctx context.Context, instanceID, lid string) (remoteJid, remoteLid string, ok bool) {
	if lid == "" {
		return "", "", false
	}
	client, ok := s.clients.Load(instanceID)
	if !ok {
		return "", "", false
	}
	if client.Store == nil || client.Store.LIDs == nil {
		return "", "", false
	}
	lidJID, err := types.ParseJID(lid)
	if err != nil {
		return "", "", false
	}
	if lidJID.Server != types.HiddenUserServer {
		// Ensure we have LID server for GetPNForLID
		lidJID.Server = types.HiddenUserServer
	}
	pnJID, err := client.Store.LIDs.GetPNForLID(ctx, lidJID)
	if err != nil || pnJID.IsEmpty() {
		return "", "", false
	}
	remoteJid = pnJID.ToNonAD().String()
	remoteLid = lid
	s.StoreChatKey(instanceID, lid, remoteJid, remoteLid)
	return remoteJid, remoteLid, true
}

// ResolveLidFromStore resolves remoteLid from the client's LID store when we have remoteJid (PN) but no lid.
// Uses GetLIDForPN. On success, stores the result in chatKeyCache.
func (s *Whatsmiau) ResolveLidFromStore(ctx context.Context, instanceID, remoteJid string) (remoteLid string, ok bool) {
	if remoteJid == "" || strings.HasSuffix(remoteJid, "@lid") {
		return "", false
	}
	client, ok := s.clients.Load(instanceID)
	if !ok {
		return "", false
	}
	if client.Store == nil || client.Store.LIDs == nil {
		return "", false
	}
	pnJID, err := types.ParseJID(remoteJid)
	if err != nil {
		return "", false
	}
	if pnJID.Server != types.DefaultUserServer {
		pnJID.Server = types.DefaultUserServer
	}
	lidJID, err := client.Store.LIDs.GetLIDForPN(ctx, pnJID)
	if err != nil || lidJID.IsEmpty() {
		return "", false
	}
	remoteLid = lidJID.ToNonAD().String()
	s.StoreChatKey(instanceID, remoteLid, remoteJid, remoteLid)
	return remoteLid, true
}
