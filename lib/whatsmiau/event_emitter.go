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
	"time"

	"github.com/emersion/go-vcard"
	"github.com/google/uuid"
	"github.com/verbeux-ai/whatsmiau/env"
	"github.com/verbeux-ai/whatsmiau/models"
	"github.com/verbeux-ai/whatsmiau/repositories/instances"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
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

	s.instanceCache.Store(id, res[0])

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
		zap.L().Warn("no instanceCached found by instance", zap.String("instance", id))
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
	for event := range s.emitter {
		data, err := json.Marshal(event.data)
		if err != nil {
			zap.L().Error("failed to marshal event", zap.Error(err))
			return
		}

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, event.url, bytes.NewReader(data))
		if err != nil {
			zap.L().Error("failed to create request", zap.Error(err))
			return
		}

		req.Header.Set("Content-Type", "application/json")
		resp, err := s.httpClient.Do(req)
		if err != nil {
			zap.L().Error("failed to send request", zap.Error(err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			res, err := io.ReadAll(resp.Body)
			if err != nil {
				zap.L().Error("failed to read response body", zap.Error(err))
			} else {
				zap.L().Error("error doing request", zap.Any("response", string(res)), zap.String("url", event.url))
			}
		}
	}
}

func (s *Whatsmiau) emit(body any, url string) {
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

// emitConnectionUpdate sends a connection.update event to the webhook when the device connects (pairing success or reconnect).
// Sent when WEBHOOK_URL is set or when the instance has webhook URL and CONNECTION_UPDATE in webhook.events.
func (s *Whatsmiau) emitConnectionUpdate(instanceID, remoteJID string) {
	instance := s.getInstance(instanceID)
	if instance == nil {
		return
	}
	if !shouldEmitEvent(instance, "CONNECTION_UPDATE") {
		return
	}
	url := getWebhookURL(instance)
	if url == "" {
		return
	}
	payload := &WookEvent[struct {
		InstanceId string `json:"instanceId"`
		RemoteJID  string `json:"remoteJid"`
		State      string `json:"state"`
	}]{
		Instance: instanceID,
		Data: &struct {
			InstanceId string `json:"instanceId"`
			RemoteJID  string `json:"remoteJid"`
			State      string `json:"state"`
		}{InstanceId: instanceID, RemoteJID: remoteJID, State: "open"},
		DateTime: time.Now(),
		Event:    WookConnectionUpdate,
	}
	s.emitForInstance(instanceID, payload, url)
}

// EmitReady sends a "ready" event to the webhook when the API has finished starting.
// Only sent if WEBHOOK_URL (env) is set.
func (s *Whatsmiau) EmitReady() {
	url := getWebhookURL(nil)
	if url == "" {
		zap.L().Info("WEBHOOK_URL not set, skipping ready event")
		return
	}
	zap.L().Info("sending ready event to webhook", zap.String("url", url))
	payload := &WookEvent[struct {
		State string `json:"state"`
	}]{
		Data: &struct {
			State string `json:"state"`
		}{State: "open"},
		DateTime: time.Now(),
		Event:    WookReady,
	}
	s.emit(payload, url)
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

// EmitMessageSent sends a MESSAGES_UPSERT event to the webhook when a message is sent via the API.
// Sent when WEBHOOK_URL is set or when the instance has webhook URL and MESSAGES_UPSERT in webhook.events.
// When instance is nil, only emits if WEBHOOK_URL env is set (so message-sent events are never dropped when using global webhook).
func (s *Whatsmiau) EmitMessageSent(instance *models.Instance, instanceID, remoteJID, messageID string, timestamp time.Time, messageType string, raw *WookMessageRaw, participant string) {
	url := getWebhookURL(instance)
	if url == "" {
		return
	}
	if instance != nil && !shouldEmitEvent(instance, "MESSAGES_UPSERT") {
		return
	}
	key := &WookKey{
		RemoteJid:   remoteJID,
		FromMe:      true,
		Id:          messageID,
		Participant: participant,
	}
	payload := &WookEvent[WookMessageData]{
		Instance: instanceID,
		Data: &WookMessageData{
			Key:              key,
			Status:           "sent",
			Message:          raw,
			MessageType:      messageType,
			MessageTimestamp: int(timestamp.Unix()),
			InstanceId:       instanceID,
			Source:           "api",
		},
		DateTime: timestamp,
		Event:    WookMessagesUpsert,
	}
	zap.L().Debug("emitting message sent to webhook", zap.String("instance", instanceID), zap.String("event", string(WookMessagesUpsert)))
	s.emitForInstance(instanceID, payload, url)
}

func (s *Whatsmiau) Handle(id string) whatsmeow.EventHandler {
	return func(evt any) {
		s.handlerSemaphore <- struct{}{}
		go func() {
			defer func() { <-s.handlerSemaphore }()
			instance := s.getInstanceCached(id)
			if instance == nil {
				zap.L().Warn("no instance found for event", zap.String("instance", id))
				return
			}

			eventMap := make(map[string]bool)
			for _, event := range instance.Webhook.Events {
				eventMap[event] = true
			}

			switch e := evt.(type) {
			case *events.Connected:
				if client, ok := s.clients.Load(id); ok && client.Store != nil && client.Store.ID != nil {
					s.emitConnectionUpdate(id, client.Store.ID.String())
				}
			case *events.LoggedOut:
				s.handleLoggedOut(id)
			case *events.Disconnected:
				s.TeardownInstance(id)
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
			case *events.HistorySync:
				s.handleHistorySyncEvent(id, instance, e, eventMap)
			case *events.GroupInfo:
				s.handleGroupInfoEvent(id, instance, e, eventMap)
			case *events.PushName:
				s.handlePushNameEvent(id, instance, e, eventMap)
			default:
				zap.L().Debug("unknown event", zap.String("type", fmt.Sprintf("%T", evt)), zap.Any("raw", evt))
			}
		}()
	}
}

func (s *Whatsmiau) handleLoggedOut(id string) {
	s.TeardownInstance(id)
}

// TeardownInstance removes the instance completely: device store, client, Redis metadata, route and notifies webhook.
// Use when a session is disconnected so that nothing remains for that number.
func (s *Whatsmiau) TeardownInstance(id string) {
	ctx := context.Background()

	// Get instance and webhook URL before deleting (so we can notify with instance's webhook if set)
	instance := s.getInstance(id)
	webhookURL := getWebhookURL(instance)

	client, ok := s.clients.Load(id)
	if ok {
		if err := s.deleteDeviceIfExists(ctx, client); err != nil {
			zap.L().Error("failed to delete device for instance", zap.String("instance", id), zap.Error(err))
			return
		}
	}

	s.clients.Delete(id)
	s.qrCache.Delete(id)

	// Remove instance metadata and route so router stops sending traffic here; notify webhook
	if err := s.repo.Delete(ctx, id); err != nil {
		zap.L().Warn("failed to delete instance from Redis", zap.String("instance", id), zap.Error(err))
	}
	if redisRepo, ok := s.repo.(*instances.RedisInstance); ok {
		if err := redisRepo.DeleteRoute(ctx, id); err != nil {
			zap.L().Warn("failed to delete route from Redis", zap.String("instance", id), zap.Error(err))
		}
	}
	if webhookURL != "" {
		payload := &WookEvent[struct {
			InstanceId string `json:"instanceId"`
		}]{
			Instance: id,
			Data: &struct {
				InstanceId string `json:"instanceId"`
			}{InstanceId: id},
			DateTime: time.Now(),
			Event:    WookSessionLost,
		}
		s.emitForInstance(id, payload, webhookURL)
	}
}
func (s *Whatsmiau) handleMessageEvent(id string, instance *models.Instance, e *events.Message, eventMap map[string]bool) {
	if !shouldEmitEvent(instance, "MESSAGES_UPSERT") {
		return
	}

	if canIgnoreGroup(e, instance) {
		return
	}

	if canIgnoreMessage(e) {
		return
	}

	messageData := s.convertEventMessage(id, instance, e)
	if messageData == nil {
		zap.L().Error("failed to convert event", zap.String("id", id), zap.String("type", fmt.Sprintf("%T", e)), zap.Any("raw", e))
		return
	}

	messageData.InstanceId = instance.ID

	dateTime := time.Unix(int64(messageData.MessageTimestamp), 0)
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

	s.emitForInstance(instance.ID, wookMessage, getWebhookURL(instance))
}

func (s *Whatsmiau) handleReceiptEvent(id string, instance *models.Instance, e *events.Receipt, eventMap map[string]bool) {
	// MESSAGES_UPDATE (delivery/read receipts) is not emitted to the webhook by design.
	_ = id
	_ = instance
	_ = e
	_ = eventMap
}

func (s *Whatsmiau) handleBusinessNameEvent(id string, instance *models.Instance, e *events.BusinessName, eventMap map[string]bool) {
	if !shouldEmitEvent(instance, "CONTACTS_UPSERT") {
		return
	}

	data := s.convertBusinessName(id, e)
	if data == nil {
		zap.L().Error("failed to convert business name", zap.String("id", id), zap.String("type", fmt.Sprintf("%T", e)), zap.Any("raw", e))
		return
	}

	wookData := &WookEvent[WookContactUpsertData]{
		Instance: instance.ID,
		Data:     &WookContactUpsertData{*data},
		DateTime: time.Now(),
		Event:    WookContactsUpsert,
	}

	s.emitForInstance(instance.ID, wookData, getWebhookURL(instance))
}

func (s *Whatsmiau) handleContactEvent(id string, instance *models.Instance, e *events.Contact, eventMap map[string]bool) {
	if !shouldEmitEvent(instance, "CONTACTS_UPSERT") {
		return
	}

	if canIgnoreGroup(e, instance) {
		return
	}

	data := s.convertContact(id, e)
	if data == nil {
		zap.L().Error("failed to convert contact", zap.String("id", id), zap.String("type", fmt.Sprintf("%T", e)), zap.Any("raw", e))
		return
	}

	wookData := &WookEvent[WookContactUpsertData]{
		Instance: instance.ID,
		Data:     &WookContactUpsertData{*data},
		DateTime: time.Now(),
		Event:    WookContactsUpsert,
	}

	s.emitForInstance(instance.ID, wookData, getWebhookURL(instance))
}

func (s *Whatsmiau) handlePictureEvent(id string, instance *models.Instance, e *events.Picture, eventMap map[string]bool) {
	if !shouldEmitEvent(instance, "CONTACTS_UPSERT") {
		return
	}

	data := s.convertPicture(id, e)
	if data == nil {
		return
	}

	wookData := &WookEvent[WookContactUpsertData]{
		Instance: instance.ID,
		Data:     &WookContactUpsertData{*data},
		DateTime: e.Timestamp,
		Event:    WookContactsUpsert,
	}

	s.emitForInstance(instance.ID, wookData, getWebhookURL(instance))
}

func (s *Whatsmiau) handleHistorySyncEvent(id string, instance *models.Instance, e *events.HistorySync, eventMap map[string]bool) {
	if !shouldEmitEvent(instance, "CONTACTS_UPSERT") {
		return
	}

	data := s.convertContactHistorySync(id, e.Data.GetPushnames(), e.Data.Conversations)
	if data == nil {
		return
	}

	wookData := &WookEvent[WookContactUpsertData]{
		Instance: instance.ID,
		Data:     &data,
		DateTime: time.Now(),
		Event:    WookContactsUpsert,
	}

	s.emitForInstance(instance.ID, wookData, getWebhookURL(instance))
}

func (s *Whatsmiau) handleGroupInfoEvent(id string, instance *models.Instance, e *events.GroupInfo, eventMap map[string]bool) {
	if !shouldEmitEvent(instance, "CONTACTS_UPSERT") {
		return
	}

	if instance.GroupsIgnore {
		return
	}

	data := s.convertGroupInfo(id, e)
	if data == nil {
		zap.L().Debug("failed to convert group info", zap.String("id", id), zap.String("type", fmt.Sprintf("%T", e)), zap.Any("raw", e))
		return
	}

	wookData := &WookEvent[WookContactUpsertData]{
		Instance: instance.ID,
		Data:     &WookContactUpsertData{*data},
		DateTime: time.Now(),
		Event:    WookContactsUpsert,
	}

	s.emitForInstance(instance.ID, wookData, getWebhookURL(instance))
}

func (s *Whatsmiau) handlePushNameEvent(id string, instance *models.Instance, e *events.PushName, eventMap map[string]bool) {
	if !shouldEmitEvent(instance, "CONTACTS_UPSERT") {
		return
	}

	if canIgnoreGroup(e, instance) {
		return
	}

	data := s.convertPushName(id, e)
	if data == nil {
		zap.L().Error("failed to convert pushname", zap.String("id", id), zap.String("type", fmt.Sprintf("%T", e)), zap.Any("raw", e))
		return
	}

	wookData := &WookEvent[WookContactUpsertData]{
		Instance: instance.ID,
		Data:     &WookContactUpsertData{*data},
		DateTime: time.Now(),
		Event:    WookContactsUpsert,
	}

	s.emitForInstance(instance.ID, wookData, getWebhookURL(instance))
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

		url, _, err := s.getPic(id, jid)
		if err != nil {
			zap.L().Error("failed to get pic", zap.Error(err))
		}

		c.ProfilePicUrl = url
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

	// Determine status
	status := "received"
	if e.Info.IsFromMe {
		status = "sent"
	}

	// Timestamp
	ts := e.Info.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	// Convert the WA protobuf message into our internal raw structure
	messageType, raw, ci := s.parseWAMessage(m)

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

	// Map MessageContextInfo (quoted, mentions, disappearing mode, external ad reply)
	var messageContext WookMessageContextInfo
	if ci != nil {
		messageContext.EphemeralSettingTimestamp = i64(ci.GetEphemeralSettingTimestamp())
		messageContext.StanzaId = ci.GetStanzaID()
		messageContext.Participant = ci.GetParticipant()
		messageContext.Expiration = int(ci.GetExpiration())
		messageContext.MentionedJid = ci.GetMentionedJID()
		messageContext.ConversionSource = ci.GetConversionSource()
		messageContext.ConversionData = b64(ci.GetConversionData())
		messageContext.ConversionDelaySeconds = int(ci.GetConversionDelaySeconds())
		messageContext.EntryPointConversionSource = ci.GetEntryPointConversionSource()
		messageContext.EntryPointConversionApp = ci.GetEntryPointConversionApp()
		messageContext.EntryPointConversionDelaySeconds = int(ci.GetEntryPointConversionDelaySeconds())
		messageContext.TrustBannerAction = ci.GetTrustBannerAction()

		if dm := ci.GetDisappearingMode(); dm != nil {
			messageContext.DisappearingMode = &ContextInfoDisappearingMode{
				Initiator:     dm.GetInitiator().String(),
				Trigger:       dm.GetTrigger().String(),
				InitiatedByMe: dm.GetInitiatedByMe(),
			}
		}

		if ear := ci.GetExternalAdReply(); ear != nil {
			messageType = "conversation"
			messageContext.ExternalAdReply = &WookMessageContextInfoExternalAdReply{
				Title:                 ear.GetTitle(),
				Body:                  ear.GetBody(),
				MediaType:             ear.GetMediaType().String(),
				ThumbnailUrl:          ear.GetThumbnailURL(),
				Thumbnail:             b64(ear.GetThumbnail()),
				SourceType:            ear.GetSourceType(),
				SourceId:              ear.GetSourceID(),
				SourceUrl:             ear.GetSourceURL(),
				ContainsAutoReply:     ear.GetContainsAutoReply(),
				RenderLargerThumbnail: ear.GetRenderLargerThumbnail(),
				ShowAdAttribution:     ear.GetShowAdAttribution(),
				CtwaClid:              ear.GetCtwaClid(),
			}
		}

		if qm := ci.GetQuotedMessage(); qm != nil {
			_, qmRaw, _ := s.parseWAMessage(qm)
			messageContext.QuotedMessage = qmRaw
		}
	}

	return &WookMessageData{
		Key:              key,
		PushName:         strings.TrimSpace(e.Info.PushName),
		Status:           status,
		Message:          raw,
		ContextInfo:      &messageContext,
		MessageType:      messageType,
		MessageTimestamp: int(ts.Unix()),
		InstanceId:       id,
		Source:           "whatsapp",
	}
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
		zap.L().Error("failed to download image", zap.Error(err))
		return "", ""
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		zap.L().Error("failed to seek image", zap.Error(err))
	}

	ext = extractExtFromFile(fileName, mimetype, tmpFile)
	// Include base64 when instance has webhook.base64 or when using global WEBHOOK_URL (so webhook always gets decoded media)
	includeBase64 := (instance.Webhook.Base64 != nil && *instance.Webhook.Base64) || env.Env.WebhookURL != ""
	if includeBase64 {
		data, err := io.ReadAll(tmpFile)
		if err != nil {
			zap.L().Error("failed to read image", zap.Error(err))
		} else {
			b64Result = base64.StdEncoding.EncodeToString(data)
		}
	}
	if s.fileStorage != nil {
		if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
			zap.L().Error("failed to seek image", zap.Error(err))
		}

		urlResult, _, err = s.fileStorage.Upload(ctx, uuid.NewString()+"."+ext, mimetype, tmpFile)
		if err != nil {
			zap.L().Error("failed to upload image", zap.Error(err))
		}
	}

	return urlResult, b64Result
}

