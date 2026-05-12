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
