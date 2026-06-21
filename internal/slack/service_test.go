package slack

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestParseEvent_URLVerification(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{"type":"url_verification","challenge":"abc123","token":"tok"}`)
	msg, verification := ParseEvent(body)
	Expect(msg).To(BeNil())
	Expect(verification).NotTo(BeNil())
	Expect(verification.Challenge).To(Equal("abc123"))
}

func TestParseEvent_Message(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"type": "event_callback",
		"team_id": "T123",
		"event_id": "Ev123",
		"event_time": 1712160000,
		"event": {
			"type": "message",
			"text": "Hello agent!",
			"user": "U456",
			"channel": "C789",
			"ts": "1712160000.000100"
		}
	}`)

	msg, verification := ParseEvent(body)
	Expect(verification).To(BeNil())
	Expect(msg).NotTo(BeNil())
	Expect(msg.Text).To(Equal("Hello agent!"))
	Expect(msg.UserID).To(Equal("U456"))
	Expect(msg.ChannelID).To(Equal("C789"))
	Expect(msg.TeamID).To(Equal("T123"))
	Expect(msg.EventType).To(Equal("message"))
}

func TestParseEvent_AppMention(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"type": "event_callback",
		"team_id": "T123",
		"event_id": "Ev456",
		"event": {
			"type": "app_mention",
			"text": "<@U111> what is the status?",
			"user": "U789",
			"channel": "C012",
			"ts": "1712160060.000200",
			"thread_ts": "1712160000.000100"
		}
	}`)

	msg, _ := ParseEvent(body)
	Expect(msg).NotTo(BeNil())
	Expect(msg.Text).To(Equal("<@U111> what is the status?"))
	Expect(msg.EventType).To(Equal("app_mention"))
	Expect(msg.ThreadTS).To(Equal("1712160000.000100"))
}

func TestParseEvent_BotMessage_Skipped(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"type": "event_callback",
		"event": {
			"type": "message",
			"text": "Bot reply",
			"user": "U111",
			"channel": "C222",
			"ts": "1712160120.000300",
			"bot_id": "B333"
		}
	}`)

	msg, _ := ParseEvent(body)
	Expect(msg).To(BeNil())
}

func TestParseEvent_SubtypeMessage_Skipped(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"type": "event_callback",
		"event": {
			"type": "message",
			"subtype": "message_changed",
			"text": "Edited",
			"channel": "C444"
		}
	}`)

	msg, _ := ParseEvent(body)
	Expect(msg).To(BeNil())
}

func TestParseEvent_InvalidJSON(t *testing.T) {
	RegisterTestingT(t)

	msg, verification := ParseEvent([]byte("not json"))
	Expect(msg).To(BeNil())
	Expect(verification).To(BeNil())
}

func TestParseEvent_UnknownType(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{"type":"some_other_type"}`)
	msg, verification := ParseEvent(body)
	Expect(msg).To(BeNil())
	Expect(verification).To(BeNil())
}

// TestParseEvent_FileShare_PhotoAttachment — a file_share message
// carrying a single image. The subtype filter must let it through
// (the M3 contract change) AND the parser must surface the file in
// the unified Attachments slice with the right mime + kind.
func TestParseEvent_FileShare_PhotoAttachment(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"Ev1",
		"event":{
			"type":"message",
			"subtype":"file_share",
			"channel":"C123",
			"user":"U1",
			"ts":"1712160000.000100",
			"text":"check this out",
			"files":[
				{
					"id":"F8K9LMVNZ",
					"name":"sunset.jpg",
					"title":"sunset.jpg",
					"mimetype":"image/jpeg",
					"size":254300,
					"url_private":"https://files.slack.com/files-pri/T1-F8K9LMVNZ/sunset.jpg"
				}
			]
		}
	}`)
	msg, ver := ParseEvent(body)
	Expect(ver).To(BeNil())
	Expect(msg).NotTo(BeNil())
	Expect(msg.Text).To(Equal("check this out"))
	Expect(msg.Attachments).To(HaveLen(1))

	att := msg.Attachments[0]
	Expect(att.Kind).To(Equal("photo"))
	Expect(att.FileID).To(Equal("F8K9LMVNZ"))
	Expect(att.Mime).To(Equal("image/jpeg"))
	Expect(att.Size).To(Equal(int64(254300)))
	Expect(att.URLPrivate).To(HavePrefix("https://files.slack.com/"))
}

// TestParseEvent_FileShare_DocumentAndPhoto — two attachments on
// one message. They should both come through with stable order
// (Slack's array order preserved).
func TestParseEvent_FileShare_DocumentAndPhoto(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"type":"event_callback",
		"team_id":"T1",
		"event_id":"Ev2",
		"event":{
			"type":"message",
			"subtype":"file_share",
			"channel":"C123",
			"user":"U1",
			"ts":"1712160001.000100",
			"text":"",
			"files":[
				{"id":"F1","name":"doc.pdf","mimetype":"application/pdf","size":1024,"url_private":"https://files.slack.com/d"},
				{"id":"F2","name":"img.png","mimetype":"image/png","size":2048,"url_private":"https://files.slack.com/i"}
			]
		}
	}`)
	msg, _ := ParseEvent(body)
	Expect(msg).NotTo(BeNil())
	Expect(msg.Attachments).To(HaveLen(2))
	Expect(msg.Attachments[0].Kind).To(Equal("document"))
	Expect(msg.Attachments[0].Name).To(Equal("doc.pdf"))
	Expect(msg.Attachments[1].Kind).To(Equal("photo"))
	Expect(msg.Attachments[1].Name).To(Equal("img.png"))
}

