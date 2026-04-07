package whatsmiau

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/google/uuid"
	"github.com/verbeux-ai/whatsmiau/env"
	"github.com/verbeux-ai/whatsmiau/models"
	"github.com/verbeux-ai/whatsmiau/repositories/instances"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
	"golang.org/x/net/context"
)

type emitter struct {
	url  string
	data any
}

func (s *Whatsmiau) getInstance(id string) *models.Instance {
	ctx, c := context.WithTimeout(context.Background(), time.Second*5)
	defer c()

	res, err := s.repo.List(ctx, id)
	if err != nil {
		zap.L().Panic("failed to get instanceCached by instance", zap.Error(err))
	}

	if len(res) == 0 {
		zap.L().Warn("no instanceCached found by instance", zap.String("instance", id))
		return nil
	}

	return &res[0]
}

func (s *Whatsmiau) getInstanceCached(id string) *models.Instance {
	instanceCached, ok := s.instanceCache.Load(id)
	if ok {
		return &instanceCached
	}

	ctx, c := context.WithTimeout(context.Background(), time.Second*5)
	defer c()

	res, err := s.repo.List(ctx, id)
	if err != nil {
		zap.L().Panic("failed to get instanceCached by instance", zap.Error(err))
	}

	if len(res) == 0 {
		zap.L().Debug("no instance in Redis for id (expected after delete/logout)", zap.String("instance", id))
		return nil
	}

	s.instanceCache.Store(id, res[0])
	go func() {
		// expires in 10sec
		time.Sleep(time.Second * 10)
		s.instanceCache.Delete(id)
	}()

	return &res[0]
}

func (s *Whatsmiau) startEmitter() {
	workers := env.Env.EmitterWorkers
	if workers <= 0 {
		workers = 50
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for event := range s.emitter {
				s.processEmit(event)
			}
		}()
	}
	wg.Wait()
}

func (s *Whatsmiau) processEmit(event emitter) {
	data, err := json.Marshal(event.data)
	if err != nil {
		zap.L().Error("failed to marshal event", zap.Error(err))
		return
	}

	const maxRetries = 2
	backoff := time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}

		success, shouldRetry := s.doEmit(data, event.url)
		if success || !shouldRetry {
			return
		}

		if attempt < maxRetries {
			zap.L().Warn("webhook delivery failed, retrying",
				zap.String("url", event.url),
				zap.Int("attempt", attempt+1),
				zap.Int("maxRetries", maxRetries),
			)
		}
	}

	zap.L().Error("webhook delivery permanently failed after retries",
		zap.String("url", event.url),
	)
}

// doEmit performs a single webhook delivery attempt with a 10s timeout.
// Returns (success, shouldRetry).
func (s *Whatsmiau) doEmit(data []byte, url string) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		zap.L().Error("failed to create request", zap.Error(err))
		return false, false
	}

	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		zap.L().Error("failed to send webhook", zap.Error(err), zap.String("url", url))
		return false, true // network error, retry
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		zap.L().Debug("webhook POST ok", zap.String("url", url), zap.Int("status", resp.StatusCode))
		return true, false
	}

	if resp.StatusCode >= 500 {
		res, _ := io.ReadAll(resp.Body)
		zap.L().Error("webhook returned server error",
			zap.Int("status", resp.StatusCode),
			zap.String("response", string(res)),
			zap.String("url", url),
		)
		return false, true // server error, retry
	}

	// 4xx: client error, don't retry
	res, _ := io.ReadAll(resp.Body)
	zap.L().Error("webhook returned client error",
		zap.Int("status", resp.StatusCode),
		zap.String("response", string(res)),
		zap.String("url", url),
	)
	return false, false
}

func (s *Whatsmiau) emit(body any, url string) {
	if url == "" {
		return
	}
	s.emitter <- emitter{url, body}
}

// emitForInstance sends the event to the webhook and records in Redis the last time this instance sent an event (for stale cleanup).
func (s *Whatsmiau) emitForInstance(instanceID string, body any, url string) {
	if instanceID != "" {
		if redisRepo, ok := s.repo.(*instances.RedisInstance); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = redisRepo.TouchLastWebhookActivity(ctx, instanceID)
			cancel()
		}
	}
	s.emit(body, url)
}

// messageKey builds a unique key for deduplication: chatJid|fromMe|messageId.
func messageKey(chatJid string, fromMe bool, messageId string) string {
	return fmt.Sprintf("%s|%v|%s", chatJid, fromMe, messageId)
}

// shouldEmitMessageEvent returns false if we already emitted this message (dedup). Otherwise marks as emitted after emitting.
// Call this before emitting; if it returns true, caller must emit and then call markMessageEmitted.
func (s *Whatsmiau) wasMessageEmitted(ctx context.Context, instanceID, key string) bool {
	redisRepo, ok := s.repo.(*instances.RedisInstance)
	if !ok {
		return false
	}
	yes, err := redisRepo.WasMessageEmitted(ctx, instanceID, key)
	if err != nil {
		zap.L().Warn("failed to check emitted message", zap.String("instance", instanceID), zap.String("key", key), zap.Error(err))
		return false
	}
	return yes
}

func (s *Whatsmiau) markMessageEmitted(ctx context.Context, instanceID, key string) {
	redisRepo, ok := s.repo.(*instances.RedisInstance)
	if !ok {
		return
	}
	if err := redisRepo.MarkMessageEmitted(ctx, instanceID, key); err != nil {
		zap.L().Warn("failed to mark message emitted", zap.String("instance", instanceID), zap.String("key", key), zap.Error(err))
	}
}

// sessionEventPayload is the flat format for session.lost (no "data" wrapper).
type sessionEventPayload struct {
	Instance    string    `json:"instance"`
	PhoneNumber string    `json:"phoneNumber"`
	DateTime    time.Time `json:"date_time"`
	Event       Wook      `json:"event"`
}

// connectionUpdatePayload is the payload for connection.update webhook when device connects.
type connectionUpdatePayload struct {
	Instance string    `json:"instance"`
	State    string    `json:"state"`
	DateTime time.Time `json:"date_time"`
	Event    Wook      `json:"event"`
}

