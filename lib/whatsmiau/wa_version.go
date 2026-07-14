package whatsmiau

import (
	"net/http"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.uber.org/zap"
	"golang.org/x/net/context"
)

// waVersionMu serializes WA web version refreshes (startup, periodic and 405 recovery).
var waVersionMu sync.Mutex

// RefreshWAVersion fetches the current WhatsApp Web client version and applies it globally.
// Connecting with a stale version makes WhatsApp reject the handshake with 405 ClientOutdated.
// Returns whether the version changed.
func RefreshWAVersion(ctx context.Context) (bool, error) {
	waVersionMu.Lock()
	defer waVersionMu.Unlock()

	latest, err := whatsmeow.GetLatestVersion(ctx, &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		return false, err
	}

	current := store.GetWAVersion()
	if *latest == current {
		return false, nil
	}

	store.SetWAVersion(*latest)
	zap.L().Info("updated WhatsApp web client version",
		zap.String("from", current.String()),
		zap.String("to", latest.String()))
	return true, nil
}

// runWAVersionRefresher keeps the WA web version fresh so connections never handshake with an
// outdated version, and reconnects logged-in clients whose socket dropped (whatsmeow disables
// its own auto-reconnect after a 405).
func (s *Whatsmiau) runWAVersionRefresher() {
	ticker := time.NewTicker(3 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := RefreshWAVersion(ctx)
		cancel()
		if err != nil {
			zap.L().Warn("periodic WA version refresh failed", zap.Error(err))
			continue
		}
		s.reconnectDisconnectedClients()
	}
}

// handleClientOutdated recovers from a 405 ClientOutdated connect failure: whatsmeow stops
// auto-reconnecting on 405, so we fetch the new WA version and reconnect ourselves. Only one
// recovery runs at a time; the initial sleep coalesces the burst of 405 events that all
// instances fire together.
func (s *Whatsmiau) handleClientOutdated(id string) {
	zap.L().Warn("client outdated (405): scheduling WA version refresh and reconnect", zap.String("instance", id))
	if !s.outdatedRecovery.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.outdatedRecovery.Store(false)
		time.Sleep(15 * time.Second)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		changed, err := RefreshWAVersion(ctx)
		cancel()
		if err != nil {
			zap.L().Error("WA version refresh after 405 failed", zap.Error(err))
		}
		if !changed {
			// WhatsApp has not published a newer version yet (or the fetch failed):
			// wait before reconnecting so we don't hot-loop 405 handshakes.
			time.Sleep(3 * time.Minute)
		}
		s.reconnectDisconnectedClients()
	}()
}

// reconnectDisconnectedClients reconnects logged-in clients whose socket is closed.
// Clients still pairing via QR (no Store.ID yet) are skipped.
func (s *Whatsmiau) reconnectDisconnectedClients() {
	s.clients.Range(func(id string, client *whatsmeow.Client) bool {
		if client == nil || client.Store == nil || client.Store.ID == nil || client.IsConnected() {
			return true
		}
		zap.L().Info("reconnecting disconnected client", zap.String("instance", id))
		if err := client.Connect(); err != nil {
			zap.L().Error("failed to reconnect client", zap.String("instance", id), zap.Error(err))
		}
		return true
	})
}
