package whatsmiau

import (
	"sort"
	"sync/atomic"
	"time"

	"github.com/verbeux-ai/whatsmiau/repositories/instances"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
	"golang.org/x/net/context"
)

// Backlog policy. A message's Info.Timestamp is stamped by the WhatsApp server when the
// account received it (same value on every linked device), so comparing it with the moment
// our session came up tells live messages from backlog that was queued while we were away.
//
//   - Live messages (stamped after the connection came up) are always delivered.
//   - Backlog (offline queue or history sync) is delivered only after a short outage: the
//     session was live less than backlogMaxGap ago (micro-cuts, deploys, moves between
//     backends) and the message is newer than that last live moment.
//   - After a long outage or a fresh QR pairing nothing older than the connection goes out:
//     those are the cases where the cashier already handled the chat by hand.
//   - At most backlogMaxMessages backlog messages per connection, so a reconnection can never
//     flood the webhook.
//
// The Redis claim dedup still applies, so backlog that was already delivered is skipped.
const (
	backlogMaxGap      = 10 * time.Minute
	backlogMaxMessages = 30
	// backlogMargin widens the window before the last live mark: it covers the heartbeat
	// interval (the mark can be up to liveHeartbeatInterval stale) and clock skew.
	backlogMargin = 60 * time.Second
	// liveTolerance absorbs clock skew between WhatsApp's servers and ours, and the second
	// precision of WhatsApp timestamps, when deciding whether a message is live.
	liveTolerance         = 2 * time.Second
	liveHeartbeatInterval = 30 * time.Second
	liveRedisTimeout      = 2 * time.Second
)

// liveWindow describes the current connection of an instance.
type liveWindow struct {
	connectedAt time.Time
	// backlogFrom: oldest timestamp accepted as recoverable backlog. Zero = no backlog.
	backlogFrom time.Time
	backlogLeft atomic.Int32
}

func (s *Whatsmiau) redisInstances() *instances.RedisInstance {
	r, _ := s.repo.(*instances.RedisInstance)
	return r
}

// openLiveWindow returns the window of the current connection, creating it if needed.
// It is created on Connected or, if the offline queue starts flowing before Connected is
// dispatched, on the first message.
func (s *Whatsmiau) openLiveWindow(id string) *liveWindow {
	if w, ok := s.liveWindows.Load(id); ok {
		return w
	}

	now := time.Now()
	w := &liveWindow{connectedAt: now.Truncate(time.Second)}
	w.backlogLeft.Store(backlogMaxMessages)

	var lastLive time.Time
	found := false
	if r := s.redisInstances(); r != nil {
		ctx, cancel := context.WithTimeout(context.Background(), liveRedisTimeout)
		at, ok, err := r.GetLiveLast(ctx, id)
		cancel()
		if err != nil {
			zap.L().Warn("live window: failed to read last live mark, no backlog will be delivered",
				zap.String("instance", id), zap.Error(err))
		}
		lastLive, found = at, ok
	}
	if found && now.Sub(lastLive) <= backlogMaxGap {
		w.backlogFrom = lastLive.Add(-backlogMargin)
	}

	actual, loaded := s.liveWindows.LoadOrStore(id, w)
	if loaded {
		return actual
	}

	s.markLive([]string{id}, now)
	zap.L().Info("live window opened",
		zap.String("instance", id),
		zap.Time("connectedAt", w.connectedAt),
		zap.Bool("lastLiveFound", found),
		zap.Time("lastLive", lastLive),
		zap.Bool("backlogAllowed", !w.backlogFrom.IsZero()),
		zap.Time("backlogFrom", w.backlogFrom))
	return w
}

// closeLiveWindow ends the current connection window, recording when the session stopped
// being live so a quick reconnection can recover what arrived in between.
func (s *Whatsmiau) closeLiveWindow(id string) {
	if _, ok := s.liveWindows.LoadAndDelete(id); ok {
		s.markLive([]string{id}, time.Now())
	}
}

// resetLiveHistory forgets the instance's last live mark: after a fresh pairing nothing
// from before the scan is recoverable backlog.
func (s *Whatsmiau) resetLiveHistory(id string) {
	s.liveWindows.Delete(id)
	r := s.redisInstances()
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRedisTimeout)
	defer cancel()
	if err := r.DeleteLiveLast(ctx, id); err != nil {
		zap.L().Warn("live window: failed to reset last live mark", zap.String("instance", id), zap.Error(err))
	}
}

func (s *Whatsmiau) markLive(ids []string, at time.Time) {
	r := s.redisInstances()
	if r == nil || len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRedisTimeout)
	defer cancel()
	if err := r.SetLiveLast(ctx, ids, at); err != nil {
		zap.L().Warn("live window: failed to store last live mark", zap.Int("instances", len(ids)), zap.Error(err))
	}
}

// runLiveHeartbeat refreshes the last live mark of every connected instance, so an abrupt
// stop (deploy, crash) leaves a mark at most liveHeartbeatInterval old.
func (s *Whatsmiau) runLiveHeartbeat() {
	ticker := time.NewTicker(liveHeartbeatInterval)
	defer ticker.Stop()
	for range ticker.C {
		ids := make([]string, 0, s.liveWindows.Size())
		s.liveWindows.Range(func(id string, _ *liveWindow) bool {
			ids = append(ids, id)
			return true
		})
		s.markLive(ids, time.Now())
	}
}

