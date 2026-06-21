// Package telegram handles Telegram Bot API webhook registration and update parsing.
// When an agent with a Telegram channel is started, the service registers a webhook
// with Telegram pointing to this Launch instance. Incoming updates are parsed and
// routed to the agent dispatch pipeline.
package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	telegramAPIBase = "https://api.telegram.org"
	httpTimeout     = 15 * time.Second
)

// Service manages Telegram webhook registrations for agents.
type Service struct {
	mu          sync.RWMutex
	webhookBase string            // e.g. "https://launch.flomation.app"
	registered  map[string]string // agentID → bot token (for cleanup)
	client      *http.Client
}

// NewService creates a Telegram service. webhookBase is the public URL
// of the Launch service (e.g. "https://launch.flomation.app").
func NewService(webhookBase string) *Service {
	return &Service{
		webhookBase: webhookBase,
		registered:  make(map[string]string),
		client:      &http.Client{Timeout: httpTimeout},
	}
}

// RegisterWebhook calls the Telegram Bot API to set the webhook URL.
// The webhook points to: {webhookBase}/webhook/telegram/{id}
//
// id is whatever identifier the channel handler will read from the URL
// — historically an agent_id, currently transitioning to trigger_id as
// part of the trigger-keyed webhook refactor. The handler itself looks
// up whichever interpretation applies.
func (s *Service) RegisterWebhook(id string, botToken string) error {
	webhookURL := fmt.Sprintf("%s/webhook/telegram/%s", s.webhookBase, id)

	payload, err := json.Marshal(map[string]interface{}{
		"url":                  webhookURL,
		"allowed_updates":      []string{"message"},
		"drop_pending_updates": true,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal setWebhook payload: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/setWebhook", telegramAPIBase, botToken)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to call setWebhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result telegramResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode setWebhook response: %w", err)
	}

	if !result.OK {
		return fmt.Errorf("setWebhook failed: %s (code %d)", result.Description, result.ErrorCode)
	}

	s.mu.Lock()
	s.registered[id] = botToken
	s.mu.Unlock()

	log.WithFields(log.Fields{
		"id":          id,
		"webhook_url": webhookURL,
	}).Info("telegram webhook registered")

	return nil
}

