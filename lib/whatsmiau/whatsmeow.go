package whatsmiau

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/purpshell/meowcaller"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/verbeux-ai/whatsmiau/env"
	"github.com/verbeux-ai/whatsmiau/interfaces"
	filestorage "github.com/verbeux-ai/whatsmiau/lib/storage"
	"github.com/verbeux-ai/whatsmiau/models"
	"github.com/verbeux-ai/whatsmiau/repositories/instances"
	"github.com/verbeux-ai/whatsmiau/services"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waWa6"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.uber.org/zap"
	"golang.org/x/net/context"
	"google.golang.org/protobuf/proto"
)

// ChatKeyCache stores remoteJid and remoteLid for a chat, so events that arrive only with LID can be resolved to full key data.
type ChatKeyCache struct {
	RemoteJid string
	RemoteLid string
}

// instanceCacheTTL is how long an instance read from Redis stays usable in memory.
const instanceCacheTTL = 10 * time.Second

// CachedInstance is an instance plus its own expiry, so lookups can age out entries inline.
type CachedInstance struct {
	Instance  models.Instance
	ExpiresAt time.Time
}

type Whatsmiau struct {
	clients            *xsync.Map[string, *whatsmeow.Client]
	callClients        *xsync.Map[string, *meowcaller.Client]
	callBridges        *xsync.Map[string, *callBridge]
	container          *sqlstore.Container
	logger             waLog.Logger
	repo               interfaces.InstanceRepository
	qrCache            *xsync.Map[string, string]
	pairingCache       *xsync.Map[string, string]
	pairingErrorCache  *xsync.Map[string, string]
	observerRunning    *xsync.Map[string, *whatsmeow.Client]
	instanceCache      *xsync.Map[string, CachedInstance]
	lockConnection     *xsync.Map[string, *sync.Mutex]
	connectPhoneNumber *xsync.Map[string, string]
	emitter            chan emitter
	httpClient         *http.Client
	mediaHTTPClient    *http.Client // longer timeout for document/image downloads (e.g. large PDFs)
	webhookClient      *http.Client
	linkPreviewClient  *http.Client
	fileStorage        interfaces.Storage
	handlerSemaphore   chan struct{}
	pendingSyncs       *xsync.Map[string, *pendingSyncWaiter]
	syncLocks          *xsync.Map[string, *sync.Mutex]
	// chatKeyCache: key "instanceID:lid" -> ChatKeyCache, so we can resolve LID to remoteJid when an event has no remoteJid.
	chatKeyCache *xsync.Map[string, ChatKeyCache]
	// outdatedRecovery guards the 405 ClientOutdated recovery so only one runs at a time.
	outdatedRecovery atomic.Bool
}

var instance *Whatsmiau
var mu = &sync.Mutex{}

const loadMiauConnectConcurrency = 10

var ErrAwaitingConnection = errors.New("saved session connection is pending")

func newWhatsmeowClient(device *store.Device, logger waLog.Logger) *whatsmeow.Client {
	client := whatsmeow.NewClient(device, logger)
	// A temporary dial failure must not strand an already-paired session.
	// Fresh QR/pairing attempts retain their existing timeout and cleanup flow.
	client.InitialAutoReconnect = device.ID != nil && !device.Deleted
	// Only auto-download history sync blobs when enabled; the emitter turns them into
	// webhook events (see handleHistorySyncEvent). Otherwise notifications are ignored.
	client.ManualHistorySyncDownload = !env.Env.HistorySyncEnabled
	// Ask the primary phone to re-send messages that arrive undecryptable/unavailable
	// (broken Signal session): the recovered message is dispatched as a normal Message event.
	client.AutomaticMessageRerequestFromPhone = true
	return client
}

func runLoadMiauTasks[T any](items []T, task func(T)) {
	semaphore := make(chan struct{}, loadMiauConnectConcurrency)
	var wg sync.WaitGroup

	for _, item := range items {
		semaphore <- struct{}{}
		wg.Add(1)
		go func(item T) {
			defer wg.Done()
			defer func() { <-semaphore }()
			task(item)
		}(item)
	}

	wg.Wait()
}

func Get() *Whatsmiau {
	mu.Lock()
	defer mu.Unlock()
	return instance
}

func (s *Whatsmiau) storeClient(id string, client *whatsmeow.Client) {
	s.registerCallClient(id, client)
	s.clients.Store(id, client)
}