// getWebhookURL returns WEBHOOK_URL from env if set (ECS mode), else the instance's webhook URL.
func getWebhookURL(instance *models.Instance) string {
	if env.Env.WebhookURL != "" {
		return env.Env.WebhookURL
	}
	if instance != nil && instance.Webhook.Url != "" {
		return instance.Webhook.Url
	}
	return ""
}

// shouldEmitEvent returns true if we should send this event to the webhook: when using global WEBHOOK_URL we send all events; otherwise we respect instance.Webhook.Events.
func shouldEmitEvent(instance *models.Instance, eventName string) bool {
	if env.Env.WebhookURL != "" {
		return true
	}
	if instance == nil {
		return false
	}
	for _, e := range instance.Webhook.Events {
		if e == eventName {
			return true
		}
	}
	return false
}

// messageContentFromRaw returns the webhook message object: message (text/caption) and fileBase64, each null when not present.
func messageContentFromRaw(raw *WookMessageRaw, messageType string) WookMessageContent {
	out := WookMessageContent{}
	if raw == nil {
		return out
	}
	switch messageType {
	case "conversation":
		if raw.Conversation != "" {
			out.Message = &raw.Conversation
		}
	case "imageMessage", "audioMessage", "documentMessage", "videoMessage":
		if raw.Base64 != "" {
			out.FileBase64 = &raw.Base64
		}
		// caption for image/video/document
		var caption string
		switch {
		case raw.ImageMessage != nil:
			caption = raw.ImageMessage.Caption
		case raw.VideoMessage != nil:
			caption = raw.VideoMessage.Caption
		case raw.DocumentMessage != nil:
			caption = raw.DocumentMessage.Caption
		}
		if caption != "" {
			out.Message = &caption
		}
	default:
		// reaction, contact, list, etc.: both null
	}
	return out
}

// participantToPhoneNumber returns the part of participant before "@" (e.g. "5493515830572@s.whatsapp.net" -> "5493515830572").
func participantToPhoneNumber(participant string) string {
	if i := strings.Index(participant, "@"); i != -1 {
		return participant[:i]
	}
	return participant
}

// fillKeyFromCacheOrStore resolves key missing fields: first from per-instance cache, then from the client's LID store.
// When the key has only LID or empty RemoteJid we resolve via cache then store (GetPNForLID).
// When the key has RemoteJid (PN) but no RemoteLid we resolve via store (GetLIDForPN).
func (s *Whatsmiau) fillKeyFromCacheOrStore(ctx context.Context, instanceID string, key *WookKey) {
	if key == nil {
		return
	}
	// Case 1: we have LID (or RemoteJid is actually a LID) but need remoteJid
	lid := key.RemoteLid
	if lid == "" && key.RemoteJid != "" && strings.HasSuffix(key.RemoteJid, "@lid") {
		lid = key.RemoteJid
	}
	if lid != "" {
		jid, cachedLid, ok := s.ResolveChatKey(instanceID, lid)
		if !ok {
			jid, cachedLid, ok = s.ResolveChatKeyFromStore(ctx, instanceID, lid)
		}
		if ok {
			key.RemoteJid = jid
			key.RemoteLid = cachedLid
		}
		return
	}
	// Case 2: we have remoteJid (PN) but no lid — resolve LID from store
	if key.RemoteJid != "" && key.RemoteLid == "" && !strings.HasSuffix(key.RemoteJid, "@lid") {
		if lid, ok := s.ResolveLidFromStore(ctx, instanceID, key.RemoteJid); ok {
			key.RemoteLid = lid
		}
	}
}

