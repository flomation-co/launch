package facebook

import (
	"encoding/json"
	"fmt"
	"strings"
)

// webhookEnvelope is the top-level structure of a Facebook webhook payload.
type webhookEnvelope struct {
	Object string         `json:"object"`
	Entry  []webhookEntry `json:"entry"`
}

type webhookEntry struct {
	ID        string            `json:"id"` // Page ID
	Time      int64             `json:"time"`
	Messaging []json.RawMessage `json:"messaging"`
	Changes   []feedChange      `json:"changes"`
}

type feedChange struct {
	Field string          `json:"field"` // "feed"
	Value json.RawMessage `json:"value"`
}

type feedChangeValue struct {
	Item         string `json:"item"` // "comment", "reaction", "post", "share", "like"
	Verb         string `json:"verb"` // "add", "edit", "remove"
	CommentID    string `json:"comment_id"`
	PostID       string `json:"post_id"`
	ParentID     string `json:"parent_id"`
	SenderID     int64  `json:"sender_id"`
	SenderName   string `json:"sender_name"`
	Message      string `json:"message"`
	ReactionType string `json:"reaction_type"` // "like", "love", "haha", "wow", "sad", "angry"
	CreatedTime  int64  `json:"created_time"`
}

// ParseEnvelope parses the top-level webhook envelope to determine the
// object type and extract page IDs. Returns the parsed envelope or an error.
func ParseEnvelope(body []byte) (*webhookEnvelope, error) {
	var env webhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("failed to parse webhook body: %w", err)
	}
	if env.Object != "page" {
		return nil, fmt.Errorf("unexpected webhook object type: %s", env.Object)
	}
	return &env, nil
}

// ParseMessengerEvents extracts Messenger messages from a webhook entry.
func ParseMessengerEvents(entry webhookEntry) []MessengerMessage {
	var messages []MessengerMessage

	for _, raw := range entry.Messaging {
		var msg struct {
			Sender    struct{ ID string } `json:"sender"`
			Recipient struct{ ID string } `json:"recipient"`
			Timestamp int64               `json:"timestamp"`
			Message   *struct {
				MID         string `json:"mid"`
				Text        string `json:"text"`
				IsEcho      bool   `json:"is_echo"`
				Attachments []struct {
					Type    string `json:"type"`
					Payload struct {
						URL string `json:"url"`
					} `json:"payload"`
				} `json:"attachments"`
			} `json:"message"`
			Postback *struct {
				Title   string `json:"title"`
				Payload string `json:"payload"`
			} `json:"postback"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		m := MessengerMessage{
			SenderPSID:  msg.Sender.ID,
			RecipientID: msg.Recipient.ID,
			Timestamp:   msg.Timestamp,
		}

		if msg.Message != nil {
			m.Text = msg.Message.Text
			m.MessageID = msg.Message.MID
			for _, att := range msg.Message.Attachments {
				m.Attachments = append(m.Attachments, Attachment{
					Type:       att.Type,
					PayloadURL: att.Payload.URL,
				})
			}
		}

		if msg.Postback != nil {
			m.IsPostback = true
			m.PostbackTitle = msg.Postback.Title
			if m.Text == "" {
				m.Text = msg.Postback.Payload
			}
		}

		// Skip echo messages (sent by our own page)
		if msg.Message != nil && msg.Message.IsEcho {
			continue
		}
		if m.SenderPSID == "" || m.SenderPSID == m.RecipientID {
			continue
		}

		messages = append(messages, m)
	}

	return messages
}

// ParseFeedEvents extracts feed change events from a webhook entry.
func ParseFeedEvents(entry webhookEntry) []FeedEvent {
	var events []FeedEvent

	for _, change := range entry.Changes {
		if change.Field != "feed" {
			continue
		}

		var val feedChangeValue
		if err := json.Unmarshal(change.Value, &val); err != nil {
			continue
		}

		itemID := val.CommentID
		if itemID == "" {
			itemID = val.PostID
		}

		events = append(events, FeedEvent{
			PageID:       entry.ID,
			EventType:    val.Item,
			Verb:         val.Verb,
			ItemID:       itemID,
			ParentID:     val.ParentID,
			PostID:       val.PostID,
			SenderID:     fmt.Sprintf("%d", val.SenderID),
			SenderName:   val.SenderName,
			Message:      val.Message,
			ReactionType: val.ReactionType,
			CreatedTime:  val.CreatedTime,
		})
	}

	return events
}

// MatchesFilter checks whether the event type matches the comma-separated filter.
// An empty filter matches all events.
func MatchesFilter(eventType, filter string) bool {
	if filter == "" {
		return true
	}
	for _, f := range strings.Split(filter, ",") {
		if strings.TrimSpace(f) == eventType {
			return true
		}
	}
	return false
}