func (s *Whatsmiau) deleteClient(id string) {
	s.removeCallClient(id)
	s.clients.Delete(id)
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
	callClients := xsync.NewMap[string, *meowcaller.Client]()
	callBridges := xsync.NewMap[string, *callBridge]()

	// Fetch the current WA web version before connecting: a stale baked-in version
	// makes WhatsApp reject every handshake with 405 ClientOutdated.
	if _, err := RefreshWAVersion(ctx); err != nil {
		zap.L().Warn("failed to fetch latest WA version, starting with built-in version",
			zap.Error(err), zap.String("version", store.GetWAVersion().String()))
	}

	clientLog := waLog.Stdout("Client", level, false)
	clientsToConnect := make([]*whatsmeow.Client, 0, len(deviceStore))
	for _, device := range deviceStore {
		client := newWhatsmeowClient(device, clientLog)
		if client.Store.ID == nil {
			zap.L().Error("device without id on db", zap.Any("device", device))
			continue
		}

		instanceFound, ok := instanceByRemoteJid[client.Store.ID.String()]
		if ok {
			configProxy(client, instanceFound.InstanceProxy)
			if env.Env.CallsEnabled {
				callClients.Store(instanceFound.ID, newCallClient(client))
			}
			clients.Store(instanceFound.ID, client)
			clientsToConnect = append(clientsToConnect, client)
			continue
		}

		if err := client.Logout(context.TODO()); err != nil {
			jid := ""
			if client.Store != nil && client.Store.ID != nil {
				jid = client.Store.ID.String()
			}
			zap.L().Error("failed to logout", zap.Error(err), zap.String("jid", jid))
		}
		client.Disconnect()
		if client.Store != nil && client.Store.ID != nil {
			if err := client.Store.Delete(context.Background()); err != nil {
				zap.L().Error("failed to delete device", zap.Error(err))
			}
		}
	}

	runLoadMiauTasks(clientsToConnect, func(client *whatsmeow.Client) {
		if err := client.Connect(); err != nil {
			jid := ""
			if client.Store != nil && client.Store.ID != nil {
				jid = client.Store.ID.String()
			}
			zap.L().Error("failed to connect connected device", zap.Error(err), zap.String("jid", jid))
		}
	})

	storage, err := filestorage.New(ctx, env.Env)
	if err != nil {
		zap.L().Panic("failed to create media storage", zap.Error(err))
	}

	instance = &Whatsmiau{
		clients:            clients,
		callClients:        callClients,
		callBridges:        callBridges,
		container:          container,
		logger:             clientLog,
		repo:               repo,
		qrCache:            xsync.NewMap[string, string](),
		pairingCache:       xsync.NewMap[string, string](),
		pairingErrorCache:  xsync.NewMap[string, string](),
		instanceCache:      xsync.NewMap[string, CachedInstance](),
		observerRunning:    xsync.NewMap[string, *whatsmeow.Client](),
		lockConnection:     xsync.NewMap[string, *sync.Mutex](),
		connectPhoneNumber: xsync.NewMap[string, string](),
		emitter:            make(chan emitter, env.Env.EmitterBufferSize),
		httpClient:         newMediaHTTPClient(),
		mediaHTTPClient:    newLongMediaHTTPClient(),
		webhookClient: &http.Client{
			Timeout:       time.Second * 30,
			CheckRedirect: rejectWebhookRedirect,
		},
		linkPreviewClient: newLinkPreviewHTTPClient(),
		fileStorage:       storage,
		handlerSemaphore:  make(chan struct{}, env.Env.HandlerSemaphoreSize),
		pendingSyncs:      xsync.NewMap[string, *pendingSyncWaiter](),
		syncLocks:         xsync.NewMap[string, *sync.Mutex](),
		chatKeyCache:      xsync.NewMap[string, ChatKeyCache](),
	}

	go instance.startEmitter()
	go instance.runStaleInstancesCleanup()
	go instance.runWAVersionRefresher()

	clients.Range(func(id string, client *whatsmeow.Client) bool {
		// Store.ID can become nil between Connect() above and this Range when the
		// server sends a logout/conflict event during the async connect.
		jid := ""
		if client.Store != nil && client.Store.ID != nil {
			jid = client.Store.ID.String()
		}
		zap.L().Info("starting event handler", zap.String("id", id), zap.String("jid", jid))
		client.AddEventHandler(instance.handleClient(id, client))
		if callClient, ok := callClients.Load(id); ok {
			instance.attachIncomingCallHandler(id, callClient)
		}
		return true
	})

}