// EmitMessageSent sends a MESSAGES_UPSERT event to the webhook when a message is sent via the API.
// Sent when WEBHOOK_URL is set or when the instance has webhook URL and MESSAGES_UPSERT in webhook.events.
// When instance is nil, only emits if WEBHOOK_URL env is set (so message-sent events are never dropped when using global webhook).
// Dedup: we only emit if this message id was not already emitted for this instance.
func (s *Whatsmiau) EmitMessageSent(instance *models.Instance, instanceID, remoteJID, messageID string, timestamp time.Time, messageType string, raw *WookMessageRaw, participant string) {
	if messageType == "unknown" {
		return
	}
	url := getWebhookURL(instance)
	if url == "" {
		return
	}
	if instance != nil && !shouldEmitEvent(instance, "MESSAGES_UPSERT") {
		return
	}
	// Dedup: skip if we already emitted this sent message.
	msgKey := messageKey(remoteJID, true, messageID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if s.wasMessageEmitted(ctx, instanceID, msgKey) {
		cancel()
		zap.L().Debug("skipping duplicate message sent event", zap.String("instance", instanceID), zap.String("messageId", messageID))
		return
	}
	cancel()

	if raw != nil && raw.Base64 != "" {
		raw.Filebase64 = &raw.Base64
	}
	phoneNumber := participantToPhoneNumber(remoteJID)
	if phoneNumber == "" {
		phoneNumber = participantToPhoneNumber(participant)
	}
	payload := &WookMessageUpsertPayload{
		Instance:    instanceID,
		PhoneNumber: phoneNumber,
		FromMe:      true,
		Message:     messageContentFromRaw(raw, messageType),
		DateTime:    timestamp,
		Event:       WookMessagesUpsert,
	}
	zap.L().Debug("emitting message sent to webhook", zap.String("instance", instanceID), zap.String("event", string(WookMessagesUpsert)))
	s.emitForInstance(instanceID, payload, url)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	s.markMessageEmitted(ctx2, instanceID, msgKey)
	cancel2()
}

func (s *Whatsmiau) Handle(id string) whatsmeow.EventHandler {
	return func(evt any) {
		s.handlerSemaphore <- struct{}{}
		go func() {
			defer func() { <-s.handlerSemaphore }()
			instance := s.getInstanceCached(id)
			// LoggedOut: still teardown client/DB even if Redis row was already removed (401 cascade).
			if _, ok := evt.(*events.LoggedOut); ok {
				s.handleLoggedOut(id)
				return
			}

			// Connected can emit with nil instance when WEBHOOK_URL env is set (prod): avoids
			// "no instance found" when Redis/instance lookup fails due to timing or multi-backend.
			if instance == nil {
				switch evt.(type) {
				case *events.Connected:
					if env.Env.WebhookURL != "" {
						s.handleConnected(id, nil)
					} else {
						zap.L().Warn("no instance found for event", zap.String("instance", id))
					}
					return
				case *events.Disconnected, *events.ConnectFailure:
					// Normal after TeardownInstance removed Redis while the socket still drains.
					zap.L().Debug("ignoring connection event: instance no longer in store", zap.String("instance", id), zap.String("type", fmt.Sprintf("%T", evt)))
					return
				default:
					zap.L().Debug("no instance for event", zap.String("instance", id), zap.String("type", fmt.Sprintf("%T", evt)))
					return
				}
			}

			// Per-instance webhook toggle only applies when not using global WEBHOOK_URL.
			if env.Env.WebhookURL == "" {
				if instance.Webhook.Enabled != nil && !*instance.Webhook.Enabled {
					return
				}
			}

			eventMap := make(map[string]bool)
			for _, event := range instance.Webhook.Events {
				eventMap[event] = true
			}

			switch e := evt.(type) {
			case *events.Message:
				s.handleMessageEvent(id, instance, e, eventMap)
			case *events.Receipt:
				s.handleReceiptEvent(id, instance, e, eventMap)
			case *events.BusinessName:
				s.handleBusinessNameEvent(id, instance, e, eventMap)
			case *events.Contact:
				s.handleContactEvent(id, instance, e, eventMap)
			case *events.Picture:
				s.handlePictureEvent(id, instance, e, eventMap)
			case *events.PushName:
				s.handlePushNameEvent(id, instance, e, eventMap)
			case *events.Connected:
				s.handleConnectionUpdateEvent(id, instance, "open", 200, eventMap)
			case *events.Disconnected:
				s.handleConnectionUpdateEvent(id, instance, "close", 0, eventMap)
			case *events.ConnectFailure:
				s.handleConnectionUpdateEvent(id, instance, "close", int(e.Reason), eventMap)
			default:
				zap.L().Debug("unknown event", zap.String("type", fmt.Sprintf("%T", evt)), zap.Any("raw", evt))
			}
		}()
	}
}

func (s *Whatsmiau) handleConnected(id string, instance *models.Instance) {
	if !shouldEmitEvent(instance, "connection.update") && !shouldEmitEvent(instance, "CONNECTION_UPDATE") {
		return
	}
	url := getWebhookURL(instance)
	if url == "" {
		zap.L().Debug("skipping connection.update emit: no webhook URL", zap.String("instance", id))
		return
	}
	payload := &connectionUpdatePayload{
		Instance: id,
		State:    Connected,
		DateTime: time.Now(),
		Event:    WookConnectionUpdate,
	}
	zap.L().Info("emitting connection.update to webhook", zap.String("instance", id), zap.String("state", Connected))
	s.emitForInstance(id, payload, url)
}

func (s *Whatsmiau) handleLoggedOut(id string) {
	s.TeardownInstance(id)
}

// TeardownInstance removes the instance completely: device store, client, Redis metadata, route and notifies webhook.
// Use when a session is disconnected so that nothing remains for that number.
func (s *Whatsmiau) TeardownInstance(id string) {
	ctx := context.Background()

	// Get instance and webhook URL before deleting (so we can notify with instance's webhook if set)
	meta := s.getInstance(id)
	webhookURL := getWebhookURL(meta)

	client, ok := s.clients.Load(id)
	if ok {
		if err := s.deleteDeviceIfExists(ctx, client); err != nil {
			zap.L().Error("failed to delete device for instance (continuing Redis/route cleanup)", zap.String("instance", id), zap.Error(err))
		}
	} else if meta != nil && meta.RemoteJID != "" {
		// Client not in memory (other task / restart): still remove session rows from the local DB.
		if jid, err := types.ParseJID(meta.RemoteJID); err == nil {
			if dev, err := s.container.GetDevice(ctx, jid); err == nil && dev != nil {
				if err := s.container.DeleteDevice(ctx, dev); err != nil {
					zap.L().Warn("failed to delete device from store", zap.String("instance", id), zap.Error(err))
				}
			}
		}
	}

	s.clients.Delete(id)
	s.observerRunning.Delete(id)
	s.lockConnection.Delete(id)
	s.qrCache.Delete(id)
	s.pairingCache.Delete(id)
	s.ClearChatKeyCache(id)
	s.instanceCache.Delete(id)

	if redisRepo, ok := s.repo.(*instances.RedisInstance); ok {
		_ = redisRepo.DeleteEmittedMessagesForInstance(ctx, id)
	}

	if err := s.repo.Delete(ctx, id); err != nil {
		zap.L().Warn("failed to delete instance from Redis", zap.String("instance", id), zap.Error(err))
	}
	if redisRepo, ok := s.repo.(*instances.RedisInstance); ok {
		if err := redisRepo.DeleteRoute(ctx, id); err != nil {
			zap.L().Warn("failed to delete route from Redis", zap.String("instance", id), zap.Error(err))
		}
	}
	if webhookURL != "" {
		payload := &sessionEventPayload{
			Instance:    id,
			PhoneNumber: "",
			DateTime:    time.Now(),
			Event:       WookSessionLost,
		}
		s.emitForInstance(id, payload, webhookURL)
	}
}

func (s *Whatsmiau) handleMessageEvent(id string, instance *models.Instance, e *events.Message, eventMap map[string]bool) {
	if e.Message != nil {
		if pm := e.Message.GetProtocolMessage(); pm != nil && pm.GetType() == waE2E.ProtocolMessage_REVOKE {
			s.handleMessageDeleteEvent(id, instance, e, eventMap)
			return
		}
	}

	if !shouldEmitEvent(instance, "MESSAGES_UPSERT") {
		return
	}
	if e.Info.IsFromMe {
		return
	}
	// Never emit message events from groups or channels.
	if isGroupOrChannelJID(e.Info.Chat.String()) {
		return
	}

	if canIgnoreMessage(e) {
		return
	}

	// Dedup: only emit if we haven't already sent this message id to the webhook.
	msgKey := messageKey(e.Info.Chat.String(), e.Info.IsFromMe, e.Info.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if s.wasMessageEmitted(ctx, instance.ID, msgKey) {
		cancel()
		zap.L().Debug("skipping duplicate message event", zap.String("instance", id), zap.String("messageId", e.Info.ID))
		return
	}
	cancel()

	messageData := s.convertEventMessage(id, instance, e)
	if messageData == nil {
		zap.L().Error("failed to convert event", zap.String("id", id), zap.String("type", fmt.Sprintf("%T", e)), zap.Any("raw", e))
		return
	}

	messageData.InstanceId = instance.ID

	dateTime := e.Info.Timestamp
	if dateTime.IsZero() {
		dateTime = time.Now()
	}
	wookMessage := &WookEvent[WookMessageData]{
		Instance: instance.ID,
		Data:     messageData,
		DateTime: dateTime,
		Event:    WookMessagesUpsert,
	}

	if wookMessage.Data.Message != nil && len(wookMessage.Data.Message.Base64) > 0 {
		b64Temp := wookMessage.Data.Message.Base64
		wookMessage.Data.Message.Base64 = ""
		zap.L().Debug("message event", zap.String("instance", id), zap.Any("data", wookMessage.Data))
		wookMessage.Data.Message.Base64 = b64Temp
	} else if wookMessage.Data.Message != nil {
		zap.L().Debug("message event", zap.String("instance", id), zap.Any("data", wookMessage.Data))
	}

	url := getWebhookURL(instance)
	if url == "" {
		zap.L().Warn("skipping message webhook: no URL (set WEBHOOK_URL env or instance webhook.url)", zap.String("instance", id))
		return
	}
	zap.L().Debug("emitting message to webhook", zap.String("instance", id), zap.String("url", url))
	s.emitForInstance(instance.ID, wookMessage, url)
}

func (s *Whatsmiau) handleMessageDeleteEvent(id string, instance *models.Instance, e *events.Message, eventMap map[string]bool) {
	_ = eventMap
	if !shouldEmitEvent(instance, "MESSAGES_DELETE") {
		return
	}

	if canIgnoreGroup(e, instance) {
		return
	}

	if canIgnoreMessage(e) {
		return
	}

	pm := e.Message.GetProtocolMessage()
	pKey := pm.GetKey()
	if pKey == nil {
		return
	}

	ctx, c := context.WithTimeout(context.Background(), time.Second*5)
	defer c()

	remoteJid, _ := s.GetJidLid(ctx, id, e.Info.Chat)

	keyRemoteJid := pKey.GetRemoteJID()
	if keyRemoteJid == "" {
		keyRemoteJid = remoteJid
	}

	deleteData := &WookMessageDeleteData{
		Id:          pKey.GetID(),
		RemoteJid:   keyRemoteJid,
		FromMe:      pKey.GetFromMe(),
		Participant: pKey.GetParticipant(),
		Status:      "DELETED",
		InstanceId:  instance.ID,
	}

	wookEvent := &WookEvent[WookMessageDeleteData]{
		Instance: instance.ID,
		Data:     deleteData,
		DateTime: time.Now(),
		Event:    WookMessagesDelete,
	}

	zap.L().Debug("message delete event", zap.String("instance", id), zap.Any("data", deleteData))
	url := getWebhookURL(instance)
	if url == "" {
		return
	}
	s.emitForInstance(instance.ID, wookEvent, url)
}

func (s *Whatsmiau) convertEventReceipt(id string, evt *events.Receipt) []WookMessageUpdateData {
	var status WookMessageUpdateStatus
	switch evt.Type {
	case types.ReceiptTypeRead:
		status = MessageStatusRead
	case types.ReceiptTypeDelivered:
		status = MessageStatusDeliveryAck
	default:
		return nil
	}

	chatJid, chatLid := s.GetJidLid(context.Background(), id, evt.Chat)
	participantJid, _ := s.GetJidLid(context.Background(), id, evt.Sender)

	var result []WookMessageUpdateData
	for _, messageID := range evt.MessageIDs {
		result = append(result, WookMessageUpdateData{
			MessageId:   messageID,
			KeyId:       messageID,
			RemoteJid:   chatJid,
			RemoteLid:   chatLid,
			FromMe:      evt.IsFromMe,
			Participant: participantJid,
			Status:      status,
			InstanceId:  id,
		})
	}

	return result
}

func (s *Whatsmiau) handleReceiptEvent(id string, instance *models.Instance, e *events.Receipt, eventMap map[string]bool) {
	_ = eventMap
	if !shouldEmitEvent(instance, "MESSAGES_UPDATE") {
		return
	}

	if canIgnoreGroup(e, instance) {
		return
	}

	data := s.convertEventReceipt(id, e)
	if data == nil {
		return
	}

	url := getWebhookURL(instance)
	if url == "" {
		return
	}

	for _, event := range data {
		wookData := &WookEvent[WookMessageUpdateData]{
			Instance: instance.ID,
			Data:     &event,
			DateTime: e.Timestamp,
			Event:    WookMessagesUpdate,
		}

		s.emitForInstance(instance.ID, wookData, url)
	}
}

func (s *Whatsmiau) handleBusinessNameEvent(id string, instance *models.Instance, e *events.BusinessName, eventMap map[string]bool) {
	// contacts.upsert is not emitted to the webhook by design.
	_ = id
	_ = instance
	_ = e
	_ = eventMap
}

func (s *Whatsmiau) handleContactEvent(id string, instance *models.Instance, e *events.Contact, eventMap map[string]bool) {
	// contacts.upsert is not emitted to the webhook by design.
	_ = id
	_ = instance
	_ = e
	_ = eventMap
}

func (s *Whatsmiau) handlePictureEvent(id string, instance *models.Instance, e *events.Picture, eventMap map[string]bool) {
	// contacts.upsert is not emitted to the webhook by design.
	_ = id
	_ = instance
	_ = e
	_ = eventMap
}

func (s *Whatsmiau) handlePushNameEvent(id string, instance *models.Instance, e *events.PushName, eventMap map[string]bool) {
	// contacts.upsert is not emitted to the webhook by design.
	_ = id
	_ = instance
	_ = e
	_ = eventMap
}

func (s *Whatsmiau) handleConnectionUpdateEvent(id string, instance *models.Instance, state string, statusReason int, eventMap map[string]bool) {
	_ = eventMap
	if !shouldEmitEvent(instance, "CONNECTION_UPDATE") && !shouldEmitEvent(instance, "connection.update") {
		return
	}

	data := &WookConnectionUpdateData{
		Instance:     instance.ID,
		State:        state,
		StatusReason: statusReason,
	}

	if state == "open" {
		if client, ok := s.clients.Load(id); ok && client.Store.ID != nil {
			data.Wuid = client.Store.ID.ToNonAD().String()
			data.ProfileName = client.Store.PushName
		}
	}

	wookEvent := &WookEvent[WookConnectionUpdateData]{
		Instance: instance.ID,
		Data:     data,
		DateTime: time.Now(),
		Event:    WookConnectionUpdate,
	}

	zap.L().Debug("connection update event", zap.String("instance", id), zap.Any("data", data))
	url := getWebhookURL(instance)
	if url == "" {
		return
	}
	s.emitForInstance(instance.ID, wookEvent, url)
}

func (s *Whatsmiau) emitConnectionUpdate(id string, state string, statusReason int) {
	instance := s.getInstanceCached(id)
	if instance == nil {
		return
	}
	if env.Env.WebhookURL == "" {
		if instance.Webhook.Enabled == nil || !*instance.Webhook.Enabled {
			return
		}
	}

	eventMap := make(map[string]bool)
	for _, evt := range instance.Webhook.Events {
		eventMap[evt] = true
	}

	s.handleConnectionUpdateEvent(id, instance, state, statusReason, eventMap)
}

// parseWAMessage converts a raw waE2E.Message into our internal representation.
// It only inspects the content of the protobuf message itself –
// media upload (URL/Base64 generation) is handled later by the caller.
func (s *Whatsmiau) parseWAMessage(m *waE2E.Message) (string, *WookMessageRaw, *waE2E.ContextInfo) {
	var messageType string
	raw := &WookMessageRaw{}
	var ci *waE2E.ContextInfo

	// === Prioritize action-like messages ===
	if r := m.GetReactionMessage(); r != nil {
		messageType = "reactionMessage"
		reactionKey := &WookKey{}
		if rk := r.GetKey(); rk != nil {
			reactionKey.RemoteJid = rk.GetRemoteJID()
			if reactionKey.RemoteJid != "" && strings.HasSuffix(reactionKey.RemoteJid, "@lid") {
				reactionKey.RemoteLid = reactionKey.RemoteJid
			}
			reactionKey.FromMe = rk.GetFromMe()
			reactionKey.Id = rk.GetID()
			reactionKey.Participant = rk.GetParticipant()
		}
		raw.ReactionMessage = &ReactionMessageRaw{
			Text:              r.GetText(),
			SenderTimestampMs: i64(r.GetSenderTimestampMS()),
			Key:               reactionKey,
		}
	} else if lr := m.GetListResponseMessage(); lr != nil {
		messageType = "listResponseMessage"
		listType := lr.GetListType().String()
		var selectedRowID string
		if ssr := lr.GetSingleSelectReply(); ssr != nil {
			selectedRowID = ssr.GetSelectedRowID()
		}
		raw.ListResponseMessage = &WookListMessageRaw{
			ListType: listType,
			SingleSelectReply: &WookListMessageRawListSingleSelectReply{
				SelectedRowId: selectedRowID,
			},
		}
	} else if br := m.GetButtonsResponseMessage(); br != nil {
		messageType = "buttonsResponseMessage"
		raw.Conversation = br.GetSelectedDisplayText()
		ci = br.GetContextInfo()
	} else if img := m.GetImageMessage(); img != nil {
		messageType = "imageMessage"
		ci = img.GetContextInfo()
		raw.ImageMessage = &WookImageMessageRaw{
			Url:               img.GetURL(),
			Mimetype:          img.GetMimetype(),
			FileSha256:        b64(img.GetFileSHA256()),
			FileLength:        u64(img.GetFileLength()),
			Height:            int(img.GetHeight()),
			Width:             int(img.GetWidth()),
			Caption:           img.GetCaption(),
			MediaKey:          b64(img.GetMediaKey()),
			FileEncSha256:     b64(img.GetFileEncSHA256()),
			DirectPath:        img.GetDirectPath(),
			MediaKeyTimestamp: i64(img.GetMediaKeyTimestamp()),
			JpegThumbnail:     b64(img.GetJPEGThumbnail()),
			ViewOnce:          img.GetViewOnce(),
		}
	} else if aud := m.GetAudioMessage(); aud != nil {
		messageType = "audioMessage"
		ci = aud.GetContextInfo()
		raw.AudioMessage = &WookAudioMessageRaw{
			Url:               aud.GetURL(),
			Mimetype:          aud.GetMimetype(),
			FileSha256:        b64(aud.GetFileSHA256()),
			FileLength:        u64(aud.GetFileLength()),
			Seconds:           int(aud.GetSeconds()),
			Ptt:               aud.GetPTT(),
			MediaKey:          b64(aud.GetMediaKey()),
			FileEncSha256:     b64(aud.GetFileEncSHA256()),
			DirectPath:        aud.GetDirectPath(),
			MediaKeyTimestamp: i64(aud.GetMediaKeyTimestamp()),
			Waveform:          b64(aud.GetWaveform()),
			ViewOnce:          aud.GetViewOnce(),
		}
	} else if doc := m.GetDocumentMessage(); doc != nil {
		messageType = "documentMessage"
		ci = doc.GetContextInfo()
		raw.DocumentMessage = &WookDocumentMessageRaw{
			Url:               doc.GetURL(),
			Mimetype:          doc.GetMimetype(),
			Title:             doc.GetTitle(),
			FileSha256:        b64(doc.GetFileSHA256()),
			FileLength:        u64(doc.GetFileLength()),
			PageCount:         int(doc.GetPageCount()),
			MediaKey:          b64(doc.GetMediaKey()),
			FileName:          doc.GetFileName(),
			FileEncSha256:     b64(doc.GetFileEncSHA256()),
			DirectPath:        doc.GetDirectPath(),
			MediaKeyTimestamp: i64(doc.GetMediaKeyTimestamp()),
			ContactVcard:      doc.GetContactVcard(),
			JpegThumbnail:     b64(doc.GetJPEGThumbnail()),
			Caption:           doc.GetCaption(),
		}
	} else if video := m.GetVideoMessage(); video != nil {
		messageType = "videoMessage"
		raw.VideoMessage = &WookVideoMessageRaw{
			Url:           video.GetURL(),
			Mimetype:      video.GetMimetype(),
			Caption:       video.GetCaption(),
			FileSha256:    b64(video.GetFileSHA256()),
			FileLength:    u64(video.GetFileLength()),
			Seconds:       video.GetSeconds(),
			MediaKey:      b64(video.GetMediaKey()),
			FileEncSha256: b64(video.GetFileEncSHA256()),
			JPEGThumbnail: b64(video.GetJPEGThumbnail()),
			GIFPlayback:   video.GetGifPlayback(),
		}
		ci = video.GetContextInfo()
	} else if contact := m.GetContactMessage(); contact != nil {
		card, err := vcard.NewDecoder(strings.NewReader(contact.GetVcard())).Decode()
		if err != nil {
			zap.L().Error("decode card error", zap.Error(err))
		}

		messageType = "contactMessage"
		raw.ContactMessage = &ContactMessageRaw{
			VCard:        contact.GetVcard(),
			DisplayName:  contact.GetDisplayName(),
			DecodedVcard: card,
		}
		ci = contact.GetContextInfo()
	} else if contactArray := m.GetContactsArrayMessage(); contactArray != nil {
		messageType = "contactsArrayMessage"
		var contacts []ContactMessageRaw
		for _, contact := range contactArray.Contacts {
			card, err := vcard.NewDecoder(strings.NewReader(contact.GetVcard())).Decode()
			if err != nil {
				zap.L().Error("decode card error", zap.Error(err))
			}

			contacts = append(contacts, ContactMessageRaw{
				VCard:        contact.GetVcard(),
				DisplayName:  contact.GetDisplayName(),
				DecodedVcard: card,
			})
		}
		raw.ContactsArrayMessage = &ContactsArrayMessageRaw{
			DisplayName: contactArray.GetDisplayName(),
			Contacts:    contacts,
		}
		ci = contactArray.GetContextInfo()
	} else if conv := strings.TrimSpace(m.GetConversation()); conv != "" {
		messageType = "conversation"
		raw.Conversation = conv
	} else if et := m.GetExtendedTextMessage(); et != nil && len(et.GetText()) > 0 {
		messageType = "conversation"
		raw.Conversation = et.GetText()
		ci = et.GetContextInfo()
	} else {
		messageType = "unknown"
	}

	return messageType, raw, ci
}

func (s *Whatsmiau) convertContactHistorySync(id string, event []*waHistorySync.Pushname, conversations []*waHistorySync.Conversation) WookContactUpsertData {
	resultMap := make(map[string]WookContact)
	for _, pushName := range event {

		if len(pushName.GetPushname()) == 0 {
			continue
		}

		if dt := strings.Split(pushName.GetPushname(), "@"); len(dt) == 2 && (dt[1] == "g.us" || dt[1] == "s.whatsapp.net") {
			return nil
		}

		jid, err := types.ParseJID(pushName.GetID())
		if err != nil {
			zap.L().Error("failed to parse jid", zap.String("pushname", pushName.GetPushname()))
			return nil
		}

		jidParsed, lid := s.GetJidLid(context.Background(), id, jid)

		resultMap[jidParsed] = WookContact{
			RemoteJid:  jidParsed,
			PushName:   pushName.GetPushname(),
			InstanceId: id,
			RemoteLid:  lid,
		}
	}

	for _, conversation := range conversations {
		name := conversation.GetName()
		if len(name) == 0 {
			name = conversation.GetDisplayName()
		}
		if len(name) == 0 {
			name = conversation.GetUsername()
		}
		if len(name) == 0 {
			continue
		}
		if dt := strings.Split(name, "@"); len(dt) == 2 && (dt[1] == "g.us" || dt[1] == "s.whatsapp.net") {
			return nil
		}

		jid, err := types.ParseJID(conversation.GetID())
		if err != nil {
			zap.L().Error("failed to parse jid", zap.String("name", conversation.GetName()))
			return nil
		}
		jidParsed, lid := s.GetJidLid(context.Background(), id, jid)

		resultMap[conversation.GetID()] = WookContact{
			RemoteJid:  jidParsed,
			PushName:   name,
			InstanceId: id,
			RemoteLid:  lid,
		}
	}

	var result []WookContact
	for _, c := range resultMap {
		jid, err := types.ParseJID(c.RemoteJid)
		if err != nil {
			continue
		}

		url, b64Pic, err := s.getPic(id, jid, false)
		if err != nil {
			zap.L().Error("failed to get pic", zap.Error(err))
		}

		picUrl, err := s.uploadPic(context.Background(), jid.ToNonAD().String(), b64Pic)
		if err != nil {
			zap.L().Error("failed to upload pic", zap.Error(err))
		} else {
			url = picUrl
		}

		c.ProfilePicUrl = url
		c.Base64Pic = b64Pic
		result = append(result, c)
	}

	return result
}

func (s *Whatsmiau) convertEventMessage(id string, instance *models.Instance, evt *events.Message) *WookMessageData {
	ctx, c := context.WithTimeout(context.Background(), time.Second*60)
	defer c()

	client, ok := s.clients.Load(id)
	if !ok {
		zap.L().Warn("no client for event", zap.String("id", id))
		return nil
	}

	if evt == nil || evt.Message == nil {
		return nil
	}

	jid, lid := s.GetJidLid(ctx, id, evt.Info.Chat)
	senderJid, _ := s.GetJidLid(ctx, id, evt.Info.Sender)
	s.StoreChatKey(id, lid, jid, lid)

	// Always unwrap to work with the real content
	e := evt.UnwrapRaw()
	m := e.Message

	// Build the key
	key := &WookKey{
		RemoteJid:   jid,
		RemoteLid:   lid,
		FromMe:      e.Info.IsFromMe,
		Id:          e.Info.ID,
		Participant: senderJid,
	}

	// Convert the WA protobuf message into our internal raw structure
	messageType, raw, _ := s.parseWAMessage(m)

	// Upload media (URL / Base64) when needed; also set decoded fields inside the media object for convenience
	switch messageType {
	case "imageMessage":
		if img := m.GetImageMessage(); img != nil {
			raw.MediaURL, raw.Base64 = s.uploadMessageFile(ctx, instance, client, img, img.GetMimetype(), "")
			if raw.ImageMessage != nil {
				raw.ImageMessage.DecodedMediaUrl = raw.MediaURL
				raw.ImageMessage.DecodedBase64 = raw.Base64
			}
		}
	case "audioMessage":
		if aud := m.GetAudioMessage(); aud != nil {
			raw.MediaURL, raw.Base64 = s.uploadMessageFile(ctx, instance, client, aud, aud.GetMimetype(), "")
		}
	case "documentMessage":
		if doc := m.GetDocumentMessage(); doc != nil {
			raw.MediaURL, raw.Base64 = s.uploadMessageFile(ctx, instance, client, doc, doc.GetMimetype(), doc.GetFileName())
		}
	case "videoMessage":
		if vid := m.GetVideoMessage(); vid != nil {
			raw.MediaURL, raw.Base64 = s.uploadMessageFile(ctx, instance, client, vid, vid.GetMimetype(), "")
		}
	}

	s.fillKeyFromCacheOrStore(ctx, id, key)
	if raw != nil && raw.ReactionMessage != nil && raw.ReactionMessage.Key != nil {
		s.fillKeyFromCacheOrStore(ctx, id, raw.ReactionMessage.Key)
	}
	if raw != nil && raw.Base64 != "" {
		raw.Filebase64 = &raw.Base64
	}

	phoneNumber := participantToPhoneNumber(senderJid)
	return &WookMessageData{
		Key:         &WookMessageKey{PhoneNumber: phoneNumber},
		PushName:    strings.TrimSpace(e.Info.PushName),
		Message:     raw,
		MessageType: messageType,
		InstanceId:  id,
	}
}

func (s *Whatsmiau) uploadMessageFile(ctx context.Context, instance *models.Instance, client *whatsmeow.Client, fileMessage whatsmeow.DownloadableMessage, mimetype, fileName string) (string, string) {
	var (
		b64Result string
		urlResult string
		ext       string
	)

	tmpFile, err := os.CreateTemp("", "file-*")
	if err != nil {
		panic(err)
	}

	defer os.Remove(tmpFile.Name())
	if err := client.DownloadToFile(ctx, fileMessage, tmpFile); err != nil {
		zap.L().Error("failed to download media", zap.Error(err))
		return "", ""
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		zap.L().Error("failed to seek media", zap.Error(err))
	}

	// If received file is PDF, convert first page to PNG at 2x resolution so we avoid PDF handling issues.
	var mediaData []byte
	head := make([]byte, 8)
	if _, err := tmpFile.Read(head); err == nil {
		_, _ = tmpFile.Seek(0, io.SeekStart)
		isPDF := bytes.HasPrefix(head, []byte("%PDF")) ||
			strings.Contains(strings.ToLower(mimetype), "pdf") ||
			strings.HasSuffix(strings.ToLower(fileName), ".pdf")
		if isPDF {
			if pngBytes, err := convertPDFToPNG(tmpFile.Name()); err != nil {
				zap.L().Warn("PDF to PNG conversion failed, using original file", zap.Error(err))
			} else {
				mediaData = pngBytes
				ext = "png"
				mimetype = "image/png"
			}
		}
	} else {
		_, _ = tmpFile.Seek(0, io.SeekStart)
	}

	if len(mediaData) == 0 {
		ext = extractExtFromFile(fileName, mimetype, tmpFile)
	}

	// Include base64 when instance has webhook.base64 or when using global WEBHOOK_URL (so webhook always gets decoded media)
	includeBase64 := (instance.Webhook.Base64 != nil && *instance.Webhook.Base64) || env.Env.WebhookURL != ""
	if includeBase64 {
		if len(mediaData) > 0 {
			b64Result = base64.StdEncoding.EncodeToString(mediaData)
		} else {
			if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
				zap.L().Error("failed to seek before reading file", zap.Error(err))
			}
			data, err := io.ReadAll(tmpFile)
			if err != nil {
				zap.L().Error("failed to read media", zap.Error(err))
			} else {
				b64Result = base64.StdEncoding.EncodeToString(data)
			}
		}
	}
	if s.fileStorage != nil {
		if len(mediaData) > 0 {
			urlResult, _, err = s.fileStorage.Upload(ctx, uuid.NewString()+"."+ext, mimetype, bytes.NewReader(mediaData))
		} else {
			if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
				zap.L().Error("failed to seek media", zap.Error(err))
			}
			urlResult, _, err = s.fileStorage.Upload(ctx, uuid.NewString()+"."+ext, mimetype, tmpFile)
		}
		if err != nil {
			zap.L().Error("failed to upload media", zap.Error(err))
		}
	}

	return urlResult, b64Result
}

