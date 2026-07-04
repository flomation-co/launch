package telegram

import "testing"

func TestParseCallback_ExtractsButtonPress(t *testing.T) {
	body := []byte(`{
		"update_id": 42,
		"callback_query": {
			"id": "cbq-1",
			"from": {"id": 555, "first_name": "Ada", "last_name": "Lovelace", "username": "ada"},
			"message": {"message_id": 99, "chat": {"id": 12345, "type": "private"}, "date": 0, "text": "Approve?"},
			"data": "hitl:tok_abc123"
		}
	}`)

	cb := ParseCallback(body)
	if cb == nil {
		t.Fatal("expected a parsed callback, got nil")
	}
	if cb.CallbackID != "cbq-1" {
		t.Errorf("CallbackID = %q, want cbq-1", cb.CallbackID)
	}
	if cb.Data != "hitl:tok_abc123" {
		t.Errorf("Data = %q", cb.Data)
	}
	if cb.FromName != "Ada Lovelace" {
		t.Errorf("FromName = %q, want Ada Lovelace", cb.FromName)
	}
	if cb.ChatID != 12345 || cb.MessageID != 99 {
		t.Errorf("chat/message = %d/%d, want 12345/99", cb.ChatID, cb.MessageID)
	}
}

func TestParseCallback_NilForPlainMessage(t *testing.T) {
	body := []byte(`{"update_id": 1, "message": {"message_id": 1, "chat": {"id": 1, "type": "private"}, "date": 0, "text": "hi"}}`)
	if cb := ParseCallback(body); cb != nil {
		t.Errorf("expected nil for a plain message update, got %+v", cb)
	}
}
