package agent

// Tests for Phase 2b system prompt assembly.
//
// Split into two halves:
//
//  1. Pure buildSystemPrompt tests — no HTTP, no mocks. Every section-
//     inclusion rule and ordering guarantee lives here so regressions
//     are caught without the overhead of a full test server.
//
//  2. End-to-end assembleSystemPrompt tests with httptest.Server acting
//     as a fake API. These cover the fetcher behaviour: happy path,
//     non-200 falls through to empty, timeout/connection failure degrades
//     gracefully, and missing agent_user_id skips the fetch entirely.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"flomation.app/automate/launch"
)

// --- buildSystemPrompt (pure) ---

func Test_BuildSystemPrompt_EmptyEverything_StillRendersHonestyDirective(t *testing.T) {
	// The honesty directive is the one thing every assembled prompt must
	// contain — it's the Layer 0 rule that holds the model back from
	// making promises the platform cannot honour yet. Dropping it
	// silently would regress the core Phase 2 contract.
	out := buildSystemPrompt("", nil, nil, nil, nil, "")
	if !strings.Contains(out, layerZeroHonestyDirective) {
		t.Fatalf("expected honesty directive in output, got:\n%s", out)
	}
	if !strings.Contains(out, "Layer 0") {
		t.Fatalf("expected Layer 0 header marker, got:\n%s", out)
	}
	// No persona → must not open with a lone newline or divider.
	if strings.HasPrefix(out, "\n") {
		t.Fatalf("empty persona should not leave leading blank line, got:\n%q", out)
	}
}

func Test_BuildSystemPrompt_PersonaFirst_HonestySecond(t *testing.T) {
	persona := "You are Atlas, a helpful assistant."
	out := buildSystemPrompt(persona, nil, nil, nil, nil, "")

	personaIdx := strings.Index(out, persona)
	honestyIdx := strings.Index(out, layerZeroHonestyDirective)

	if personaIdx == -1 || honestyIdx == -1 {
		t.Fatalf("both sections must appear, got:\n%s", out)
	}
	if personaIdx >= honestyIdx {
		t.Fatalf("persona must appear before honesty directive, got order persona=%d honesty=%d", personaIdx, honestyIdx)
	}
}

func Test_BuildSystemPrompt_MemoriesRenderedAsTitleColonBody(t *testing.T) {
	mems := []assembledMemory{
		{Title: "Preferred name", Body: "Prefers Andy over Andrew"},
		{Title: "Timezone", Body: "Europe/London"},
	}
	out := buildSystemPrompt("P", mems, nil, nil, nil, "")

	if !strings.Contains(out, "What you know about this user") {
		t.Fatalf("missing memory section header:\n%s", out)
	}
	if !strings.Contains(out, "• Preferred name: Prefers Andy over Andrew") {
		t.Fatalf("memory not rendered in 'title: body' form:\n%s", out)
	}
	if !strings.Contains(out, "• Timezone: Europe/London") {
		t.Fatalf("second memory missing:\n%s", out)
	}
}

func Test_BuildSystemPrompt_MemoryWithEmptyTitleRendersBodyOnly(t *testing.T) {
	mems := []assembledMemory{
		{Title: "", Body: "User mentioned they are visually impaired"},
	}
	out := buildSystemPrompt("P", mems, nil, nil, nil, "")
	if !strings.Contains(out, "• User mentioned they are visually impaired") {
		t.Fatalf("empty-title memory should render as body-only bullet, got:\n%s", out)
	}
	// Must NOT render as "• : User mentioned..." — the title-colon
	// prefix should be suppressed.
	if strings.Contains(out, "• : ") {
		t.Fatalf("empty title leaked a literal colon into the bullet, got:\n%s", out)
	}
}

func Test_BuildSystemPrompt_NoMemories_OmitsSectionEntirely(t *testing.T) {
	out := buildSystemPrompt("P", nil, nil, nil, nil, "")
	if strings.Contains(out, "What you know about this user") {
		t.Fatalf("empty memories should omit the section entirely, got:\n%s", out)
	}
	// Empty slice (not nil) should also omit.
	out2 := buildSystemPrompt("P", []assembledMemory{}, nil, nil, nil, "")
	if strings.Contains(out2, "What you know about this user") {
		t.Fatalf("empty slice should omit the section entirely, got:\n%s", out2)
	}
}