func (s *Whatsmiau) uploadPic(ctx context.Context, waId, b64Data string) (string, error) {
	if s.fileStorage == nil {
		return "", nil
	}

	mimetype, ext, _, err := extractFromBase64(b64Data)
	if err != nil {
		return "", err
	}

	waIdTreated := strings.Split(waId, "@")

	urlResult, err := s.fileStorage.UploadBase64IfDontExists(ctx, waIdTreated[0]+"."+ext, mimetype, b64Data)
	if err != nil {
		zap.L().Error("failed to upload image", zap.Error(err))
		return "", err
	}

	return urlResult, nil
}

func (s *Whatsmiau) convertContact(id string, evt *events.Contact) *WookContact {
	url, b64Pic, err := s.getPic(id, evt.JID, false)
	if err != nil {
		zap.L().Error("failed to get pic", zap.Error(err))
	}

	name := evt.Action.GetFirstName()
	if name == "" {
		name = evt.Action.GetFullName()
	}
	if name == "" {
		name = evt.Action.GetUsername()
	}
	if name == "" {
		return nil
	}

	if dt := strings.Split(name, "@"); len(dt) == 2 && (dt[1] == "g.us" || dt[1] == "s.whatsapp.net") {
		return nil
	}

	picUrl, err := s.uploadPic(context.Background(), evt.JID.ToNonAD().String(), b64Pic)
	if err != nil {
		zap.L().Error("failed to upload pic", zap.Error(err))
	} else {
		url = picUrl
	}

	jid, lid := s.GetJidLid(context.Background(), id, evt.JID)
	s.StoreChatKey(id, lid, jid, lid)
	return &WookContact{
		RemoteJid:     jid,
		RemoteLid:     lid,
		PushName:      name,
		ProfilePicUrl: url,
		InstanceId:    id,
		Base64Pic:     b64Pic,
	}
}