// shouldDeliverMessage decides whether an incoming message reaches the webhook.
func (s *Whatsmiau) shouldDeliverMessage(id string, e *events.Message) bool {
	w, ok := s.liveWindows.Load(id)
	if !ok {
		// A message can only arrive over a live socket; the window is missing only when the
		// offline queue starts before Connected is dispatched.
		client, hasClient := s.clients.Load(id)
		if !hasClient || !client.IsConnected() {
			zap.L().Info("dropping message received while not connected",
				zap.String("instance", id), zap.String("messageId", e.Info.ID), zap.Time("messageTimestamp", e.Info.Timestamp))
			return false
		}
		w = s.openLiveWindow(id)
	}

	if !e.Info.Timestamp.Before(w.connectedAt.Add(-liveTolerance)) {
		return true
	}
	return s.acceptBacklog(id, w, e, "offline")
}

// acceptBacklog applies the backlog policy to a message stamped before the connection.
func (s *Whatsmiau) acceptBacklog(id string, w *liveWindow, e *events.Message, source string) bool {
	if w.backlogFrom.IsZero() || e.Info.Timestamp.Before(w.backlogFrom) {
		zap.L().Info("dropping backlog message",
			zap.String("instance", id),
			zap.String("source", source),
			zap.String("messageId", e.Info.ID),
			zap.Time("messageTimestamp", e.Info.Timestamp),
			zap.Time("connectedAt", w.connectedAt),
			zap.Time("backlogFrom", w.backlogFrom))
		return false
	}
	// Own messages and groups are never emitted: don't let them use up the backlog quota.
	if e.Info.IsFromMe || isGroupOrChannelJID(e.Info.Chat.String()) {
		return false
	}
	if w.backlogLeft.Add(-1) < 0 {
		zap.L().Warn("dropping backlog message: backlog limit reached for this connection",
			zap.String("instance", id),
			zap.String("source", source),
			zap.String("messageId", e.Info.ID),
			zap.Time("messageTimestamp", e.Info.Timestamp),
			zap.Int("limit", backlogMaxMessages))
		return false
	}
	zap.L().Info("delivering backlog message",
		zap.String("instance", id),
		zap.String("source", source),
		zap.String("messageId", e.Info.ID),
		zap.Time("messageTimestamp", e.Info.Timestamp),
		zap.Time("connectedAt", w.connectedAt))
	return true
}

// handleHistorySyncNotification downloads a history sync blob only when the current
// connection may recover backlog, and passes its messages through the backlog policy.
// Fresh pairings never download it (that is where the big INITIAL_BOOTSTRAP/FULL blobs are).
func (s *Whatsmiau) handleHistorySyncNotification(id string, notif *waE2E.HistorySyncNotification) {
	syncType := notif.GetSyncType()
	switch syncType {
	case waE2E.HistorySyncType_RECENT, waE2E.HistorySyncType_ON_DEMAND:
	default:
		zap.L().Debug("history sync: ignoring notification", zap.String("instance", id), zap.String("type", syncType.String()))
		return
	}

	w, ok := s.liveWindows.Load(id)
	if !ok || w.backlogFrom.IsZero() {
		zap.L().Info("history sync: skipped, no recoverable backlog for this connection",
			zap.String("instance", id), zap.String("type", syncType.String()))
		return
	}
	client, ok := s.clients.Load(id)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	blob, err := client.DownloadHistorySync(ctx, notif, false)
	if err != nil {
		zap.L().Warn("history sync: download failed", zap.String("instance", id), zap.String("type", syncType.String()), zap.Error(err))
		return
	}
	if err := client.DeleteMedia(ctx, whatsmeow.MediaHistory, notif.GetDirectPath(), notif.GetFileEncSHA256(), notif.GetEncHandle()); err != nil {
		zap.L().Debug("history sync: failed to delete media from server", zap.String("instance", id), zap.Error(err))
	}

	instance := s.getInstanceCached(id)
	if instance == nil {
		return
	}
	eventMap := make(map[string]bool)
	for _, event := range instance.Webhook.Events {
		eventMap[event] = true
	}

	delivered, dropped := 0, 0
	for _, msgEvt := range parseHistoryMessages(id, client, blob) {
		if !s.acceptBacklog(id, w, msgEvt, "history_sync") {
			dropped++
			continue
		}
		s.handleMessageEvent(id, instance, msgEvt, eventMap)
		delivered++
	}
	zap.L().Info("history sync processed",
		zap.String("instance", id),
		zap.String("type", syncType.String()),
		zap.Int("delivered", delivered),
		zap.Int("dropped", dropped))
}

// parseHistoryMessages extracts incoming 1:1 messages from a history sync blob, oldest first.
func parseHistoryMessages(id string, client *whatsmeow.Client, blob *waHistorySync.HistorySync) []*events.Message {
	var parsed []*events.Message
	for _, conv := range blob.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil || isGroupOrChannelJID(chatJID.String()) {
			continue
		}
		for _, histMsg := range conv.GetMessages() {
			webMsg := histMsg.GetMessage()
			if webMsg == nil || webMsg.GetKey().GetFromMe() {
				continue
			}
			msgEvt, err := client.ParseWebMessage(chatJID, webMsg)
			if err != nil {
				zap.L().Debug("history sync: failed to parse message", zap.String("instance", id), zap.String("chat", chatJID.String()), zap.Error(err))
				continue
			}
			parsed = append(parsed, msgEvt)
		}
	}
	// Oldest first: chronological order to the webhook, and the quota goes to the oldest
	// missing messages of the outage.
	sort.Slice(parsed, func(i, j int) bool {
		return parsed[i].Info.Timestamp.Before(parsed[j].Info.Timestamp)
	})
	return parsed
}
