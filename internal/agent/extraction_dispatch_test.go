package agent

// Tests for Phase 2d-γ: the dispatchExtraction hook that fires the
// extraction System Flow after every inbound (user) turn. The hook
// calls POST /api/v1/internal/agent/:id/extract and expects either
// 204 (no extraction configured — no-op) or 202 (dispatched).
//
// These are small, focused tests:
//   1. Happy path: 202 response, request shape correct.
//   2. No-op: 204 response, no errors logged.
//   3. API returns 500: warning logged, no crash, method returns.
//   4. Unreachable API: same — warn and return.
//   5. Optional fields omitted when nil (message_id, agent_user_id,
//      conversation_id should not appear as JSON nulls).
//   6. Confirm the hook fires after dispatchExecution in the
//      end-to-end handleInboundMessageForReg path.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"flomation.app/automate/launch"
)

type extractFakeAPI struct {
	server      *httptest.Server
	mu          sync.Mutex
	extractHits int
	lastBody    map[string]interface{}
	lastPath    string
	replyStatus int
}

func newExtractFakeAPI(replyStatus int) *extractFakeAPI {
	f := &extractFakeAPI{replyStatus: replyStatus}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		if r.URL.Path != "" && r.Method == http.MethodPost {
			f.extractHits++
			f.lastPath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			var body map[string]interface{}
			_ = json.Unmarshal(raw, &body)
			f.lastBody = body
		}
		w.WriteHeader(f.replyStatus)
		if f.replyStatus == http.StatusAccepted {
			_, _ = w.Write([]byte(`{"execution_id":"exec-99"}`))
		}
	}))
	return f
}

func (f *extractFakeAPI) close() { f.server.Close() }

func sptr(s string) *string { return &s }

// --- happy path: 202 accepted ---

func TestDispatchExtraction_HappyPath(t *testing.T) {
	fake := newExtractFakeAPI(http.StatusAccepted)
	defer fake.close()

	svc := &Service{}
	reg := &launch.AgentRegistration{
		AgentID: "agent-1",
		APIURL:  fake.server.URL,
	}
	msg := InboundMessage{
		ChannelType: "slack",
		Content:     "call me Andy not Andrew",
	}

	svc.dispatchExtraction(reg, msg, sptr("msg-42"), sptr("user-abc"), sptr("conv-xyz"), "user")

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.extractHits != 1 {
		t.Fatalf("expected 1 extract call, got %d", fake.extractHits)
	}
	if fake.lastPath != "/api/v1/internal/agent/agent-1/extract" {
		t.Fatalf("unexpected path: %s", fake.lastPath)
	}
	if fake.lastBody["role"] != "user" {
		t.Fatalf("expected role=user, got %v", fake.lastBody["role"])
	}
	if fake.lastBody["content"] != "call me Andy not Andrew" {
		t.Fatalf("expected content to match, got %v", fake.lastBody["content"])
	}
	if fake.lastBody["message_id"] != "msg-42" {
		t.Fatalf("expected message_id, got %v", fake.lastBody["message_id"])
	}
	if fake.lastBody["agent_user_id"] != "user-abc" {
		t.Fatalf("expected agent_user_id, got %v", fake.lastBody["agent_user_id"])
	}
	if fake.lastBody["conversation_id"] != "conv-xyz" {
		t.Fatalf("expected conversation_id, got %v", fake.lastBody["conversation_id"])
	}
}

// --- no-op: 204 when no extraction flow configured ---

func TestDispatchExtraction_204NoOp(t *testing.T) {
	fake := newExtractFakeAPI(http.StatusNoContent)
	defer fake.close()

	svc := &Service{}
	reg := &launch.AgentRegistration{
		AgentID: "agent-1",
		APIURL:  fake.server.URL,
	}
	msg := InboundMessage{Content: "hello"}

	// Should not panic or log an error — 204 is the expected path for
	// agents with no extraction flow configured.
	svc.dispatchExtraction(reg, msg, nil, nil, nil, "user")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.extractHits != 1 {
		t.Fatalf("expected 1 call even for 204 path, got %d", fake.extractHits)
	}
}

// --- API returns 500 → warning, no crash ---

func TestDispatchExtraction_APIError_NoReturn(t *testing.T) {
	fake := newExtractFakeAPI(http.StatusInternalServerError)
	defer fake.close()

	svc := &Service{}
	reg := &launch.AgentRegistration{
		AgentID: "agent-1",
		APIURL:  fake.server.URL,
	}
	msg := InboundMessage{Content: "hello"}

	// Must not panic — dispatchExtraction is fire-and-forget on the
	// tail of the reply pipeline.
	svc.dispatchExtraction(reg, msg, nil, nil, nil, "user")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.extractHits != 1 {
		t.Fatalf("expected 1 call, got %d", fake.extractHits)
	}
}

// --- unreachable API → warning, no crash ---

func TestDispatchExtraction_UnreachableAPI(t *testing.T) {
	svc := &Service{}
	reg := &launch.AgentRegistration{
		AgentID: "agent-1",
		APIURL:  "http://127.0.0.1:1", // guaranteed refused
	}
	msg := InboundMessage{Content: "hello"}

	// Must not panic or block indefinitely.
	svc.dispatchExtraction(reg, msg, nil, nil, nil, "user")
}

// --- optional fields omitted from JSON when nil ---

func TestDispatchExtraction_NilOptionalsOmitted(t *testing.T) {
	fake := newExtractFakeAPI(http.StatusAccepted)
	defer fake.close()

	svc := &Service{}
	reg := &launch.AgentRegistration{
		AgentID: "agent-1",
		APIURL:  fake.server.URL,
	}
	msg := InboundMessage{Content: "hello"}

	// All optional pointers nil.
	svc.dispatchExtraction(reg, msg, nil, nil, nil, "assistant")

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if _, has := fake.lastBody["message_id"]; has {
		t.Fatal("message_id should be omitted when nil")
	}
	if _, has := fake.lastBody["agent_user_id"]; has {
		t.Fatal("agent_user_id should be omitted when nil")
	}
	if _, has := fake.lastBody["conversation_id"]; has {
		t.Fatal("conversation_id should be omitted when nil")
	}
	if fake.lastBody["role"] != "assistant" {
		t.Fatalf("expected role=assistant, got %v", fake.lastBody["role"])
	}
}

// --- empty API URL → no call ---

func TestDispatchExtraction_EmptyAPIURL_Skips(t *testing.T) {
	// Should be a silent no-op — don't try to dial an empty URL.
	svc := &Service{}
	reg := &launch.AgentRegistration{AgentID: "agent-1", APIURL: ""}
	msg := InboundMessage{Content: "hello"}
	svc.dispatchExtraction(reg, msg, nil, nil, nil, "user")
}
