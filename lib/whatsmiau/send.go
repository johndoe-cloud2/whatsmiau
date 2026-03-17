package whatsmiau

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type SendText struct {
	Text           string     `json:"text"`
	InstanceID     string     `json:"instance_id"`
	RemoteJID      *types.JID `json:"remote_jid"`
	QuoteMessageID string     `json:"quote_message_id"`
	QuoteMessage   string     `json:"quote_message"`
	Participant    *types.JID `json:"participant"`
}

type SendTextResponse struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Whatsmiau) SendText(ctx context.Context, data *SendText) (*SendTextResponse, error) {
	client, ok := s.clients.Load(data.InstanceID)
	if !ok {
		return nil, whatsmeow.ErrClientIsNil
	}

	//rJid := data.RemoteJID.ToNonAD().String()
	var extendedMessage *waE2E.ExtendedTextMessage
	if len(data.QuoteMessage) > 0 && len(data.QuoteMessageID) > 0 {
		extendedMessage = &waE2E.ExtendedTextMessage{
			//ContextInfo: &waE2E.ContextInfo{ // TODO: implement quoted message
			//	StanzaID:    &data.QuoteMessageID,
			//	Participant: &rJid,
			//	QuotedMessage: &waE2E.Message{
			//		Conversation: &data.QuoteMessage,
			//		ProtocolMessage: &waE2E.ProtocolMessage{
			//			Key: &waCommon.MessageKey{
			//				RemoteJID:   &rJid,
			//				FromMe:      &[]bool{true}[0],
			//				ID:          &data.QuoteMessageID,
			//				Participant: nil,
			//			},
			//		},
			//	},
			//},
		}
	}

	res, err := client.SendMessage(ctx, *data.RemoteJID, &waE2E.Message{
		Conversation:        &data.Text,
		ExtendedTextMessage: extendedMessage,
	})
	if err != nil {
		return nil, err
	}

	return &SendTextResponse{
		ID:        res.ID,
		CreatedAt: res.Timestamp,
	}, nil
}

type SendAudioRequest struct {
	AudioURL       string     `json:"text"`
	InstanceID     string     `json:"instance_id"`
	RemoteJID      *types.JID `json:"remote_jid"`
	QuoteMessageID string     `json:"quote_message_id"`
	QuoteMessage   string     `json:"quote_message"`
	Participant    *types.JID `json:"participant"`
}

type SendAudioResponse struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Whatsmiau) SendAudio(ctx context.Context, data *SendAudioRequest) (*SendAudioResponse, error) {
	client, ok := s.clients.Load(data.InstanceID)
	if !ok {
		return nil, whatsmeow.ErrClientIsNil
	}

	dataBytes, err := s.getMediaBytes(ctx, data.AudioURL)
	if err != nil {
		return nil, err
	}

	audioData, waveForm, secs, err := convertAudio(dataBytes, 64)
	if err != nil {
		return nil, err
	}

	uploaded, err := client.Upload(ctx, audioData, whatsmeow.MediaAudio)
	if err != nil {
		return nil, err
	}

	audio := waE2E.AudioMessage{
		URL:           proto.String(uploaded.URL),
		Mimetype:      proto.String("audio/ogg; codecs=opus"),
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uploaded.FileLength),
		Seconds:       proto.Uint32(uint32(secs)),
		PTT:           proto.Bool(true),
		MediaKey:      uploaded.MediaKey,
		FileEncSHA256: uploaded.FileEncSHA256,
		DirectPath:    proto.String(uploaded.DirectPath),
		Waveform:      waveForm,
	}

	res, err := client.SendMessage(ctx, *data.RemoteJID, &waE2E.Message{
		AudioMessage: &audio,
	})
	if err != nil {
		return nil, err
	}

	return &SendAudioResponse{
		ID:        res.ID,
		CreatedAt: res.Timestamp,
	}, nil
}

type SendDocumentRequest struct {
	InstanceID string     `json:"instance_id"`
	MediaURL   string     `json:"media_url"`
	Caption    string     `json:"caption"`
	FileName   string     `json:"file_name"`
	RemoteJID  *types.JID `json:"remote_jid"`
	Mimetype   string     `json:"mimetype"`
}

type SendDocumentResponse struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// SendDocument accepts a PDF URL, converts the first page to PNG, and sends it as an image. Only PDFs are supported.
func (s *Whatsmiau) SendDocument(ctx context.Context, data *SendDocumentRequest) (*SendDocumentResponse, error) {
	res, err := s.SendPDFAsImage(ctx, data)
	if err != nil {
		return nil, err
	}
	return &SendDocumentResponse{ID: res.ID, CreatedAt: res.CreatedAt}, nil
}

