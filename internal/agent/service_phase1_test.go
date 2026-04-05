package agent

// Tests for Phase 1 of the Agent Memory feature: identity resolution,
// conversation resolution, pre-store history fetch, and the new
// trigger data keys plumbed through dispatchExecution.
//
// We stub the API with httptest.Server and drive the Service's
// lower-level methods directly — the DB-backed registration lookup
// and lease machinery is orthogonal to the flow being tested here.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"flomation.app/automate/launch"
)

// recordedRequest captures what the fake API saw for later assertions.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]interface{}
}

// fakeAPI spins up an httptest.Server that handles every internal
// endpoint the agent service calls during inbound message handling.
// The responses are fixed but the requests are recorded.
type fakeAPI struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest

	identityUserID  string
	conversationID  string
	historyMessages []map[string]interface{}
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{
		t:               t,
		identityUserID:  "user-123",
		conversationID:  "conv-abc",
		historyMessages: []map[string]interface{}{{"direction": "inbound", "content": "hello earlier"}},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeAPI) close() {
	f.server.Close()
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
	if r.Body != nil {
		body, _ := io.ReadAll(r.Body)
		if len(body) > 0 {
			_ = json.Unmarshal(body, &rec.Body)
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	f.mu.Unlock()

	switch {
	case strings.HasSuffix(r.URL.Path, "/resolve-identity"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"identity": map[string]interface{}{"id": "ident-1"},
			"user":     map[string]interface{}{"id": f.identityUserID},
		})
	case strings.HasSuffix(r.URL.Path, "/conversation"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": f.conversationID,
		})
	case strings.Contains(r.URL.Path, "/conversation/") && strings.HasSuffix(r.URL.Path, "/history"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.historyMessages)
	case strings.Contains(r.URL.Path, "/conversation/") && strings.HasSuffix(r.URL.Path, "/message"):
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "msg-new"})
	case strings.HasSuffix(r.URL.Path, "/message"):
		// Legacy agent-wide fallback endpoint
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "msg-legacy"})
	case strings.Contains(r.URL.Path, "/execute"):
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeAPI) findRequest(pathSuffix string) *recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.requests {
		if strings.HasSuffix(f.requests[i].Path, pathSuffix) {
			return &f.requests[i]
		}
	}
	return nil
}

// ---------- helper derivation tests (pure functions, no API) ----------

func TestDeriveExternalID_Slack(t *testing.T) {
	msg := InboundMessage{
		ChannelType: "slack",
		Sender:      "Andy Esser",
		Metadata: map[string]interface{}{
			"user_id":      "U0ABC123",
			"user_name":    "Andy Esser",
			"display_name": "andy",
		},
	}
	id, name := deriveExternalID(msg)
	if id != "U0ABC123" {
		t.Fatalf("expected U0ABC123, got %q", id)
	}
	if name != "Andy Esser" {
		t.Fatalf("expected display name from user_name, got %q", name)
	}
}

func TestDeriveExternalID_Telegram(t *testing.T) {
	msg := InboundMessage{
		ChannelType: "telegram",
		Sender:      "@andy",
		Metadata: map[string]interface{}{
			"sender_id":       "12345",
			"sender_name":     "Andy",
			"sender_username": "andy",
		},
	}
	id, name := deriveExternalID(msg)
	if id != "12345" {
		t.Fatalf("expected stable numeric id, got %q", id)
	}
	if name != "Andy" {
		t.Fatalf("expected sender_name, got %q", name)
	}
}

func TestDeriveExternalID_UnknownChannel_FallsBackToSender(t *testing.T) {
	msg := InboundMessage{
		ChannelType: "webhook",
		Sender:      "1.2.3.4",
		Metadata:    map[string]interface{}{},
	}
	id, name := deriveExternalID(msg)
	if id != "1.2.3.4" || name != "1.2.3.4" {
		t.Fatalf("expected webhook fallback to sender, got id=%q name=%q", id, name)
	}
}