// DeregisterWebhook removes the Telegram webhook for the given id
// (agent_id or trigger_id — see RegisterWebhook).
func (s *Service) DeregisterWebhook(id string) error {
	s.mu.RLock()
	botToken, exists := s.registered[id]
	s.mu.RUnlock()

	if !exists {
		return nil // nothing to deregister
	}

	url := fmt.Sprintf("%s/bot%s/deleteWebhook", telegramAPIBase, botToken)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("failed to call deleteWebhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	s.mu.Lock()
	delete(s.registered, id)
	s.mu.Unlock()

	log.WithFields(log.Fields{
		"id": id,
	}).Info("telegram webhook deregistered")

	return nil
}

// ParseUpdate extracts message information from a Telegram webhook update.
// Returns nil if the update doesn't contain a usable message.
func ParseUpdate(body []byte) *ParsedMessage {
	var update telegramUpdate
	if err := json.Unmarshal(body, &update); err != nil {
		return nil
	}

	msg := update.Message
	if msg == nil {
		// Could be an edited message or other update type
		msg = update.EditedMessage
	}
	if msg == nil {
		return nil
	}

	parsed := &ParsedMessage{
		MessageID: msg.MessageID,
		ChatID:    msg.Chat.ID,
		Text:      msg.Text,
		Date:      time.Unix(msg.Date, 0),
	}

	// Build sender name
	if msg.From != nil {
		parsed.SenderID = msg.From.ID
		parsed.SenderUsername = msg.From.Username
		if msg.From.FirstName != "" {
			parsed.SenderName = msg.From.FirstName
			if msg.From.LastName != "" {
				parsed.SenderName += " " + msg.From.LastName
			}
		}
	}

	// Chat metadata
	parsed.ChatType = msg.Chat.Type
	if msg.Chat.Title != "" {
		parsed.ChatTitle = msg.Chat.Title
	}

	// Voice message detection — voice notes and audio files
	if msg.Voice != nil {
		parsed.IsVoice = true
		parsed.VoiceFileID = msg.Voice.FileID
		parsed.VoiceDuration = msg.Voice.Duration
		parsed.VoiceMimeType = msg.Voice.MimeType
		if parsed.Text == "" {
			parsed.Text = msg.Caption // voice notes may have a caption
		}
	} else if msg.Audio != nil {
		parsed.IsVoice = true
		parsed.VoiceFileID = msg.Audio.FileID
		parsed.VoiceDuration = msg.Audio.Duration
		parsed.VoiceMimeType = msg.Audio.MimeType
		if parsed.Text == "" {
			parsed.Text = msg.Caption
		}
	}

	// Non-voice attachments. Telegram never combines voice with these
	// in a single message, so the else-if-voice branches above and the
	// attachment scan below cannot fire on the same Update — but the
	// code reads each independently so a future API surface change can't
	// silently start dropping data either way.
	parsed.Attachments = extractAttachments(msg)
	// If the message has attachments but no text, surface the caption so
	// downstream handlers don't see an empty Content alongside the files.
	if parsed.Text == "" && len(parsed.Attachments) > 0 {
		parsed.Text = msg.Caption
	}

	return parsed
}

// extractAttachments folds Photo/Document/Video into a single ordered
// list of ParsedAttachment entries.
//
//   - For photos, Telegram emits an array of resolutions sharing the
//     same file_unique_id. We always take the largest by file_size (or
//     by width when sizes are missing), since the LLM should see the
//     most informative version. Lower resolutions exist for clients
//     that need a thumbnail but are useless to a flow.
//   - Documents and videos appear once each per message.
//   - The Kind field disambiguates the original Telegram field, useful
//     for downstream UIs that want to render a video player differently
//     from a document badge.
func extractAttachments(msg *telegramMessage) []ParsedAttachment {
	if msg == nil {
		return nil
	}
	var out []ParsedAttachment

	if largest := largestPhoto(msg.Photo); largest != nil {
		out = append(out, ParsedAttachment{
			FileID: largest.FileID,
			Name:   fmt.Sprintf("photo_%s.jpg", largest.FileUniqueID),
			Mime:   "image/jpeg",
			Size:   largest.FileSize,
			Kind:   "photo",
			Width:  largest.Width,
			Height: largest.Height,
		})
	}
	if msg.Document != nil {
		name := msg.Document.FileName
		if name == "" {
			name = fmt.Sprintf("document_%s.bin", msg.Document.FileUniqueID)
		}
		mime := msg.Document.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		out = append(out, ParsedAttachment{
			FileID: msg.Document.FileID,
			Name:   name,
			Mime:   mime,
			Size:   msg.Document.FileSize,
			Kind:   "document",
		})
	}
	if msg.Video != nil {
		name := msg.Video.FileName
		if name == "" {
			name = fmt.Sprintf("video_%s.mp4", msg.Video.FileUniqueID)
		}
		mime := msg.Video.MimeType
		if mime == "" {
			mime = "video/mp4"
		}
		out = append(out, ParsedAttachment{
			FileID:   msg.Video.FileID,
			Name:     name,
			Mime:     mime,
			Size:     msg.Video.FileSize,
			Kind:     "video",
			Width:    msg.Video.Width,
			Height:   msg.Video.Height,
			Duration: msg.Video.Duration,
		})
	}
	return out
}

// largestPhoto picks the largest available resolution from the
// Telegram photo_size array. Larger file_size wins; ties break on
// width.
func largestPhoto(sizes []telegramPhotoSize) *telegramPhotoSize {
	if len(sizes) == 0 {
		return nil
	}
	bestIdx := 0
	for i := 1; i < len(sizes); i++ {
		if sizes[i].FileSize > sizes[bestIdx].FileSize {
			bestIdx = i
			continue
		}
		if sizes[i].FileSize == sizes[bestIdx].FileSize && sizes[i].Width > sizes[bestIdx].Width {
			bestIdx = i
		}
	}
	return &sizes[bestIdx]
}

// ParsedMessage contains the extracted fields from a Telegram update.
type ParsedMessage struct {
	MessageID      int64     `json:"message_id"`
	ChatID         int64     `json:"chat_id"`
	ChatType       string    `json:"chat_type"` // private, group, supergroup, channel
	ChatTitle      string    `json:"chat_title,omitempty"`
	Text           string    `json:"text"`
	SenderID       int64     `json:"sender_id"`
	SenderUsername string    `json:"sender_username,omitempty"`
	SenderName     string    `json:"sender_name,omitempty"`
	Date           time.Time `json:"date"`
	IsVoice        bool      `json:"is_voice,omitempty"`
	VoiceFileID    string    `json:"voice_file_id,omitempty"`
	VoiceDuration  int       `json:"voice_duration,omitempty"`
	VoiceMimeType  string    `json:"voice_mime_type,omitempty"`
	// Attachments is the unified non-voice file list — populated when
	// the Telegram message includes Photo, Document, Video, or Audio
	// (non-voice) entries. Voice is intentionally handled by the
	// existing IsVoice/VoiceFileID path; it carries its own trigger
	// data shape (`channel_type: telegram_voice`) that downstream
	// flows already depend on.
	Attachments []ParsedAttachment `json:"attachments,omitempty"`
}

// ParsedAttachment is a single non-voice file Telegram sent alongside
// the message. Kind classifies the source (photo / document / video /
// audio) so the dispatch path can render a helpful display name when
// Telegram itself omits one (photos never carry a filename).
type ParsedAttachment struct {
	FileID    string `json:"file_id"`
	Name      string `json:"name,omitempty"` // Telegram document/audio name; synthesised for photos/videos
	Mime      string `json:"mime,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Kind      string `json:"kind"` // "photo" | "document" | "video" | "audio"
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Duration  int    `json:"duration,omitempty"`
}

// --- Telegram API types (minimal subset) ---

type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description,omitempty"`
	ErrorCode   int    `json:"error_code,omitempty"`
}

type telegramUpdate struct {
	UpdateID      int64            `json:"update_id"`
	Message       *telegramMessage `json:"message,omitempty"`
	EditedMessage *telegramMessage `json:"edited_message,omitempty"`
}

type telegramMessage struct {
	MessageID int64             `json:"message_id"`
	From      *telegramUser     `json:"from,omitempty"`
	Chat      telegramChat      `json:"chat"`
	Date      int64             `json:"date"`
	Text      string             `json:"text"`
	Voice     *telegramVoice     `json:"voice,omitempty"`
	Audio     *telegramAudio     `json:"audio,omitempty"`
	Caption   string             `json:"caption,omitempty"`
	// Non-voice attachment fields. Telegram emits these as siblings
	// of Voice; the parser surfaces them as a unified Attachments slice
	// on ParsedMessage so the dispatch path can iterate uniformly.
	Photo    []telegramPhotoSize `json:"photo,omitempty"`
	Document *telegramDocument   `json:"document,omitempty"`
	Video    *telegramVideo      `json:"video,omitempty"`
}

type telegramPhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type telegramDocument struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type telegramVideo struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Duration     int    `json:"duration"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	FileName     string `json:"file_name,omitempty"`
}

type telegramVoice struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Duration     int    `json:"duration"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type telegramAudio struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Duration     int    `json:"duration"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	Title        string `json:"title,omitempty"`
	Performer    string `json:"performer,omitempty"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

type telegramChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"` // private, group, supergroup, channel
	Title string `json:"title,omitempty"`
}

