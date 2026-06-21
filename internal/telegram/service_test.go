package telegram

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestParseUpdate_ValidMessage(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"update_id": 123456,
		"message": {
			"message_id": 42,
			"from": {
				"id": 987654,
				"is_bot": false,
				"first_name": "Alice",
				"last_name": "Smith",
				"username": "alicesmith"
			},
			"chat": {
				"id": 987654,
				"type": "private"
			},
			"date": 1712160000,
			"text": "Hello agent!"
		}
	}`)

	parsed := ParseUpdate(body)
	Expect(parsed).NotTo(BeNil())
	Expect(parsed.MessageID).To(Equal(int64(42)))
	Expect(parsed.ChatID).To(Equal(int64(987654)))
	Expect(parsed.ChatType).To(Equal("private"))
	Expect(parsed.Text).To(Equal("Hello agent!"))
	Expect(parsed.SenderID).To(Equal(int64(987654)))
	Expect(parsed.SenderUsername).To(Equal("alicesmith"))
	Expect(parsed.SenderName).To(Equal("Alice Smith"))
}

func TestParseUpdate_GroupMessage(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"update_id": 123457,
		"message": {
			"message_id": 43,
			"from": {
				"id": 111,
				"first_name": "Bob",
				"username": "bob"
			},
			"chat": {
				"id": -100123456,
				"type": "supergroup",
				"title": "Team Chat"
			},
			"date": 1712160060,
			"text": "Check the deployment"
		}
	}`)

	parsed := ParseUpdate(body)
	Expect(parsed).NotTo(BeNil())
	Expect(parsed.ChatID).To(Equal(int64(-100123456)))
	Expect(parsed.ChatType).To(Equal("supergroup"))
	Expect(parsed.ChatTitle).To(Equal("Team Chat"))
	Expect(parsed.SenderName).To(Equal("Bob"))
}

func TestParseUpdate_EditedMessage(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"update_id": 123458,
		"edited_message": {
			"message_id": 44,
			"from": {
				"id": 222,
				"first_name": "Charlie"
			},
			"chat": {
				"id": 222,
				"type": "private"
			},
			"date": 1712160120,
			"text": "Updated message"
		}
	}`)

	parsed := ParseUpdate(body)
	Expect(parsed).NotTo(BeNil())
	Expect(parsed.Text).To(Equal("Updated message"))
	Expect(parsed.SenderName).To(Equal("Charlie"))
}

func TestParseUpdate_NoMessage_ReturnsNil(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{"update_id": 123459}`)
	parsed := ParseUpdate(body)
	Expect(parsed).To(BeNil())
}

func TestParseUpdate_InvalidJSON_ReturnsNil(t *testing.T) {
	RegisterTestingT(t)

	parsed := ParseUpdate([]byte("not json"))
	Expect(parsed).To(BeNil())
}

func TestParseUpdate_EmptyText(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"update_id": 123460,
		"message": {
			"message_id": 45,
			"from": {"id": 333, "first_name": "Dave"},
			"chat": {"id": 333, "type": "private"},
			"date": 1712160180,
			"text": ""
		}
	}`)

	parsed := ParseUpdate(body)
	Expect(parsed).NotTo(BeNil())
	Expect(parsed.Text).To(Equal(""))
}

func TestParseUpdate_NoFrom(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"update_id": 123461,
		"message": {
			"message_id": 46,
			"chat": {"id": 444, "type": "private"},
			"date": 1712160240,
			"text": "Anonymous message"
		}
	}`)

	parsed := ParseUpdate(body)
	Expect(parsed).NotTo(BeNil())
	Expect(parsed.SenderID).To(Equal(int64(0)))
	Expect(parsed.SenderName).To(Equal(""))
	Expect(parsed.Text).To(Equal("Anonymous message"))
}