func TestDeriveChannelScope_SlackThread(t *testing.T) {
	msg := InboundMessage{
		ChannelType: "slack",
		Metadata: map[string]interface{}{
			"channel_id": "C0GEN",
			"thread_ts":  "1712160000.000100",
		},
	}
	ch, thread := deriveChannelScope(msg)
	if ch != "C0GEN" {
		t.Fatalf("channel: %q", ch)
	}
	if thread == nil || *thread != "1712160000.000100" {
		t.Fatalf("thread: %v", thread)
	}
}

func TestDeriveChannelScope_SlackNoThread(t *testing.T) {
	msg := InboundMessage{
		ChannelType: "slack",
		Metadata:    map[string]interface{}{"channel_id": "C0GEN"},
	}
	ch, thread := deriveChannelScope(msg)
	if ch != "C0GEN" || thread != nil {
		t.Fatalf("unexpected: ch=%q thread=%v", ch, thread)
	}
}

func TestDeriveChannelScope_Webhook_EmptyScope(t *testing.T) {
	msg := InboundMessage{
		ChannelType: "webhook",
		Metadata:    map[string]interface{}{"some": "data"},
	}
	ch, thread := deriveChannelScope(msg)
	if ch != "" || thread != nil {
		t.Fatalf("webhook scope should be empty, got ch=%q thread=%v", ch, thread)
	}
}

// ---------- end-to-end dispatch tests (real http calls to fakeAPI) ----------

func newTestReg(apiURL string) *launch.AgentRegistration {
	flow := "flow-xyz"
	sp := "You are a helpful assistant."
	return &launch.AgentRegistration{
		AgentID:            "agent-1",
		OrchestratorFlowID: &flow,
		SystemPrompt:       &sp,
		APIURL:             apiURL,
	}
}

func TestResolveIdentity_SendsStableIDAndDisplayName(t *testing.T) {
	api := newFakeAPI(t)
	defer api.close()

	svc := &Service{}
	reg := newTestReg(api.server.URL)
	msg := InboundMessage{
		ChannelType: "slack",
		Sender:      "Andy Esser",
		Metadata: map[string]interface{}{
			"user_id":   "U0ABC123",
			"user_name": "Andy Esser",
		},
	}

	id, err := svc.resolveIdentity(reg, msg)
	if err != nil {
		t.Fatalf("resolveIdentity: %v", err)
	}
	if id.User.ID != "user-123" {
		t.Fatalf("unexpected user id: %s", id.User.ID)
	}

	req := api.findRequest("/resolve-identity")
	if req == nil {
		t.Fatal("expected resolve-identity request")
	}
	if req.Body["channel_external_id"] != "U0ABC123" {
		t.Fatalf("expected U-id as external id, got %v", req.Body["channel_external_id"])
	}
	if req.Body["channel_type"] != "slack" {
		t.Fatalf("expected slack channel_type")
	}
	if req.Body["display_name"] != "Andy Esser" {
		t.Fatalf("expected display name passed through")
	}
}

func TestResolveConversation_PassesThreadAndUser(t *testing.T) {
	api := newFakeAPI(t)
	defer api.close()

	svc := &Service{}
	reg := newTestReg(api.server.URL)
	userID := "user-123"
	thread := "1712160000.000100"

	conv, err := svc.resolveConversation(reg, &userID, "slack", "C0GEN", &thread)
	if err != nil {
		t.Fatalf("resolveConversation: %v", err)
	}
	if conv.ID != "conv-abc" {
		t.Fatalf("unexpected conversation id: %s", conv.ID)
	}

	req := api.findRequest("/conversation")
	if req == nil {
		t.Fatal("expected resolve-conversation request")
	}
	if req.Body["agent_user_id"] != "user-123" ||
		req.Body["channel_id"] != "C0GEN" ||
		req.Body["thread_id"] != thread {
		t.Fatalf("unexpected body: %+v", req.Body)
	}
}