func (s *Whatsmiau) convertGroupInfo(id string, evt *events.GroupInfo) *WookContact {
	url, b64Pic, err := s.getPic(id, evt.JID, false)
	if err != nil {
		zap.L().Error("failed to get pic", zap.Error(err))
	}

	if evt.Name == nil || len(evt.Name.Name) == 0 {
		return nil
	}

	if dt := strings.Split(evt.Name.Name, "@"); len(dt) == 2 && (dt[1] == "g.us" || dt[1] == "s.whatsapp.net") {
		return nil
	}

	picUrl, err := s.uploadPic(context.Background(), evt.JID.ToNonAD().String(), b64Pic)
	if err != nil {
		zap.L().Error("failed to upload pic", zap.Error(err))
	} else {
		url = picUrl
	}

	jid, lid := s.GetJidLid(context.Background(), id, evt.JID)

	return &WookContact{
		RemoteJid:     jid,
		PushName:      evt.Name.Name,
		ProfilePicUrl: url,
		InstanceId:    id,
		RemoteLid:     lid,
		Base64Pic:     b64Pic,
	}
}

func (s *Whatsmiau) convertPushName(id string, evt *events.PushName) *WookContact {
	url, b64Pic, err := s.getPic(id, evt.JID, false)
	if err != nil {
		zap.L().Error("failed to get pic", zap.Error(err))
	}

	name := evt.NewPushName
	if len(name) == 0 {
		name = evt.OldPushName
	}

	if name == "" {
		return nil
	}

	if dt := strings.Split(name, "@"); len(dt) == 2 && (dt[1] == "g.us" || dt[1] == "s.whatsapp.net") {
		return nil
	}

	picUrl, err := s.uploadPic(context.Background(), evt.JID.ToNonAD().String(), b64Pic)
	if err != nil {
		zap.L().Error("failed to upload pic", zap.Error(err))
	} else {
		url = picUrl
	}

	jid, lid := s.GetJidLid(context.Background(), id, evt.JID)
	s.StoreChatKey(id, lid, jid, lid)
	return &WookContact{
		RemoteJid:     jid,
		PushName:      evt.NewPushName,
		InstanceId:    id,
		ProfilePicUrl: url,
		RemoteLid:     lid,
		Base64Pic:     b64Pic,
	}
}