// TestParseUpdate_PhotoTakesLargestResolution — Telegram emits the same
// photo as an array of resolutions sharing a file_unique_id; we must pick
// the largest by file_size so the LLM sees the most informative version,
// not a thumbnail.
func TestParseUpdate_PhotoTakesLargestResolution(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"update_id": 200001,
		"message": {
			"message_id": 100,
			"from": {"id": 1, "first_name": "Anna"},
			"chat": {"id": 1, "type": "private"},
			"date": 1712160000,
			"caption": "look at this",
			"photo": [
				{"file_id": "thumb",  "file_unique_id": "u1", "width": 90,  "height": 60,  "file_size": 1500},
				{"file_id": "medium", "file_unique_id": "u1", "width": 320, "height": 213, "file_size": 19000},
				{"file_id": "large",  "file_unique_id": "u1", "width": 1280,"height": 853, "file_size": 254300}
			]
		}
	}`)

	parsed := ParseUpdate(body)
	Expect(parsed).NotTo(BeNil())
	Expect(parsed.Attachments).To(HaveLen(1))
	att := parsed.Attachments[0]
	Expect(att.Kind).To(Equal("photo"))
	Expect(att.FileID).To(Equal("large"))
	Expect(att.Size).To(Equal(int64(254300)))
	Expect(att.Width).To(Equal(1280))
	Expect(att.Mime).To(Equal("image/jpeg"))
	// Empty text on the message → caption gets surfaced as Text so
	// downstream handlers don't see content-less messages with files.
	Expect(parsed.Text).To(Equal("look at this"))
}

// TestParseUpdate_DocumentWithMissingMime — Telegram sometimes omits the
// document mime_type. We default to application/octet-stream so the
// dispatch path always has a non-empty mime to forward.
func TestParseUpdate_DocumentWithMissingMime(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"update_id": 200002,
		"message": {
			"message_id": 101,
			"from": {"id": 2, "first_name": "Bob"},
			"chat": {"id": 2, "type": "private"},
			"date": 1712160000,
			"document": {
				"file_id": "doc1",
				"file_unique_id": "u2",
				"file_name": "report.pdf",
				"file_size": 50000
			}
		}
	}`)

	parsed := ParseUpdate(body)
	Expect(parsed).NotTo(BeNil())
	Expect(parsed.Attachments).To(HaveLen(1))
	att := parsed.Attachments[0]
	Expect(att.Kind).To(Equal("document"))
	Expect(att.Name).To(Equal("report.pdf"))
	Expect(att.Mime).To(Equal("application/octet-stream"))
	Expect(att.Size).To(Equal(int64(50000)))
}

// TestParseUpdate_VideoSurfacesDimensionsAndDuration — videos carry
// width/height/duration that the API may want to render in the
// agent_message context for the LLM.
func TestParseUpdate_VideoSurfacesDimensionsAndDuration(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"update_id": 200003,
		"message": {
			"message_id": 102,
			"from": {"id": 3, "first_name": "Cara"},
			"chat": {"id": 3, "type": "private"},
			"date": 1712160000,
			"video": {
				"file_id": "vid1",
				"file_unique_id": "u3",
				"file_name": "trip.mp4",
				"mime_type": "video/mp4",
				"width": 1920,
				"height": 1080,
				"duration": 42,
				"file_size": 1500000
			}
		}
	}`)

	parsed := ParseUpdate(body)
	Expect(parsed).NotTo(BeNil())
	Expect(parsed.Attachments).To(HaveLen(1))
	att := parsed.Attachments[0]
	Expect(att.Kind).To(Equal("video"))
	Expect(att.Width).To(Equal(1920))
	Expect(att.Height).To(Equal(1080))
	Expect(att.Duration).To(Equal(42))
	Expect(att.Mime).To(Equal("video/mp4"))
}

// TestParseUpdate_MultipleAttachmentsOnOneMessage — Telegram can send a
// document and a video on the same message (rare but valid). The parser
// returns them in stable order: photo, document, video.
func TestParseUpdate_MultipleAttachmentsOnOneMessage(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"update_id": 200004,
		"message": {
			"message_id": 103,
			"from": {"id": 4, "first_name": "Dee"},
			"chat": {"id": 4, "type": "private"},
			"date": 1712160000,
			"photo": [
				{"file_id": "p", "file_unique_id": "up", "width": 800, "height": 600, "file_size": 30000}
			],
			"document": {"file_id": "d", "file_unique_id": "ud", "file_name": "x.txt", "mime_type": "text/plain", "file_size": 100}
		}
	}`)

	parsed := ParseUpdate(body)
	Expect(parsed).NotTo(BeNil())
	Expect(parsed.Attachments).To(HaveLen(2))
	Expect(parsed.Attachments[0].Kind).To(Equal("photo"))
	Expect(parsed.Attachments[1].Kind).To(Equal("document"))
}

// TestParseUpdate_VoiceMessageStillTakesVoicePath — adding attachment
// support must not regress the voice flow. A voice message produces
// IsVoice=true, VoiceFileID populated, AND zero Attachments (voice
// uses its own channel_type for downstream branching).
func TestParseUpdate_VoiceMessageStillTakesVoicePath(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"update_id": 200005,
		"message": {
			"message_id": 104,
			"from": {"id": 5, "first_name": "Erin"},
			"chat": {"id": 5, "type": "private"},
			"date": 1712160000,
			"voice": {
				"file_id": "voice1",
				"file_unique_id": "uv",
				"duration": 5,
				"mime_type": "audio/ogg",
				"file_size": 12000
			}
		}
	}`)

	parsed := ParseUpdate(body)
	Expect(parsed).NotTo(BeNil())
	Expect(parsed.IsVoice).To(BeTrue())
	Expect(parsed.VoiceFileID).To(Equal("voice1"))
	Expect(parsed.Attachments).To(BeEmpty())
}