func TestFetchConversationHistory_ReturnsDecodedMessages(t *testing.T) {
	api := newFakeAPI(t)
	defer api.close()

	svc := &Service{}
	reg := newTestReg(api.server.URL)

	history := svc.fetchConversationHistory(reg, "conv-abc", 20)
	if len(history) != 1 {
		t.Fatalf("expected 1 history message, got %d", len(history))
	}
	if history[0]["content"] != "hello earlier" {
		t.Fatalf("unexpected content: %v", history[0]["content"])
	}

	req := api.findRequest("/history")
	if req == nil {
		t.Fatal("expected history request")
	}
	if !strings.Contains(req.Query, "limit=20") {
		t.Fatalf("expected limit=20 in query, got %q", req.Query)
	}
}

func TestStoreMessage_WithConversationID_UsesScopedEndpoint(t *testing.T) {
	api := newFakeAPI(t)
	defer api.close()

	svc := &Service{}
	reg := newTestReg(api.server.URL)
	convID := "conv-abc"
	msg := InboundMessage{
		ChannelType: "slack",
		Sender:      "Andy",
		Content:     "hi",
	}

	id, err := svc.storeMessage(reg, msg, &convID)
	if err != nil {
		t.Fatalf("storeMessage: %v", err)
	}
	if id == nil || *id != "msg-new" {
		t.Fatalf("unexpected id: %v", id)
	}

	req := api.findRequest("/message")
	if req == nil || !strings.Contains(req.Path, "/conversation/conv-abc/message") {
		t.Fatalf("expected scoped conversation endpoint, got %+v", req)
	}
	if !strings.Contains(req.Query, "agent_id=agent-1") {
		t.Fatalf("expected agent_id query param, got %q", req.Query)
	}
}

func TestStoreMessage_WithoutConversationID_FallsBackToLegacy(t *testing.T) {
	api := newFakeAPI(t)
	defer api.close()

	svc := &Service{}
	reg := newTestReg(api.server.URL)
	msg := InboundMessage{ChannelType: "webhook", Sender: "1.2.3.4", Content: "hi"}

	id, err := svc.storeMessage(reg, msg, nil)
	if err != nil {
		t.Fatalf("storeMessage: %v", err)
	}
	if id == nil || *id != "msg-legacy" {
		t.Fatalf("unexpected id: %v", id)
	}

	req := api.findRequest("/message")
	if req == nil || !strings.Contains(req.Path, "/agent/agent-1/message") {
		t.Fatalf("expected legacy endpoint, got %+v", req)
	}
}

func TestDispatchExecution_PlumbsMemoryKeysIntoTriggerData(t *testing.T) {
	api := newFakeAPI(t)
	defer api.close()

	svc := &Service{}
	reg := newTestReg(api.server.URL)

	msg := InboundMessage{
		ChannelType: "slack",
		Sender:      "Andy",
		Content:     "hi",
		Metadata:    map[string]interface{}{"channel_id": "C0GEN"},
	}
	msgID := "msg-new"
	userID := "user-123"
	convID := "conv-abc"
	history := []map[string]interface{}{{"direction": "inbound", "content": "prior"}}

	err := svc.dispatchExecution(reg, msg, &msgID, &userID, &convID, history)
	if err != nil {
		t.Fatalf("dispatchExecution: %v", err)
	}

	req := api.findRequest("/execute")
	if req == nil {
		t.Fatal("expected execute request")
	}
	if req.Body["agent_user_id"] != "user-123" {
		t.Fatalf("expected agent_user_id in trigger data, got %v", req.Body["agent_user_id"])
	}
	if req.Body["conversation_id"] != "conv-abc" {
		t.Fatalf("expected conversation_id in trigger data, got %v", req.Body["conversation_id"])
	}
	if req.Body["conversation_history"] == nil {
		t.Fatalf("expected conversation_history in trigger data")
	}
	if req.Body["system_prompt"] != "You are a helpful assistant." {
		t.Fatalf("expected system_prompt in trigger data, got %v", req.Body["system_prompt"])
	}
	if req.Body["message_id"] != "msg-new" {
		t.Fatalf("expected message_id, got %v", req.Body["message_id"])
	}
	// Metadata should flatten into top level
	if req.Body["channel_id"] != "C0GEN" {
		t.Fatalf("expected flattened channel_id, got %v", req.Body["channel_id"])
	}
}