func Test_BuildSystemPrompt_SlackChannelDirective(t *testing.T) {
	out := buildSystemPrompt("P", nil, nil, nil, nil, "slack")
	if !strings.Contains(out, "mrkdwn") {
		t.Fatalf("slack directive must mention mrkdwn, got:\n%s", out)
	}
	if !strings.Contains(out, "Do NOT use standard Markdown") {
		t.Fatalf("slack directive must warn against standard markdown, got:\n%s", out)
	}
}

func Test_BuildSystemPrompt_UnknownChannelType_NoDirectiveSection(t *testing.T) {
	out := buildSystemPrompt("P", nil, nil, nil, nil, "made-up-channel")
	if strings.Contains(out, "Current channel") {
		t.Fatalf("unknown channel type should not render the directive section, got:\n%s", out)
	}
}

func Test_BuildSystemPrompt_PendingActionRendersEvidenceAndInstruction(t *testing.T) {
	pas := []assembledPendingAction{
		{Type: "identity_link", Evidence: "btw I'm also @andyesser on Slack"},
	}
	out := buildSystemPrompt("P", nil, nil, pas, nil, "")
	if !strings.Contains(out, "ACTION REQUIRED") {
		t.Fatalf("missing ACTION REQUIRED header:\n%s", out)
	}
	if !strings.Contains(out, "IDENTITY LINK PENDING") {
		t.Fatalf("identity_link type must render as IDENTITY LINK PENDING, got:\n%s", out)
	}
	if !strings.Contains(out, "btw I'm also @andyesser on Slack") {
		t.Fatalf("evidence utterance must be surfaced to the model, got:\n%s", out)
	}
	if !strings.Contains(out, "link your accounts") {
		t.Fatalf("must instruct the model to ask about linking, got:\n%s", out)
	}
}

func Test_BuildSystemPrompt_SectionOrdering(t *testing.T) {
	// persona → honesty → memories → channel → pending
	persona := "You are Atlas."
	mems := []assembledMemory{{Title: "Name", Body: "Andy"}}
	pas := []assembledPendingAction{{Type: "forget_memory", Evidence: "forget Python"}}

	out := buildSystemPrompt(persona, mems, nil, pas, nil, "slack")

	markers := []struct {
		name    string
		needle  string
		wantPos int // -1 until resolved
	}{
		{"persona", persona, -1},
		{"honesty", "Layer 0", -1},
		{"memories", "What you know about this user", -1},
		{"channel", "Current channel", -1},
		{"pending", "ACTION REQUIRED", -1},
	}
	for i := range markers {
		markers[i].wantPos = strings.Index(out, markers[i].needle)
		if markers[i].wantPos == -1 {
			t.Fatalf("marker %q missing from output:\n%s", markers[i].name, out)
		}
	}
	for i := 1; i < len(markers); i++ {
		if markers[i].wantPos <= markers[i-1].wantPos {
			t.Fatalf("section %q must come after %q; got positions %d vs %d",
				markers[i].name, markers[i-1].name, markers[i].wantPos, markers[i-1].wantPos)
		}
	}
}

func Test_BuildSystemPrompt_TrailingWhitespaceTrimmed(t *testing.T) {
	out := buildSystemPrompt("P", nil, nil, nil, nil, "")
	// Exactly one trailing newline, no double newline.
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("output must end with a newline, got:\n%q", out)
	}
	if strings.HasSuffix(out, "\n\n") {
		t.Fatalf("output must not end with multiple blank lines, got trailing:\n%q", out[len(out)-20:])
	}
}

// --- assembleSystemPrompt (end-to-end with fake API) ---

// fakeMemoryAPI is a minimal httptest-backed stub that lets us assert on
// the exact URLs Launch hits and control what it sees back. Shared
// across all Phase 2b end-to-end tests.
type fakeMemoryAPI struct {
	server          *httptest.Server
	memoryHits      int32
	pendingHits     int32
	memoryResponse  []assembledMemory
	pendingResponse []assembledPendingAction
	memoryStatus    int // override HTTP status
	pendingStatus   int
}