// OwnedInstanceIDs returns the IDs of the instances this process holds a WhatsApp client for.
// LoadMiau connects every device in the store, so this is the full set this task serves.
func (s *Whatsmiau) OwnedInstanceIDs() []string {
	ids := make([]string, 0, s.clients.Size())
	s.clients.Range(func(id string, _ *whatsmeow.Client) bool {
		ids = append(ids, id)
		return true
	})

	return ids
}

func (s *Whatsmiau) Connect(ctx context.Context, id string, phoneNumber string) (qrCode string, pairingCode string, pairingError string, err error) {
	// Tear down any in-flight QR/pair attempt when the client toggles between
	// QR-only and pairing-code mode. Once whatsmeow's QR channel is open, the
	// underlying session is "in QR mode"; calling PairPhone afterwards still
	// returns a code but WhatsApp later rejects it with "não foi possível
	// conectar". Starting fresh guarantees the session matches the method the
	// user is about to use.
	s.resetIfConnectMethodChanged(ctx, id, phoneNumber)

	client, err := s.generateClient(ctx, id)
	if err != nil {
		return "", "", "", err
	}
	if client == nil {
		return "", "", "", nil
	}

	if qr, ok := s.qrCache.Load(id); ok {
		pc, _ := s.pairingCache.Load(id)
		// A QR is already cached but the pairing code may be missing. This
		// happens when the observer was first started without a phoneNumber
		// (QR-only attempt) or when it is still mid-flight on PairPhone.
		// Request it explicitly so the caller does not race against an earlier
		// observer goroutine.
		// The failure is only reported to callers that asked for a code: a
		// QR-only poll must not carry an error about a pairing it never
		// requested.
		pairingError := ""
		if phoneNumber != "" {
			if pc == "" {
				pc = s.ensurePairingCode(ctx, id, client, phoneNumber)
			}
			pairingError = s.pairingFailure(id)
		}
		return qr, pc, pairingError, nil
	}

	return s.observeAndQrCode(ctx, id, client, phoneNumber)
}

// resetIfConnectMethodChanged destroys an in-progress, not-yet-logged-in client
// when the caller switches between QR-only and pairing-code methods. whatsmeow's
// QR channel opens on the first connect and "locks" the session into QR mode —
// calling PairPhone later still returns a code but WhatsApp refuses the typed
// code. The only reliable way to switch methods is to discard the client and
// start over.
func (s *Whatsmiau) resetIfConnectMethodChanged(ctx context.Context, id, phoneNumber string) {
	lock, _ := s.lockConnection.LoadOrStore(id, &sync.Mutex{})
	lock.Lock()
	defer lock.Unlock()

	previous, hadPrevious := s.connectPhoneNumber.Load(id)
	s.connectPhoneNumber.Store(id, phoneNumber)

	if !hadPrevious || previous == phoneNumber {
		return
	}

	client, ok := s.clients.Load(id)
	if !ok {
		return
	}
	if client.IsLoggedIn() {
		return
	}

	zap.L().Info("reset pending connection due to connect method change",
		zap.String("id", id),
		zap.Bool("previous_had_number", previous != ""),
		zap.Bool("current_has_number", phoneNumber != ""),
	)

	// Closing the client also closes its QR channel, which unblocks the
	// observer goroutine and lets its deferred cleanup run.
	client.Disconnect()
	if err := s.deleteDeviceIfExists(ctx, client); err != nil {
		zap.L().Error("failed to delete device on method change", zap.String("id", id), zap.Error(err))
	}
	s.clients.Delete(id)
	s.qrCache.Delete(id)
	s.pairingCache.Delete(id)
	s.pairingErrorCache.Delete(id)
	s.observerRunning.Delete(id)
}