// ---------- end-to-end orchestration tests ----------

// TestHandleInboundMessageForReg_EndToEnd_WiresValuesBetweenSteps is
// the Phase 1.7 integration test. It drives the full Phase 1 pipeline
// through handleInboundMessageForReg and asserts that values from
// each step reach the next one — specifically:
//
//  1. resolve-identity returns user-123 → that id reaches resolve-conversation
//  2. resolve-conversation returns conv-abc → that id is used for history fetch
//  3. that same conv-abc id is used for the conversation-scoped message store
//  4. all three identifiers arrive at dispatchExecution as trigger data keys
//
// The unit tests alone can't catch a bug where e.g. the conversation
// id from step 2 is discarded before step 3 — this test makes that
// wiring regression impossible to miss.
func TestHandleInboundMessageForReg_EndToEnd_WiresValuesBetweenSteps(t *testing.T) {
	api := newFakeAPI(t)
	defer api.close()

	svc := &Service{}
	reg := newTestReg(api.server.URL)
	msg := InboundMessage{
		ChannelType: "slack",
		Sender:      "Andy Esser",
		Content:     "remember I prefer tea over coffee",
		Metadata: map[string]interface{}{
			"user_id":    "U0ABC123",
			"user_name":  "Andy Esser",
			"channel_id": "C0GEN",
			"thread_ts":  "1712160000.000100",
		},
	}

	if err := svc.handleInboundMessageForReg(reg, msg); err != nil {
		t.Fatalf("handleInboundMessageForReg: %v", err)
	}

	// Assertion 1: identity was resolved with the stable Slack U-id.
	identReq := api.findRequest("/resolve-identity")
	if identReq == nil {
		t.Fatal("expected /resolve-identity call")
	}
	if identReq.Body["channel_external_id"] != "U0ABC123" {
		t.Fatalf("expected U0ABC123 as external id, got %v", identReq.Body["channel_external_id"])
	}

	// Assertion 2: resolved user-123 flowed into the conversation
	// lookup, along with the thread_ts that scopes this to a thread.
	convReq := api.findRequest("/conversation")
	if convReq == nil {
		t.Fatal("expected /conversation call")
	}
	if convReq.Body["agent_user_id"] != "user-123" {
		t.Fatalf("expected agent_user_id from identity step to reach conversation step, got %v", convReq.Body["agent_user_id"])
	}
	if convReq.Body["channel_id"] != "C0GEN" || convReq.Body["thread_id"] != "1712160000.000100" {
		t.Fatalf("expected channel/thread scope passed through, got %+v", convReq.Body)
	}

	// Assertion 3: history fetch used the conv-abc id returned by the
	// conversation step, BEFORE the inbound message was stored (the
	// conversation-loop bug this phase exists to prevent).
	historyReq := api.findRequest("/history")
	if historyReq == nil {
		t.Fatal("expected /history call")
	}
	if !strings.Contains(historyReq.Path, "/conversation/conv-abc/history") {
		t.Fatalf("expected history call scoped to conv-abc, got path %q", historyReq.Path)
	}

	// Assertion 4: the inbound message was stored into the same
	// conversation via the scoped endpoint, not the legacy fallback.
	// This is the "fetch history BEFORE store" ordering contract —
	// if the message was stored first, fetchConversationHistory would
	// see its own prompt in the returned turns.
	storeReq := api.findRequest("/message")
	if storeReq == nil {
		t.Fatal("expected /message call")
	}
	if !strings.Contains(storeReq.Path, "/conversation/conv-abc/message") {
		t.Fatalf("expected scoped message store, got path %q", storeReq.Path)
	}
	if !strings.Contains(storeReq.Query, "agent_id=agent-1") {
		t.Fatalf("expected agent_id query param on scoped store, got %q", storeReq.Query)
	}

	// Assertion 5: dispatchExecution received all three memory keys
	// in trigger data. This is the payload the runner/executor will
	// see on ExecutionContext.
	execReq := api.findRequest("/execute")
	if execReq == nil {
		t.Fatal("expected /execute call")
	}
	if execReq.Body["agent_user_id"] != "user-123" {
		t.Fatalf("agent_user_id missing from trigger data: %v", execReq.Body["agent_user_id"])
	}
	if execReq.Body["conversation_id"] != "conv-abc" {
		t.Fatalf("conversation_id missing from trigger data: %v", execReq.Body["conversation_id"])
	}
	if execReq.Body["conversation_history"] == nil {
		t.Fatalf("conversation_history missing from trigger data")
	}
	if execReq.Body["message_id"] != "msg-new" {
		t.Fatalf("expected message_id from scoped store, got %v", execReq.Body["message_id"])
	}
	if execReq.Body["system_prompt"] != "You are a helpful assistant." {
		t.Fatalf("expected system_prompt from registration, got %v", execReq.Body["system_prompt"])
	}
}