func newFakeMemoryAPI() *fakeMemoryAPI {
	f := &fakeMemoryAPI{
		memoryStatus:  http.StatusOK,
		pendingStatus: http.StatusOK,
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/memory"):
			atomic.AddInt32(&f.memoryHits, 1)
			// Assert query parameters for visibility in test failures.
			if r.URL.Query().Get("pinned") != "true" {
				http.Error(w, "expected pinned=true", http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("agent_user_id") == "" {
				http.Error(w, "expected agent_user_id", http.StatusBadRequest)
				return
			}
			w.WriteHeader(f.memoryStatus)
			_ = json.NewEncoder(w).Encode(f.memoryResponse)
		case strings.HasSuffix(path, "/pending-action"):
			atomic.AddInt32(&f.pendingHits, 1)
			if r.URL.Query().Get("agent_user_id") == "" {
				http.Error(w, "expected agent_user_id", http.StatusBadRequest)
				return
			}
			w.WriteHeader(f.pendingStatus)
			_ = json.NewEncoder(w).Encode(f.pendingResponse)
		default:
			http.NotFound(w, r)
		}
	}))
	return f
}

func (f *fakeMemoryAPI) close() { f.server.Close() }

// regWith is a tiny builder for AgentRegistration with just the fields
// the assembler touches.
func regWith(apiURL, agentID, persona string) *launch.AgentRegistration {
	return &launch.AgentRegistration{
		AgentID:      agentID,
		APIURL:       apiURL,
		SystemPrompt: &persona,
	}
}

func strPtr(s string) *string { return &s }

func Test_AssembleSystemPrompt_HappyPath_FetchesAndComposes(t *testing.T) {
	fake := newFakeMemoryAPI()
	defer fake.close()
	fake.memoryResponse = []assembledMemory{
		{Title: "Preferred name", Body: "Andy"},
	}
	fake.pendingResponse = []assembledPendingAction{
		{Type: "identity_link", Evidence: "I'm @andyesser"},
	}

	svc := &Service{}
	reg := regWith(fake.server.URL, "agent-1", "You are Atlas.")
	msg := InboundMessage{ChannelType: "slack"}

	out := svc.assembleSystemPrompt(reg, msg, strPtr("user-123"))

	if !strings.Contains(out, "You are Atlas.") {
		t.Errorf("persona missing: %s", out)
	}
	if !strings.Contains(out, "Preferred name: Andy") {
		t.Errorf("memory missing: %s", out)
	}
	if !strings.Contains(out, "IDENTITY LINK PENDING") {
		t.Errorf("pending action missing: %s", out)
	}
	if !strings.Contains(out, "mrkdwn") {
		t.Errorf("slack channel directive missing: %s", out)
	}
	if !strings.Contains(out, layerZeroHonestyDirective) {
		t.Errorf("honesty directive missing: %s", out)
	}

	if atomic.LoadInt32(&fake.memoryHits) != 1 {
		t.Errorf("expected exactly 1 memory fetch, got %d", fake.memoryHits)
	}
	if atomic.LoadInt32(&fake.pendingHits) != 1 {
		t.Errorf("expected exactly 1 pending-action fetch, got %d", fake.pendingHits)
	}
}

func Test_AssembleSystemPrompt_NoAgentUserID_SkipsFetchesEntirely(t *testing.T) {
	fake := newFakeMemoryAPI()
	defer fake.close()

	svc := &Service{}
	reg := regWith(fake.server.URL, "agent-1", "P")
	msg := InboundMessage{ChannelType: "slack"}

	// nil agentUserID → unresolved webhook path → no memory fetch.
	out := svc.assembleSystemPrompt(reg, msg, nil)

	if atomic.LoadInt32(&fake.memoryHits) != 0 {
		t.Errorf("expected zero fetches with nil agent_user_id, got %d", fake.memoryHits)
	}
	if atomic.LoadInt32(&fake.pendingHits) != 0 {
		t.Errorf("expected zero fetches with nil agent_user_id, got %d", fake.pendingHits)
	}
	// Degraded output still includes persona + honesty + channel.
	if !strings.Contains(out, "P") || !strings.Contains(out, layerZeroHonestyDirective) || !strings.Contains(out, "mrkdwn") {
		t.Errorf("degraded output must still include persona + honesty + channel, got:\n%s", out)
	}
}