// ensurePairingCode returns the cached pairing code, or requests a fresh one
// from WhatsApp and stores it. Safe to call concurrently with the observer
// goroutine — the per-instance lock plus the cache double-check prevent
// duplicate PairPhone RPCs.
func (s *Whatsmiau) ensurePairingCode(ctx context.Context, id string, client *whatsmeow.Client, phoneNumber string) string {
	lock, _ := s.lockConnection.LoadOrStore(id, &sync.Mutex{})
	lock.Lock()
	defer lock.Unlock()

	if pc, ok := s.pairingCache.Load(id); ok && pc != "" {
		return pc
	}

	code, err := client.PairPhone(ctx, phoneNumber, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if !s.attemptAlive(id) {
		zap.L().Debug("discarding pairing result for a torn down attempt", zap.String("id", id))
		return ""
	}
	if err != nil {
		zap.L().Error("failed to request pairing code", zap.String("id", id), zap.Error(err))
		s.pairingErrorCache.Store(id, publicPairingError(err))
		return ""
	}
	s.pairingErrorCache.Delete(id)
	s.pairingCache.Store(id, code)
	return code
}

func (s *Whatsmiau) attemptAlive(id string) bool {
	_, ok := s.qrCache.Load(id)
	return ok
}

func (s *Whatsmiau) pairingFailure(id string) string {
	if code, ok := s.pairingCache.Load(id); ok && code != "" {
		return ""
	}
	reason, _ := s.pairingErrorCache.Load(id)
	return reason
}

func publicPairingError(err error) string {
	if errors.Is(err, whatsmeow.ErrPhoneNumberTooShort) || errors.Is(err, whatsmeow.ErrPhoneNumberIsNotInternational) {
		return "invalid_number"
	}
	return "unavailable"
}

var devicePropsMu sync.Mutex

func setHistorySyncPayload(client *whatsmeow.Client) {
	if client.Store == nil {
		return
	}

	customProps := proto.Clone(store.DeviceProps).(*waCompanionReg.DeviceProps)
	customProps.RequireFullSync = proto.Bool(true)
	if customProps.HistorySyncConfig == nil {
		customProps.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{}
	}
	customProps.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(3650)
	customProps.HistorySyncConfig.FullSyncSizeMbLimit = proto.Uint32(51200)
	customProps.HistorySyncConfig.StorageQuotaMb = proto.Uint32(51200)

	client.GetClientPayload = func() *waWa6.ClientPayload {
		devicePropsMu.Lock()
		defer devicePropsMu.Unlock()

		oldProps := store.DeviceProps
		store.DeviceProps = customProps
		payload := client.Store.GetClientPayload()
		store.DeviceProps = oldProps

		return payload
	}
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
		instanceList, err := s.repo.List(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(instanceList) == 0 {
			return nil, instances.ErrorNotFound
		}

		device := s.container.NewDevice()

		client = newWhatsmeowClient(device, s.logger)

		if inst := s.getInstanceCached(id); inst != nil && inst.SyncFullHistory {
			setHistorySyncPayload(client)
		}

		s.storeClient(id, client)
	}

	// trying recover existent connection
	if s.hasSomeDevice(client) {
		if instanceFound := s.getInstanceCached(id); instanceFound != nil {
			configProxy(client, instanceFound.InstanceProxy)
		}

		if client.IsConnected() && client.IsLoggedIn() {
			return nil, nil
		}

		// Paired clients connect during startup, restart or login. Whatsmeow
		// owns their retry loop; polling must neither start another loop nor
		// replace valid keys while authentication is still pending.
		return nil, ErrAwaitingConnection
	}

	return client, nil
}

// reconcileInMemoryClientWithRepo removes a cached client whose paired JID disagrees with instance metadata
// in Redis (or Redis has no RemoteJID while memory still has a device id). Skips while observeConnection
// is still running for that same client to avoid racing a pairing that has not written RemoteJID yet.
func (s *Whatsmiau) reconcileInMemoryClientWithRepo(ctx context.Context, id string) {
	if s.repo == nil || s.observerRunning == nil {
		return
	}
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
			s.pairingErrorCache.Delete(id)
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
			// Only tear down when the registry still points at OUR client. The panel polls
			// /connect every ~2s while the QR screen is open, and every poll that gets a
			// freshly built client starts another observer with its own 2-minute deadline.
			// Those stale observers fire long after someone actually scanned, and deleting
			// the entry unconditionally evicted the healthy, just-paired client: its socket
			// stayed alive and kept firing events (so incoming messages still reached the
			// webhook and users got created), while every send answered "client is nil".
			if current, ok := s.clients.Load(id); !ok || current != client {
				zap.L().Debug("QR expired for a superseded client, leaving the live one alone",
					zap.String("id", id))
				return
			}
			// The device may have paired through another path (e.g. the 515 restart) after
			// this observer armed its deadline. Expiring a QR nobody needed is not a reason
			// to hard logout a working session.
			if client.IsLoggedIn() {
				zap.L().Debug("QR expired but device is already paired, keeping session",
					zap.String("id", id))
				return
			}
			if err := s.deleteDeviceIfExists(context.TODO(), client); err != nil {
				zap.L().Error("failed to hard logout", zap.String("id", id), zap.Error(err))
			}
			// Compare-and-delete: another goroutine may have replaced the client while we
			// were logging out, and that replacement must survive.
			deleted := false
			s.clients.Compute(id, func(existing *whatsmeow.Client, loaded bool) (*whatsmeow.Client, xsync.ComputeOp) {
				if !loaded || existing != client {
					return existing, xsync.CancelOp
				}
				deleted = true
				return existing, xsync.DeleteOp
			})
			if deleted {
				s.removeCallClient(id)
			}
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
					// Route through ensurePairingCode so that a concurrent
					// fast-path caller (Connect() after the QR is cached)
					// cannot trigger a second PairPhone RPC. WhatsApp
					// invalidates older codes when a new one is issued, so
					// duplicate requests lead to the frontend showing a
					// stale code that WhatsApp rejects with "could not
					// connect".
					s.ensurePairingCode(ctx, id, client, phoneNumber)
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
				client.AddEventHandler(s.handleClient(id, client))
				if _, err := s.repo.Update(context.Background(), id, &models.Instance{
					RemoteJID: client.Store.ID.String(),
				}); err != nil {
					zap.L().Error("failed to update instance after login", zap.Error(err))
				}
				s.qrCache.Delete(id)
				s.pairingCache.Delete(id)
				s.pairingErrorCache.Delete(id)
				return
			}

			zap.L().Error("unknown event", zap.String("id", id), zap.Any("evt", evt))
		}
	}
}