// SendMessage sends a text message via the Telegram Bot API.
// Used by the agent service to deliver outbound messages.
// SendChatAction sends a chat action (e.g. "typing") to a Telegram chat.
// Valid actions: typing, upload_photo, record_video, upload_video,
// record_voice, upload_voice, upload_document, choose_sticker,
// find_location, record_video_note, upload_video_note.
func SendChatAction(botToken string, chatID int64, action string) error {
	payload := map[string]interface{}{
		"chat_id": chatID,
		"action":  action,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal sendChatAction payload: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendChatAction", telegramAPIBase, botToken)
	client := &http.Client{Timeout: 3 * time.Second} // Typing indicators are best-effort, short timeout
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to call sendChatAction: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("failed to decode sendChatAction response: %w", err)
	}

	if !result.OK {
		return fmt.Errorf("sendChatAction failed: %s", result.Description)
	}
	return nil
}

func SendMessage(botToken string, chatID int64, text string, parseMode string) (int64, error) {
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal sendMessage payload: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPIBase, botToken)
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("failed to call sendMessage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, fmt.Errorf("failed to decode sendMessage response: %w", err)
	}

	if !result.OK {
		return 0, fmt.Errorf("sendMessage failed: %s", result.Description)
	}

	return result.Result.MessageID, nil
}