type SendImageRequest struct {
	InstanceID string     `json:"instance_id"`
	MediaURL   string     `json:"media_url"`
	MediaBytes []byte     `json:"-"` // optional: send image from bytes instead of URL
	Caption    string     `json:"caption"`
	RemoteJID  *types.JID `json:"remote_jid"`
	Mimetype   string     `json:"mimetype"`
}
type SendImageResponse struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Whatsmiau) SendImage(ctx context.Context, data *SendImageRequest) (*SendImageResponse, error) {
	client, ok := s.clients.Load(data.InstanceID)
	if !ok {
		return nil, whatsmeow.ErrClientIsNil
	}

	var dataBytes []byte
	var err error
	if len(data.MediaBytes) > 0 {
		dataBytes = data.MediaBytes
		if data.Mimetype == "" {
			data.Mimetype = "image/png"
		}
	} else {
		dataBytes, err = s.getMediaBytes(ctx, data.MediaURL)
		if err != nil {
			return nil, err
		}
	}

	uploaded, err := client.Upload(ctx, dataBytes, whatsmeow.MediaImage)
	if err != nil {
		return nil, err
	}

	if data.Mimetype == "" {
		data.Mimetype, err = extractMimetype(dataBytes, uploaded.URL)
	}

	doc := waE2E.ImageMessage{
		URL:           proto.String(uploaded.URL),
		Mimetype:      proto.String(data.Mimetype),
		Caption:       proto.String(data.Caption),
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uploaded.FileLength),
		MediaKey:      uploaded.MediaKey,
		FileEncSHA256: uploaded.FileEncSHA256,
		DirectPath:    proto.String(uploaded.DirectPath),
	}

	res, err := client.SendMessage(ctx, *data.RemoteJID, &waE2E.Message{
		ImageMessage: &doc,
	})
	if err != nil {
		return nil, err
	}

	return &SendImageResponse{
		ID:        res.ID,
		CreatedAt: res.Timestamp,
	}, nil
}

// SendPDFAsImage downloads the PDF from data.MediaURL, converts the first page to PNG (2.5 scale), and sends it as an image.
// Requires pdftoppm (poppler-utils). Use endpoint POST /document with media URL pointing to a PDF.
func (s *Whatsmiau) SendPDFAsImage(ctx context.Context, data *SendDocumentRequest) (*SendImageResponse, error) {
	dataBytes, err := s.getMediaBytes(ctx, data.MediaURL)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(dataBytes, []byte("%PDF")) {
		return nil, fmt.Errorf("URL did not return a valid PDF")
	}
	pngBytes, err := convertPDFBytesToPNG(dataBytes)
	if err != nil {
		return nil, err
	}
	return s.SendImage(ctx, &SendImageRequest{
		InstanceID: data.InstanceID,
		MediaBytes: pngBytes,
		Caption:    data.Caption,
		RemoteJID:  data.RemoteJID,
		Mimetype:   "image/png",
	})
}

type SendReactionRequest struct {
	InstanceID string     `json:"instance_id"`
	Reaction   string     `json:"reaction"`
	RemoteJID  *types.JID `json:"remote_jid"`
	MessageID  string     `json:"message_id"`
	FromMe     bool       `json:"from_me"`
}

type SendReactionResponse struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Whatsmiau) SendReaction(ctx context.Context, data *SendReactionRequest) (*SendReactionResponse, error) {
	client, ok := s.clients.Load(data.InstanceID)
	if !ok {
		return nil, whatsmeow.ErrClientIsNil
	}

	if len(data.Reaction) <= 0 {
		return nil, fmt.Errorf("empty reaction, len: %d", len(data.Reaction))
	}

	if len(data.MessageID) <= 0 {
		return nil, fmt.Errorf("invalid message_id")
	}

	if client.Store == nil || client.Store.ID == nil {
		return nil, fmt.Errorf("device is not connected")
	}

	sender := data.RemoteJID
	if data.FromMe {
		sender = client.Store.ID
	}

	doc := client.BuildReaction(*data.RemoteJID, *sender, data.MessageID, data.Reaction)
	res, err := client.SendMessage(ctx, *data.RemoteJID, doc)
	if err != nil {
		return nil, err
	}

	return &SendReactionResponse{
		ID:        res.ID,
		CreatedAt: res.Timestamp,
	}, nil
}
