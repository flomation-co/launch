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