func (s *Whatsmiau) convertPicture(id string, evt *events.Picture) *WookContact {
	url, b64Pic, err := s.getPic(id, evt.JID, false)
	if err != nil {
		zap.L().Error("failed to get pic", zap.Error(err))
	}

	if len(url) <= 0 {
		return nil
	}

	picUrl, err := s.uploadPic(context.Background(), evt.JID.ToNonAD().String(), b64Pic)
	if err != nil {
		zap.L().Error("failed to upload pic", zap.Error(err))
	} else {
		url = picUrl
	}

	jid, lid := s.GetJidLid(context.Background(), id, evt.JID)
	s.StoreChatKey(id, lid, jid, lid)
	return &WookContact{
		RemoteJid:     jid,
		InstanceId:    id,
		Base64Pic:     b64Pic,
		ProfilePicUrl: url,
		RemoteLid:     lid,
	}
}

func (s *Whatsmiau) convertBusinessName(id string, evt *events.BusinessName) *WookContact {
	url, b64Pic, err := s.getPic(id, evt.JID, false)
	if err != nil {
		zap.L().Error("failed to get pic", zap.Error(err))
	}

	name := evt.NewBusinessName
	if name == "" {
		name = evt.OldBusinessName
	}
	if name == "" && evt.Message != nil {
		name = evt.Message.PushName
	}
	if name == "" && evt.Message != nil && evt.Message.VerifiedName != nil && evt.Message.VerifiedName.Details != nil {
		name = evt.Message.VerifiedName.Details.GetVerifiedName()
	}

	if dt := strings.Split(name, "@"); len(dt) == 2 && (dt[1] == "g.us" || dt[1] == "s.whatsapp.net") {
		return nil
	}

	picUrl, err := s.uploadPic(context.Background(), evt.JID.ToNonAD().String(), b64Pic)
	if err != nil {
		zap.L().Error("failed to upload pic", zap.Error(err))
	} else {
		url = picUrl
	}

	jid, lid := s.GetJidLid(context.Background(), id, evt.JID)
	s.StoreChatKey(id, lid, jid, lid)
	return &WookContact{
		RemoteJid:     jid,
		InstanceId:    id,
		Base64Pic:     b64Pic,
		ProfilePicUrl: url,
		PushName:      name,
		RemoteLid:     lid,
	}
}

func (s *Whatsmiau) getPic(id string, jid types.JID, fetchProfilePic bool) (string, string, error) {
	if !fetchProfilePic {
		return "", "", nil
	}
	client, ok := s.clients.Load(id)
	if !ok || client == nil {
		zap.L().Warn("no client for event", zap.String("id", id))
		return "", "", fmt.Errorf("no client for event %s", id)
	}

	pic, err := client.GetProfilePictureInfo(context.TODO(), jid, &whatsmeow.GetProfilePictureParams{
		Preview:     true,
		IsCommunity: false,
	})
	if err != nil {
		return "", "", nil
	}

	if pic == nil {
		return "", "", err
	}

	res, err := s.httpClient.Get(pic.URL)
	if err != nil {
		zap.L().Error("get profile picture error", zap.String("id", id), zap.Error(err))
		return "", "", err
	}

	picRaw, err := io.ReadAll(res.Body)
	if err != nil {
		zap.L().Error("get profile picture error", zap.String("id", id), zap.Error(err))
		return "", "", err
	}

	return pic.URL, base64.StdEncoding.EncodeToString(picRaw), nil
}
