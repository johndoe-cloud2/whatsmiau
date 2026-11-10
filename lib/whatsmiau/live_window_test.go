package whatsmiau

import (
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func backlogMsg(id string, ts time.Time) *events.Message {
	chat := types.NewJID("5491100000000", types.DefaultUserServer)
	return &events.Message{Info: types.MessageInfo{
		ID:            id,
		Timestamp:     ts,
		MessageSource: types.MessageSource{Chat: chat, Sender: chat},
	}}
}

func newTestWindow(connectedAt, backlogFrom time.Time) *liveWindow {
	w := &liveWindow{connectedAt: connectedAt, backlogFrom: backlogFrom}
	w.backlogLeft.Store(backlogMaxMessages)
	return w
}

func TestShouldDeliverMessage(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s := &Whatsmiau{liveWindows: xsync.NewMap[string, *liveWindow]()}

	// Long outage / fresh pairing: no backlog, only live messages.
	s.liveWindows.Store("a", newTestWindow(now, time.Time{}))
	if !s.shouldDeliverMessage("a", backlogMsg("live", now.Add(time.Second))) {
		t.Fatal("live message must be delivered")
	}
	if !s.shouldDeliverMessage("a", backlogMsg("skew", now.Add(-time.Second))) {
		t.Fatal("message within live tolerance must be delivered")
	}
	if s.shouldDeliverMessage("a", backlogMsg("old", now.Add(-time.Hour))) {
		t.Fatal("backlog must be dropped when no backlog is allowed")
	}

	// Short outage: backlog since the last live mark is delivered, older is dropped.
	s.liveWindows.Store("b", newTestWindow(now, now.Add(-2*time.Minute)))
	if !s.shouldDeliverMessage("b", backlogMsg("gap", now.Add(-time.Minute))) {
		t.Fatal("message from the outage gap must be delivered")
	}
	if s.shouldDeliverMessage("b", backlogMsg("before", now.Add(-5*time.Minute))) {
		t.Fatal("message older than the outage must be dropped")
	}
}

func TestBacklogQuota(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s := &Whatsmiau{liveWindows: xsync.NewMap[string, *liveWindow]()}
	s.liveWindows.Store("c", newTestWindow(now, now.Add(-5*time.Minute)))

	delivered := 0
	for i := 0; i < backlogMaxMessages+10; i++ {
		if s.shouldDeliverMessage("c", backlogMsg("m", now.Add(-time.Minute))) {
			delivered++
		}
	}
	if delivered != backlogMaxMessages {
		t.Fatalf("delivered %d backlog messages, want %d", delivered, backlogMaxMessages)
	}
	if !s.shouldDeliverMessage("c", backlogMsg("live", now)) {
		t.Fatal("live messages are not limited by the backlog quota")
	}
}