// TestParseEvent_FileShare_NoUrlPrivate_Skipped — Slack sometimes
// tombstones files with no url_private (workspace export deleted,
// retention expired). We must skip those, not crash and not pass an
// empty URL through to the downloader.
func TestParseEvent_FileShare_NoUrlPrivate_Skipped(t *testing.T) {
	RegisterTestingT(t)

	body := []byte(`{
		"type":"event_callback","team_id":"T1","event_id":"Ev3",
		"event":{
			"type":"message","subtype":"file_share","channel":"C123","user":"U1","ts":"1712160002.000100",
			"text":"",
			"files":[{"id":"F-ghost","name":"missing.png","mimetype":"image/png","size":0}]
		}
	}`)
	msg, _ := ParseEvent(body)
	Expect(msg).NotTo(BeNil())
	Expect(msg.Attachments).To(BeEmpty())
}

// TestParseEvent_NonFileShareSubtype_StillDropped — guarding against
// the worry that loosening the subtype filter could accidentally let
// edits / joins / unfurls through. Anything that isn't "" or
// "file_share" should still be rejected.
func TestParseEvent_NonFileShareSubtype_StillDropped(t *testing.T) {
	RegisterTestingT(t)

	for _, subtype := range []string{"message_changed", "channel_join", "channel_leave", "message_deleted"} {
		body := []byte(`{
			"type":"event_callback","team_id":"T1","event_id":"E",
			"event":{"type":"message","subtype":"` + subtype + `","channel":"C","user":"U","ts":"1.0","text":"x"}
		}`)
		msg, _ := ParseEvent(body)
		Expect(msg).To(BeNil(), "subtype %q should be dropped", subtype)
	}
}

// TestSlackKindForMime — pure-function smoke test for the kind
// vocabulary so changes to the mapping show up in the diff loudly.
func TestSlackKindForMime(t *testing.T) {
	RegisterTestingT(t)
	Expect(slackKindForMime("image/png")).To(Equal("photo"))
	Expect(slackKindForMime("image/jpeg")).To(Equal("photo"))
	Expect(slackKindForMime("video/mp4")).To(Equal("video"))
	Expect(slackKindForMime("audio/ogg")).To(Equal("audio"))
	Expect(slackKindForMime("application/pdf")).To(Equal("document"))
	Expect(slackKindForMime("text/plain")).To(Equal("document"))
	Expect(slackKindForMime("")).To(Equal("document"))
}

func TestSocketManager_IsConnected_NoClient(t *testing.T) {
	RegisterTestingT(t)

	sm := NewSocketManager()
	Expect(sm.IsConnected("nonexistent")).To(BeFalse())
}

func TestSocketManager_ReconnectStale_NoClients(t *testing.T) {
	RegisterTestingT(t)

	sm := NewSocketManager()
	n := sm.ReconnectStale()
	Expect(n).To(Equal(0))
}

func TestSocketClient_AliveFlag_DefaultFalse(t *testing.T) {
	RegisterTestingT(t)

	client := &SocketClient{agentID: "test"}
	Expect(client.alive.Load()).To(BeFalse())
}

func TestSocketClient_AliveFlag_SetAndRead(t *testing.T) {
	RegisterTestingT(t)

	client := &SocketClient{agentID: "test"}
	client.alive.Store(true)
	Expect(client.alive.Load()).To(BeTrue())

	client.alive.Store(false)
	Expect(client.alive.Load()).To(BeFalse())
}