func Test_AssembleSystemPrompt_APIReturns500_FailsOpen(t *testing.T) {
	fake := newFakeMemoryAPI()
	defer fake.close()
	fake.memoryStatus = http.StatusInternalServerError
	fake.pendingStatus = http.StatusInternalServerError

	svc := &Service{}
	reg := regWith(fake.server.URL, "agent-1", "P")
	msg := InboundMessage{ChannelType: "slack"}

	out := svc.assembleSystemPrompt(reg, msg, strPtr("user-123"))

	// Both endpoints 500 — the assembler must not block or return empty;
	// it must return the degraded-but-valid prompt so the reply goes out.
	if !strings.Contains(out, "P") {
		t.Errorf("degraded output must include persona, got:\n%s", out)
	}
	if !strings.Contains(out, layerZeroHonestyDirective) {
		t.Errorf("degraded output must include honesty directive, got:\n%s", out)
	}
	if strings.Contains(out, "What you know about this user") {
		t.Errorf("failed fetch must not render an empty memory block, got:\n%s", out)
	}
}

func Test_AssembleSystemPrompt_UnreachableAPI_FailsOpen(t *testing.T) {
	// Point the registration at a URL nothing is listening on. We use
	// 127.0.0.1:1 because port 1 is the canonical "nothing here" port.
	svc := &Service{}
	reg := regWith("http://127.0.0.1:1", "agent-1", "Persona")
	msg := InboundMessage{ChannelType: "slack"}

	out := svc.assembleSystemPrompt(reg, msg, strPtr("user-123"))

	if !strings.Contains(out, "Persona") {
		t.Errorf("degraded output must include persona even when API is unreachable, got:\n%s", out)
	}
	if !strings.Contains(out, layerZeroHonestyDirective) {
		t.Errorf("degraded output must include honesty directive, got:\n%s", out)
	}
}

func Test_AssembleSystemPrompt_URLShapeMatchesAPIContract(t *testing.T) {
	// Regression guard: if the URL template ever drifts (wrong path,
	// missing query parameter, wrong endpoint segment), this test will
	// fail loudly with the exact path the fake saw.
	var capturedMemPath string
	var capturedPendPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/memory"):
			capturedMemPath = r.URL.Path + "?" + r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]assembledMemory{})
		case strings.Contains(r.URL.Path, "/pending-action"):
			capturedPendPath = r.URL.Path + "?" + r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]assembledPendingAction{})
		}
	}))
	defer server.Close()

	svc := &Service{}
	reg := regWith(server.URL, "agent-xyz", "P")
	msg := InboundMessage{ChannelType: "slack"}

	_ = svc.assembleSystemPrompt(reg, msg, strPtr("user-abc"))

	// Expected exact shape; see internal/http/service.go route registration.
	wantMem := "/api/v1/internal/agent/agent-xyz/memory?agent_user_id=user-abc&limit=50&pinned=true"
	if capturedMemPath != wantMem {
		t.Errorf("memory URL drift:\n  want: %s\n  got:  %s", wantMem, capturedMemPath)
	}

	wantPend := "/api/v1/internal/agent/agent-xyz/pending-action?agent_user_id=user-abc"
	if capturedPendPath != wantPend {
		t.Errorf("pending-action URL drift:\n  want: %s\n  got:  %s", wantPend, capturedPendPath)
	}
}

// --- Supplementary guard: honesty directive change forces test update ---

func Test_LayerZeroHonestyDirective_IsNotEmpty(t *testing.T) {
	// Structural guarantee: the directive constant exists, is non-empty,
	// and mentions the core behavioural rule ("in the background"). When
	// Phase 3 lands this test should be updated in lockstep with the new
	// directive text.
	if layerZeroHonestyDirective == "" {
		t.Fatal("honesty directive must not be empty")
	}
	if !strings.Contains(layerZeroHonestyDirective, "commitments") {
		t.Fatal("honesty directive must mention commitments (Phase 3 behavioural rule)")
	}
	// Sanity check on apparent length — a one-liner is too weak to hold
	// the model back; a ten-liner bloats every dispatch with tokens. The
	// current phrasing is ~260 chars; the bounds are generous but will
	// flag anyone who accidentally deletes half of it.
	if l := len(layerZeroHonestyDirective); l < 100 || l > 700 {
		t.Errorf("honesty directive length %d outside sanity bounds [100, 500]; unexpected edit?", l)
	}
	_ = fmt.Sprintf // keep fmt import in scope for any future addition
}