func (s *Whatsmiau) convertContact(id string, evt *events.Contact) *WookContact {
	url, _, err := s.getPic(id, evt.JID)
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

	jid, lid := s.GetJidLid(context.Background(), id, evt.JID)
	return &WookContact{
		RemoteJid:     jid,
		RemoteLid:     lid,
		PushName:      name,
		ProfilePicUrl: url,
		InstanceId:    id,
	}
}

func (s *Whatsmiau) convertGroupInfo(id string, evt *events.GroupInfo) *WookContact {
	url, _, err := s.getPic(id, evt.JID)
	if err != nil {
		zap.L().Error("failed to get pic", zap.Error(err))
	}

	if evt.Name == nil || len(evt.Name.Name) == 0 {
		return nil
	}

	if dt := strings.Split(evt.Name.Name, "@"); len(dt) == 2 && (dt[1] == "g.us" || dt[1] == "s.whatsapp.net") {
		return nil
	}

	jid, lid := s.GetJidLid(context.Background(), id, evt.JID)

	return &WookContact{
		RemoteJid:     jid,
		PushName:      evt.Name.Name,
		ProfilePicUrl: url,
		InstanceId:    id,
		RemoteLid:     lid,
	}
}

func (s *Whatsmiau) convertPushName(id string, evt *events.PushName) *WookContact {
	url, _, err := s.getPic(id, evt.JID)
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

	jid, lid := s.GetJidLid(context.Background(), id, evt.JID)

	return &WookContact{
		RemoteJid:     jid,
		PushName:      evt.NewPushName,
		InstanceId:    id,
		ProfilePicUrl: url,
		RemoteLid:     lid,
	}
}

func (s *Whatsmiau) convertPicture(id string, evt *events.Picture) *WookContact {
	url, b64, err := s.getPic(id, evt.JID)
	if err != nil {
		zap.L().Error("failed to get pic", zap.Error(err))
	}

	if len(url) <= 0 {
		return nil
	}

	jid, lid := s.GetJidLid(context.Background(), id, evt.JID)

	return &WookContact{
		RemoteJid:     jid,
		InstanceId:    id,
		Base64Pic:     b64,
		ProfilePicUrl: url,
		RemoteLid:     lid,
	}
}

func (s *Whatsmiau) convertBusinessName(id string, evt *events.BusinessName) *WookContact {
	url, b64, err := s.getPic(id, evt.JID)
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

	jid, lid := s.GetJidLid(context.Background(), id, evt.JID)

	return &WookContact{
		RemoteJid:     jid,
		InstanceId:    id,
		Base64Pic:     b64,
		ProfilePicUrl: url,
		PushName:      name,
		RemoteLid:     lid,
	}
}

func (s *Whatsmiau) getPic(id string, jid types.JID) (string, string, error) {
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
