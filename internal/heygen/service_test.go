package heygen

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifyAndParse_SecretModes(t *testing.T) {
	body := `{"event_type":"avatar_video.success"}`

	// query param
	r := httptest.NewRequest("POST", "/webhook/abc?secret=s3cr3t", strings.NewReader(body))
	if _, err := VerifyAndParse([]byte(body), r, "s3cr3t"); err != nil {
		t.Fatalf("query secret should verify: %v", err)
	}

	// header fallback
	r = httptest.NewRequest("POST", "/webhook/abc", strings.NewReader(body))
	r.Header.Set(SecretHeader, "tok")
	if _, err := VerifyAndParse([]byte(body), r, "tok"); err != nil {
		t.Fatalf("header secret should verify: %v", err)
	}

	// mismatch, absent presented, and no-secret-configured all fail
	r = httptest.NewRequest("POST", "/webhook/abc?secret=wrong", strings.NewReader(body))
	if _, err := VerifyAndParse([]byte(body), r, "right"); err == nil {
		t.Fatal("mismatched secret must fail")
	}
	r = httptest.NewRequest("POST", "/webhook/abc", strings.NewReader(body))
	if _, err := VerifyAndParse([]byte(body), r, "right"); err == nil {
		t.Fatal("absent presented secret must fail")
	}
	r = httptest.NewRequest("POST", "/webhook/abc?secret=x", strings.NewReader(body))
	if _, err := VerifyAndParse([]byte(body), r, ""); err == nil {
		t.Fatal("empty configured secret must fail")
	}
}

func TestEventToData_LiftsEventData(t *testing.T) {
	payload := map[string]interface{}{
		"event_type": "avatar_video.success",
		"event_data": map[string]interface{}{
			"video_id":    "vid_1",
			"url":         "https://files.heygen.ai/out.mp4",
			"callback_id": "cb_9",
		},
	}
	data := EventToData(payload, []byte(`{"raw":true}`))

	if data["event_type"] != "avatar_video.success" {
		t.Fatalf("event_type: %v", data["event_type"])
	}
	if data["video_id"] != "vid_1" {
		t.Fatalf("video_id not lifted from event_data: %v", data["video_id"])
	}
	if data["video_url"] != "https://files.heygen.ai/out.mp4" {
		t.Fatalf("video_url (from url) not lifted: %v", data["video_url"])
	}
	if data["callback_id"] != "cb_9" {
		t.Fatalf("callback_id not lifted: %v", data["callback_id"])
	}
	if data["content"] != "HeyGen event: avatar_video.success" {
		t.Fatalf("content: %v", data["content"])
	}
	if data["body"] != `{"raw":true}` {
		t.Fatalf("body not preserved: %v", data["body"])
	}
}

func TestEventToData_TranslationIDFallback(t *testing.T) {
	payload := map[string]interface{}{
		"event_type": "video_translate.success",
		"event_data": map[string]interface{}{"video_translate_id": "tr_7", "url": "https://x/dub.mp4"},
	}
	data := EventToData(payload, nil)
	if data["video_id"] != "tr_7" {
		t.Fatalf("video_translate_id should fall back to video_id: %v", data["video_id"])
	}
}

func TestMatchesFilter(t *testing.T) {
	if !MatchesFilter("avatar_video.success", "") {
		t.Fatal("empty filter matches all")
	}
	if !MatchesFilter("avatar_video.success", "avatar_video.fail, avatar_video.success") {
		t.Fatal("should match a listed type")
	}
	if MatchesFilter("avatar_video.success", "avatar_video.fail") {
		t.Fatal("should not match an unlisted type")
	}
}