// DownloadFile downloads a file from Telegram by file_id.
// Uses getFile to get the file path, then downloads the raw bytes.
func DownloadFile(botToken, fileID string) ([]byte, error) {
	// Step 1: getFile to get the file_path
	getURL := fmt.Sprintf("%s/bot%s/getFile?file_id=%s", telegramAPIBase, botToken, fileID)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(getURL) // #nosec G107 — constructed from known base + bot token
	if err != nil {
		return nil, fmt.Errorf("getFile failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var fileResp struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
			FileSize int64  `json:"file_size"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &fileResp); err != nil {
		return nil, fmt.Errorf("failed to parse getFile response: %w", err)
	}
	if !fileResp.OK {
		return nil, fmt.Errorf("getFile error: %s", fileResp.Description)
	}

	// Step 2: download the file
	downloadURL := fmt.Sprintf("%s/file/bot%s/%s", telegramAPIBase, botToken, fileResp.Result.FilePath)
	dlResp, err := client.Get(downloadURL) // #nosec G107 — Telegram file download URL
	if err != nil {
		return nil, fmt.Errorf("file download failed: %w", err)
	}
	defer func() { _ = dlResp.Body.Close() }()

	if dlResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("file download returned %d", dlResp.StatusCode)
	}

	// Limit to 20MB (Telegram's max file size)
	data, err := io.ReadAll(io.LimitReader(dlResp.Body, 20<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	return data, nil
}

// SendVoice sends a voice message (OGG/OPUS) via the Telegram Bot API.
// audioData should be OGG-encoded audio bytes.
func SendVoice(botToken string, chatID int64, audioData []byte, caption string) (int64, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	_ = writer.WriteField("chat_id", fmt.Sprintf("%d", chatID))
	if caption != "" {
		_ = writer.WriteField("caption", caption)
	}

	part, err := writer.CreateFormFile("voice", "voice.ogg")
	if err != nil {
		return 0, fmt.Errorf("failed to create form file: %w", err)
	}
	if _, err := part.Write(audioData); err != nil {
		return 0, fmt.Errorf("failed to write audio data: %w", err)
	}
	if err := writer.Close(); err != nil {
		return 0, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendVoice", telegramAPIBase, botToken)
	req, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		return 0, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("sendVoice failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, fmt.Errorf("failed to decode sendVoice response: %w", err)
	}
	if !result.OK {
		return 0, fmt.Errorf("sendVoice failed: %s", result.Description)
	}

	return result.Result.MessageID, nil
}