// TestHandleInboundMessageForReg_Webhook_NoChannelScope_FallsBackCleanly
// ensures a channel type with no derivable scope (generic webhook)
// skips identity+conversation resolution and falls back to the legacy
// storage path without spurious API calls. This is the "degraded but
// still working" guarantee for channels without stable identifiers.
func TestHandleInboundMessageForReg_Webhook_NoChannelScope_FallsBackCleanly(t *testing.T) {
	api := newFakeAPI(t)
	defer api.close()

	svc := &Service{}
	reg := newTestReg(api.server.URL)
	msg := InboundMessage{
		ChannelType: "webhook",
		Sender:      "1.2.3.4",
		Content:     "hi",
		Metadata:    map[string]interface{}{"random": "data"},
	}

	if err := svc.handleInboundMessageForReg(reg, msg); err != nil {
		t.Fatalf("handleInboundMessageForReg: %v", err)
	}

	// Identity *is* resolved for webhooks (falls back to sender as
	// external id) — this preserves cross-channel continuity when a
	// user later links the webhook identity to a known account.
	if api.findRequest("/resolve-identity") == nil {
		t.Fatal("expected /resolve-identity even for webhook")
	}

	// But conversation resolution is skipped because deriveChannelScope
	// returns "" for unknown channel types.
	if api.findRequest("/conversation") != nil {
		t.Fatal("webhook with no stable channel id must skip conversation resolution")
	}

	// And history fetch is skipped as a consequence.
	if api.findRequest("/history") != nil {
		t.Fatal("webhook must skip history fetch when no conversation is open")
	}

	// Message store falls back to the legacy agent-wide endpoint.
	storeReq := api.findRequest("/message")
	if storeReq == nil || !strings.Contains(storeReq.Path, "/agent/agent-1/message") {
		t.Fatalf("expected legacy store fallback, got %+v", storeReq)
	}

	// Dispatch still happens — user messages are never dropped.
	execReq := api.findRequest("/execute")
	if execReq == nil {
		t.Fatal("expected /execute even in degraded mode")
	}
	if _, ok := execReq.Body["conversation_id"]; ok {
		t.Fatal("conversation_id must be absent when no scope was resolved")
	}
}

func TestDispatchExecution_OmitsNilMemoryKeys(t *testing.T) {
	api := newFakeAPI(t)
	defer api.close()

	svc := &Service{}
	reg := newTestReg(api.server.URL)
	msg := InboundMessage{ChannelType: "webhook", Sender: "1.2.3.4", Content: "hi"}

	err := svc.dispatchExecution(reg, msg, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("dispatchExecution: %v", err)
	}

	req := api.findRequest("/execute")
	if req == nil {
		t.Fatal("expected execute request")
	}
	if _, ok := req.Body["agent_user_id"]; ok {
		t.Fatalf("agent_user_id should be absent when nil")
	}
	if _, ok := req.Body["conversation_id"]; ok {
		t.Fatalf("conversation_id should be absent when nil")
	}
	if _, ok := req.Body["conversation_history"]; ok {
		t.Fatalf("conversation_history should be absent when nil")
	}
}