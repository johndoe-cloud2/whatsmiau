package whatsmiau

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/verbeux-ai/whatsmiau/models"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestWebhookEventMapNormalizesConfigurationAliases(t *testing.T) {
	tests := []struct {
		input    string
		expected webhookConfigEvent
	}{
		{input: "MESSAGES_UPSERT", expected: webhookConfigMessagesUpsert},
		{input: "messages.upsert", expected: webhookConfigMessagesUpsert},
		{input: "  Messages.Update  ", expected: webhookConfigMessagesUpdate},
		{input: "messages.delete", expected: webhookConfigMessagesDelete},
		{input: "MESSAGES_EDITED", expected: webhookConfigMessagesEdited},
		{input: "messages.edited", expected: webhookConfigMessagesEdited},
		{input: "messages.set", expected: webhookConfigMessagesSet},
		{input: "contacts.upsert", expected: webhookConfigContactsUpsert},
		{input: "group-participants.update", expected: webhookConfigGroupParticipantsUpdate},
		{input: "CONNECTION_UPDATE", expected: webhookConfigConnectionUpdate},
		{input: "connection.update", expected: webhookConfigConnectionUpdate},
		{input: "CALL", expected: webhookConfigCall},
		{input: "call", expected: webhookConfigCall},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			eventMap := webhookEventMap([]string{tt.input})
			if !eventMap[tt.expected] {
				t.Fatalf("expected normalized event %q in %#v", tt.expected, eventMap)
			}
		})
	}
}

func TestWebhookEventMapDoesNotEnableSupportedEventsForEmptyOrUnknownInput(t *testing.T) {
	supportedEvents := []webhookConfigEvent{
		webhookConfigMessagesUpsert,
		webhookConfigMessagesUpdate,
		webhookConfigMessagesDelete,
		webhookConfigMessagesEdited,
		webhookConfigMessagesSet,
		webhookConfigContactsUpsert,
		webhookConfigGroupParticipantsUpdate,
		webhookConfigConnectionUpdate,
		webhookConfigCall,
	}

	for _, input := range []string{"", "   ", "unknown.event"} {
		t.Run(input, func(t *testing.T) {
			eventMap := webhookEventMap([]string{input})
			for _, event := range supportedEvents {
				if eventMap[event] {
					t.Fatalf("input %q unexpectedly enabled supported event %q", input, event)
				}
			}
		})
	}
}

func TestWebhookPayloadEventIdentifiersRemainLowercase(t *testing.T) {
	tests := []struct {
		event    Wook
		expected string
	}{
		{event: WookMessagesUpsert, expected: "messages.upsert"},
		{event: WookMessagesUpdate, expected: "messages.update"},
		{event: WookMessagesDelete, expected: "messages.delete"},
		{event: WookMessagesSet, expected: "messages.set"},
		{event: WookContactsUpsert, expected: "contacts.upsert"},
		{event: WookGroupParticipantsUpdate, expected: "group-participants.update"},
		{event: WookConnectionUpdate, expected: "connection.update"},
		{event: WookCall, expected: "call"},
	}

	for _, tt := range tests {
		t.Run(string(tt.event), func(t *testing.T) {
			payload, err := json.Marshal(WookEvent[struct{}]{Event: tt.event})
			if err != nil {
				t.Fatalf("marshal webhook event: %v", err)
			}

			var envelope struct {
				Event string `json:"event"`
			}
			if err := json.Unmarshal(payload, &envelope); err != nil {
				t.Fatalf("unmarshal webhook event: %v", err)
			}
			if envelope.Event != tt.expected {
				t.Fatalf("expected event %q, got %q", tt.expected, envelope.Event)
			}
		})
	}
}

func testMessageEditEvent() *events.Message {
	editType := waE2E.ProtocolMessage_MESSAGE_EDIT
	originalJID := "5511999999999@s.whatsapp.net"
	originalID := "original-message"
	fromMe := false
	conversation := "edited text"
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   types.NewJID("5511999999999", types.DefaultUserServer),
				Sender: types.NewJID("5511888888888", types.DefaultUserServer),
			},
			ID:        "edit-event",
			Timestamp: time.Date(2026, time.November, 4, 12, 0, 0, 0, time.UTC),
		},
		Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: &editType,
			Key: &waCommon.MessageKey{
				RemoteJID: &originalJID,
				ID:        &originalID,
				FromMe:    &fromMe,
			},
			EditedMessage: &waE2E.Message{Conversation: &conversation},
		}},
	}
}

func TestEnabledWebhookEventsDeactivateDeliveryWithoutTouchingConfiguration(t *testing.T) {
	disabled := false
	enabled := true

	t.Run("disabled webhook delivers nothing", func(t *testing.T) {
		events := enabledWebhookEvents(models.InstanceWebhook{
			Enabled: &disabled,
			Events:  []string{"MESSAGES_UPSERT", "CALL"},
		})
		if len(events) != 0 {
			t.Fatalf("expected no delivered events, got %#v", events)
		}
	})

	t.Run("enabled webhook delivers configured events", func(t *testing.T) {
		events := enabledWebhookEvents(models.InstanceWebhook{
			Enabled: &enabled,
			Events:  []string{"messages.upsert", "CALL"},
		})
		if !events[webhookConfigMessagesUpsert] || !events[webhookConfigCall] {
			t.Fatalf("expected configured events, got %#v", events)
		}
	})

	t.Run("webhook without an explicit switch delivers configured events", func(t *testing.T) {
		events := enabledWebhookEvents(models.InstanceWebhook{Events: []string{"CALL"}})
		if !events[webhookConfigCall] {
			t.Fatalf("expected configured events, got %#v", events)
		}
	})
}