const qrWaitTimeout = 15 * time.Second
const pairingWaitTimeout = 10 * time.Second

func waitForCachedValue(ctx context.Context, cache, abort *xsync.Map[string, string], key string, timeout time.Duration) string {
	if value, ok := cache.Load(key); ok && value != "" {
		return value
	}
	if cacheHasValue(abort, key) {
		return ""
	}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			if value, ok := cache.Load(key); ok && value != "" {
				return value
			}
			if cacheHasValue(abort, key) {
				return ""
			}
		case <-timer.C:
			value, _ := cache.Load(key)
			return value
		case <-ctx.Done():
			value, _ := cache.Load(key)
			return value
		}
	}
}

func cacheHasValue(cache *xsync.Map[string, string], key string) bool {
	if cache == nil {
		return false
	}
	_, ok := cache.Load(key)
	return ok
}

func (s *Whatsmiau) observeAndQrCode(ctx context.Context, id string, client *whatsmeow.Client, phoneNumber string) (string, string, string, error) {
	zap.L().Debug("starting observe and qr code", zap.String("id", id))
	go s.observeConnection(client, id, phoneNumber)

	qrCode := waitForCachedValue(ctx, s.qrCache, nil, id, qrWaitTimeout)
	if qrCode == "" && client.IsLoggedIn() {
		// Device paired in observeConnection while we waited: report "already connected".
		zap.L().Debug("client logged in while waiting for QR, returning without QR", zap.String("id", id))
		return "", "", "", nil
	}
	if qrCode == "" {
		zap.L().Debug("no qr code generated within budget", zap.String("id", id))
		return "", "", "", ErrAwaitingQR
	}
	if phoneNumber == "" {
		return qrCode, "", "", nil
	}

	pairingCode := waitForCachedValue(ctx, s.pairingCache, s.pairingErrorCache, id, pairingWaitTimeout)
	if pairingCode == "" {
		zap.L().Debug("pairing code unavailable", zap.String("id", id), zap.String("reason", s.pairingFailure(id)))
	}
	return qrCode, pairingCode, s.pairingFailure(id), nil
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

	client.Disconnect()
	if client.Store != nil && client.Store.ID != nil {
		if err := client.Store.Delete(ctx); err != nil {
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
	lock, _ := s.lockConnection.LoadOrStore(id, &sync.Mutex{})
	lock.Lock()
	defer lock.Unlock()

	return s.logoutUnlocked(ctx, id)
}

func (s *Whatsmiau) logoutUnlocked(ctx context.Context, id string) error {
	defer s.clearInstanceRuntimeState(id)

	client, ok := s.clients.Load(id)
	if !ok {
		zap.L().Warn("logout: client does not exist", zap.String("id", id))
		return nil
	}

	s.deleteClient(id)
	if err := s.deleteDeviceIfExists(ctx, client); err != nil {
		return err
	}
	return nil
}

// Delete serializes runtime cleanup and Redis deletion with Connect/Restart.
// Together with conditional Redis updates, this prevents late goroutines from
// recreating an instance key after deletion.
func (s *Whatsmiau) Delete(ctx context.Context, id string) error {
	lock, _ := s.lockConnection.LoadOrStore(id, &sync.Mutex{})
	lock.Lock()
	defer lock.Unlock()

	if err := s.logoutUnlocked(ctx, id); err != nil {
		return err
	}
	return s.repo.Delete(ctx, id)
}

func (s *Whatsmiau) clearInstanceRuntimeState(id string) {
	s.qrCache.Delete(id)
	s.pairingCache.Delete(id)
	s.pairingErrorCache.Delete(id)
	s.observerRunning.Delete(id)
	s.instanceCache.Delete(id)
	s.connectPhoneNumber.Delete(id)
}

// Disconnect tears down the instance completely: device store, Redis, route and webhook.
// The number is removed as if it had never existed.
func (s *Whatsmiau) Disconnect(id string) error {
	s.TeardownInstance(id)
	return nil
}

// DisconnectClient closes only the WebSocket for an instance without deleting session data.
// whatsmeow will attempt automatic reconnection. Use only when the socket itself is suspect,
// never as a reaction to a WhatsApp-side send rejection such as error 463: dropping the socket
// aborts the in-flight privacy-token issuance and makes the next send fail the same way.
// The Disconnected event fires the connection.update:close webhook.
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

func (s *Whatsmiau) InvalidateInstanceCache(id string) {
	s.instanceCache.Delete(id)
}

func (s *Whatsmiau) ApplyPresence(id string, online bool) {
	client, ok := s.clients.Load(id)
	if !ok || !client.IsConnected() {
		return
	}

	state := types.PresenceUnavailable
	if online {
		state = types.PresenceAvailable
	}
	if err := client.SendPresence(context.Background(), state); err != nil {
		zap.L().Error("failed to send presence", zap.String("instance", id), zap.Error(err))
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

func (s *Whatsmiau) Restart(ctx context.Context, id string) error {
	lock, _ := s.lockConnection.LoadOrStore(id, &sync.Mutex{})
	lock.Lock()
	defer lock.Unlock()

	// Load fresh instance from Redis BEFORE destructive ops
	ctxInst, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	instances, err := s.repo.List(ctxInst, id)
	if err != nil {
		return fmt.Errorf("failed to load instance %s: %w", id, err)
	}
	if len(instances) == 0 {
		return fmt.Errorf("instance %s not found in redis", id)
	}
	instance := &instances[0]

	// Clean up existing client
	oldClient, hadClient := s.clients.Load(id)
	if hadClient {
		oldClient.RemoveEventHandlers()
		oldClient.Disconnect()
		s.deleteClient(id)
	}

	// Clear caches
	s.qrCache.Delete(id)
	s.pairingCache.Delete(id)
	s.pairingErrorCache.Delete(id)
	s.observerRunning.Delete(id)
	s.instanceCache.Delete(id)

	// Find device: old client's Store, or scan SQL store
	var device *store.Device
	if hadClient && oldClient.Store != nil && oldClient.Store.ID != nil {
		device = oldClient.Store
	} else if instance.RemoteJID != "" {
		jid, parseErr := types.ParseJID(instance.RemoteJID)
		if parseErr != nil {
			return fmt.Errorf("failed to parse RemoteJID %s: %w", instance.RemoteJID, parseErr)
		}
		device, err = s.container.GetDevice(ctx, jid)
		if err != nil {
			return fmt.Errorf("failed to get device for %s: %w", instance.RemoteJID, err)
		}
	}

	if device == nil {
		return fmt.Errorf("instance %s has no saved session — connect via QR code first", id)
	}

	client := newWhatsmeowClient(device, s.logger)
	configProxy(client, instance.InstanceProxy)
	client.AddEventHandler(s.handleClient(id, client))
	s.storeClient(id, client)

	if err := client.Connect(); err != nil {
		zap.L().Error("restart: connect failed", zap.String("id", id), zap.Error(err))
		return err
	}

	zap.L().Info("restart: instance restarted successfully", zap.String("id", id))
	return nil
}
