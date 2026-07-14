package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"flomation.app/automate/launch"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// A fake AWX, good enough to register against.
// ---------------------------------------------------------------------------

type fakeAWX struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// calls records every request as "METHOD /path", in order.
	calls []string
	// created is the notification_configuration of the last template created.
	created map[string]interface{}
	// createdName is the `name` of the last template created.
	createdName string
	// attached / detached record the relations touched, e.g. "started".
	attached []string
	detached []string

	// knobs
	orgID           interface{} // what the job template reports as `organization`
	gateway         bool        // serve the AAP 2.5 gateway shape (/api/controller/v2/)
	failAttach      []string    // relations whose attach should 500
	nameConflict    bool        // the create returns the unique_together 400 once
	deleteStatus    int         // status for DELETE /notification_templates/{id}/
	templateDeleted bool
}

func newFakeAWX(t *testing.T) *fakeAWX {
	t.Helper()
	f := &fakeAWX{t: t, orgID: float64(1), deleteStatus: http.StatusNoContent}
	f.server = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(f.server.Close)
	return f
}

// root is the API prefix this fake serves under.
func (f *fakeAWX) root() string {
	if f.gateway {
		return "/api/controller/v2/"
	}
	return "/api/v2/"
}

func (f *fakeAWX) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	// The unauthenticated /api/ banner used to order the root sweep. The AAP 2.5
	// gateway is detected on the ABSENCE of available_versions, not on a guessed
	// gateway key — so the gateway shape omits it.
	if r.URL.Path == "/api/" {
		if f.gateway {
			writeJSON(w, 200, map[string]interface{}{"description": "Ansible Automation Platform"})
			return
		}
		writeJSON(w, 200, map[string]interface{}{
			"description":        "AWX REST API",
			"current_version":    "/api/v2/",
			"available_versions": map[string]interface{}{"v2": "/api/v2/"},
		})
		return
	}

	root := f.root()
	if !strings.HasPrefix(r.URL.Path, root) {
		// A sweep probing the wrong prefix: a 404, exactly like the real thing.
		w.WriteHeader(http.StatusNotFound)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, root)

	switch {
	case path == "me/":
		writeJSON(w, 200, map[string]interface{}{"results": []interface{}{map[string]interface{}{"id": 1, "is_superuser": true}}})

	case strings.HasPrefix(path, "job_templates/") && strings.HasSuffix(path, "/") && !strings.Contains(path, "notification_templates_"):
		body := map[string]interface{}{"id": 7, "name": "Demo Job Template"}
		if f.orgID != nil {
			body["organization"] = f.orgID
		}
		writeJSON(w, 200, body)

	case strings.Contains(path, "/notification_templates_"):
		rel := path[strings.LastIndex(path, "notification_templates_")+len("notification_templates_"):]
		rel = strings.TrimSuffix(rel, "/")

		var in map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&in)

		// ★ The id must be present and an integer. Without it AWX would CREATE a new
		// template from the body and attach that — the silent duplicate footgun.
		if _, ok := in["id"].(float64); !ok {
			f.t.Errorf("attach/detach to %q sent no numeric id: %#v", rel, in)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		f.mu.Lock()
		if dis, _ := in["disassociate"].(bool); dis {
			f.detached = append(f.detached, rel)
		} else {
			f.attached = append(f.attached, rel)
		}
		f.mu.Unlock()

		for _, bad := range f.failAttach {
			if bad == rel {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)

	case path == "notification_templates/" && r.Method == http.MethodPost:
		var in map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&in)

		if f.nameConflict {
			f.nameConflict = false
			writeJSON(w, 400, map[string]interface{}{
				"__all__": []interface{}{"Notification template with this Organization and Name already exists."},
			})
			return
		}

		f.mu.Lock()
		f.createdName, _ = in["name"].(string)
		f.created, _ = in["notification_configuration"].(map[string]interface{})
		f.mu.Unlock()
		writeJSON(w, 201, map[string]interface{}{"id": 42, "name": in["name"]})

	case strings.HasPrefix(path, "notification_templates/") && r.Method == http.MethodGet:
		// The adopt path: look up the pre-existing template by name+organization.
		writeJSON(w, 200, map[string]interface{}{
			"results": []interface{}{map[string]interface{}{"id": 99, "name": r.URL.Query().Get("name")}},
		})

	case strings.HasPrefix(path, "notification_templates/") && r.Method == http.MethodPatch:
		var in map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.created, _ = in["notification_configuration"].(map[string]interface{})
		f.mu.Unlock()
		writeJSON(w, 200, map[string]interface{}{"id": 99})

	case strings.HasPrefix(path, "notification_templates/") && r.Method == http.MethodDelete:
		f.mu.Lock()
		f.templateDeleted = true
		f.mu.Unlock()
		w.WriteHeader(f.deleteStatus)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeAWX) snapshot() ([]string, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...), append([]string(nil), f.attached...), append([]string(nil), f.detached...)
}

func (f *fakeAWX) creds(logical ...string) awxCreds {
	return awxCreds{
		base:       f.server.URL,
		method:     "token",
		token:      "awx-token-value",
		kind:       awxKindJobTemplate,
		templateID: "7",
		logical:    logical,
	}
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func TestAWXRegisterCreatesTemplateAndAttachesRelations(t *testing.T) {
	f := newFakeAWX(t)
	cr := f.creds(awxEventStarted, awxEventSuccessful, awxEventFailed)

	state, err := awxRegister(context.Background(), cr, "trig-abc", "https://launch.example.com/webhook/trig-abc")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if state.TemplateID != "42" {
		t.Errorf("template id = %q, want 42", state.TemplateID)
	}
	if state.OrgID != "1" {
		t.Errorf("org id = %q, want 1 (resolved from the job template, never hardcoded)", state.OrgID)
	}
	if state.Root != "/api/v2/" {
		t.Errorf("root = %q, want /api/v2/", state.Root)
	}
	if len(state.Secret) != 64 {
		t.Errorf("secret is %d chars, want 64 (32 bytes, hex)", len(state.Secret))
	}

	// ★ started/success/error — it is `error`, NOT `failure`.
	_, attached, _ := f.snapshot()
	if got, want := strings.Join(attached, ","), "started,success,error"; got != want {
		t.Errorf("attached relations = %q, want %q", got, want)
	}
	if !awxSameEventSet(state.Attached, []string{"started", "success", "error"}) {
		t.Errorf("state.Attached = %v", state.Attached)
	}

	// ★ The secret goes in `password` — the ONLY field AWX encrypts — and NEVER in
	// `headers`, which AWX stores and hands back in plaintext.
	if got := f.created["password"]; got != state.Secret {
		t.Errorf("password = %v, want the minted secret", got)
	}
	if got := f.created["username"]; got != awxBasicUser {
		t.Errorf("username = %v, want %q", got, awxBasicUser)
	}
	headers, _ := f.created["headers"].(map[string]interface{})
	if headers == nil {
		t.Fatal("headers must be present — AWX 400s without it (no default in init_parameters)")
	}
	for k, v := range headers {
		if s, ok := v.(string); ok && strings.Contains(s, state.Secret) {
			t.Fatalf("★ the shared secret leaked into headers[%q] — AWX stores headers in PLAINTEXT", k)
		}
	}
	if headers[awxTriggerHeader] != "trig-abc" {
		t.Errorf("headers[%s] = %v, want the trigger id", awxTriggerHeader, headers[awxTriggerHeader])
	}
	if f.created["disable_ssl_verification"] != false {
		t.Errorf("disable_ssl_verification = %v, want false (inverted semantics: false means AWX DOES verify)", f.created["disable_ssl_verification"])
	}
	if _, custom := f.created["messages"]; custom {
		t.Error("must not set a custom `messages` body — a malformed one silently POSTs {} forever")
	}
}

func TestAWXTemplateNameIsUniquePerTrigger(t *testing.T) {
	// Notification templates are org-scoped and name-unique (named_url is
	// <name>++<Org>), so two flows on one AWX collide unless the name carries the
	// trigger id.
	a := awxTemplateName("11111111-1111-1111-1111-111111111111")
	b := awxTemplateName("22222222-2222-2222-2222-222222222222")
	if a == b {
		t.Fatal("two triggers must not produce the same notification template name")
	}
	for _, id := range []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"} {
		if !strings.Contains(awxTemplateName(id), id) {
			t.Errorf("template name %q must contain the trigger id", awxTemplateName(id))
		}
	}

	// And it reaches AWX that way.
	f := newFakeAWX(t)
	if _, err := awxRegister(context.Background(), f.creds(awxEventSuccessful), "trig-xyz", "https://l/webhook/trig-xyz"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !strings.Contains(f.createdName, "trig-xyz") {
		t.Errorf("template name sent to AWX = %q, must contain the trigger id", f.createdName)
	}
}

func TestAWXRegisterCollapsesFailedAndCanceledOntoOneErrorRelation(t *testing.T) {
	// AWX has three events and `error` is a catch-all: failed, error AND canceled
	// all fire it. Picking both logical events must attach `error` exactly once.
	f := newFakeAWX(t)
	state, err := awxRegister(context.Background(), f.creds(awxEventFailed, awxEventCanceled), "trig-1", "https://l/webhook/trig-1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	_, attached, _ := f.snapshot()
	if len(attached) != 1 || attached[0] != "error" {
		t.Fatalf("attached = %v, want exactly one `error` relation", attached)
	}
	// Both logical events are still remembered — the inbound filter needs them to
	// tell a failure from a cancellation.
	if !awxSameEventSet(state.Logical, []string{awxEventFailed, awxEventCanceled}) {
		t.Errorf("state.Logical = %v, want both failed and canceled", state.Logical)
	}
}

func TestAWXRegisterPartialAttachPersistsOnlyWhatAttached(t *testing.T) {
	// A partial registration must record what ACTUALLY attached, so the next save's
	// change-check sees the shortfall and rebuilds.
	f := newFakeAWX(t)
	f.failAttach = []string{"success"}

	state, err := awxRegister(context.Background(), f.creds(awxEventStarted, awxEventSuccessful), "trig-1", "https://l/webhook/trig-1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !awxSameEventSet(state.Attached, []string{"started"}) {
		t.Fatalf("state.Attached = %v, want only the relation that succeeded", state.Attached)
	}

	// And the change-check must then report the state as stale.
	if awxStateCurrent(state, f.creds(awxEventStarted, awxEventSuccessful)) {
		t.Error("a partial registration must not be treated as current — it would never self-heal")
	}
}

func TestAWXRegisterFailsWhenNothingAttaches(t *testing.T) {
	f := newFakeAWX(t)
	f.failAttach = []string{"started", "success", "error"}

	if _, err := awxRegister(context.Background(), f.creds(awxEventSuccessful), "trig-1", "https://l/webhook/trig-1"); err == nil {
		t.Fatal("registration must fail when the template attaches to nothing")
	}
	// The orphan must be cleaned up, or its name blocks the next attempt.
	if !f.templateDeleted {
		t.Error("an unattachable template must be deleted, not left to collide with its own name on the next save")
	}
}

func TestAWXRegisterAdoptsAnExistingTemplateOnNameConflict(t *testing.T) {
	// A save whose state write did not land leaves a template behind. The next
	// registration must adopt it and rotate the secret, not fail forever.
	f := newFakeAWX(t)
	f.nameConflict = true

	state, err := awxRegister(context.Background(), f.creds(awxEventSuccessful), "trig-1", "https://l/webhook/trig-1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if state.TemplateID != "99" {
		t.Errorf("template id = %q, want the adopted 99", state.TemplateID)
	}
	if f.created["password"] != state.Secret {
		t.Error("adopting must rotate the freshly minted secret onto the existing template")
	}
}

func TestAWXRegisterRequiresAnOrganization(t *testing.T) {
	// ★ Never hardcode organization 1. An org-less template is a 403 for a normal
	// user and an unlimited-duplicate orphan for a superuser.
	f := newFakeAWX(t)
	f.orgID = nil

	_, err := awxRegister(context.Background(), f.creds(awxEventSuccessful), "trig-1", "https://l/webhook/trig-1")
	if err == nil {
		t.Fatal("registration must fail when the template belongs to no organization")
	}
	if !strings.Contains(err.Error(), "organization") {
		t.Errorf("the error must name the problem: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The secret must never reach a log line or an error string.
// ---------------------------------------------------------------------------

func TestAWXSecretsNeverAppearInErrorStrings(t *testing.T) {
	const token = "super-secret-awx-token-value"
	const password = "super-secret-awx-password"

	f := newFakeAWX(t)
	f.orgID = nil // force the register to fail so we get an error string back

	cr := f.creds(awxEventSuccessful)
	cr.method = "basic"
	cr.token = token
	cr.username = "admin"
	cr.password = password

	_, err := awxRegister(context.Background(), cr, "trig-1", "https://l/webhook/trig-1")
	if err == nil {
		t.Fatal("expected a registration error")
	}
	for _, secret := range []string{token, password} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("★ a credential leaked into an error string: %v", err)
		}
	}

	// awxRedact itself: the token, the password, and the base64 Basic blob AWX
	// would be sent — plus any extra secret the caller passes (the minted one).
	blob := base64.StdEncoding.EncodeToString([]byte("admin:" + password))
	minted := "0123456789abcdef0123456789abcdef"
	msg := fmt.Sprintf("boom token=%s password=%s basic=%s minted=%s", token, password, blob, minted)
	got := awxRedact(cr, msg, minted)
	for name, secret := range map[string]string{"token": token, "password": password, "basic blob": blob, "minted secret": minted} {
		if strings.Contains(got, secret) {
			t.Errorf("awxRedact left the %s in place: %q", name, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Deregistration
// ---------------------------------------------------------------------------

func TestAWXDeregisterDetachesThenDeletes(t *testing.T) {
	f := newFakeAWX(t)
	cr := f.creds(awxEventStarted, awxEventSuccessful)

	state, err := awxRegister(context.Background(), cr, "trig-1", "https://l/webhook/trig-1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	awxDetach(context.Background(), cr, state.Root, state.TemplateID, state.Attached)
	awxDeleteTemplate(context.Background(), cr, state.Root, state.TemplateID)

	calls, _, detached := f.snapshot()
	if !awxSameEventSet(detached, []string{"started", "success"}) {
		t.Errorf("detached = %v, want every relation that was attached", detached)
	}
	if !f.templateDeleted {
		t.Error("the notification template must be deleted")
	}

	// Order matters: every detach lands before the delete.
	deleteAt, lastDetachAt := -1, -1
	for i, c := range calls {
		if strings.HasPrefix(c, "DELETE ") {
			deleteAt = i
		}
		if strings.Contains(c, "notification_templates_") {
			lastDetachAt = i
		}
	}
	if deleteAt < 0 || lastDetachAt < 0 || deleteAt < lastDetachAt {
		t.Errorf("expected the detaches to precede the delete; calls = %v", calls)
	}
}

func TestAWXDeregisterIsIdempotentOn404(t *testing.T) {
	// A template someone already removed by hand in AWX. A 404 on the detach or the
	// delete means it is already gone — which is success, not an error.
	f := newFakeAWX(t)
	f.deleteStatus = http.StatusNotFound
	cr := f.creds(awxEventSuccessful)

	// Must not panic, must not hang, must not retry into a second call.
	awxDetach(context.Background(), cr, "/api/v2/", "42", []string{"success"})
	awxDeleteTemplate(context.Background(), cr, "/api/v2/", "42")

	calls, _, _ := f.snapshot()
	deletes := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "DELETE ") {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("a 404 delete is already-gone: expected exactly 1 DELETE, got %d", deletes)
	}

	// And a second full teardown is still a no-op rather than an error.
	awxDeleteTemplate(context.Background(), cr, "/api/v2/", "42")
}

func TestAWXDeleteTemplateRetriesOnPendingNotifications(t *testing.T) {
	// 405 = "Delete not allowed while there are pending notifications" — which is
	// exactly what happens when our own ingest was unreachable. One retry, then
	// leave it: an orphan is inert, and blocking a flow delete would be worse.
	f := newFakeAWX(t)
	f.deleteStatus = http.StatusMethodNotAllowed

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	awxDeleteTemplate(ctx, f.creds(), "/api/v2/", "42") // must return, not hang

	calls, _, _ := f.snapshot()
	deletes := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "DELETE ") {
			deletes++
		}
	}
	if deletes < 1 {
		t.Error("expected at least the first DELETE attempt")
	}
}

// ---------------------------------------------------------------------------
// Inbound
// ---------------------------------------------------------------------------

func awxBasicHeader(user, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret))
}

func TestAWXVerifyBasic(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"

	if !awxVerifyBasic(awxBasicHeader(awxBasicUser, secret), secret) {
		t.Fatal("the credential AWX actually sends must verify")
	}

	// Fail closed, every way in.
	cases := map[string]string{
		"absent header":     "",
		"wrong password":    awxBasicHeader(awxBasicUser, "not-the-secret"),
		"wrong username":    awxBasicHeader("someone-else", secret),
		"not basic at all":  "Bearer " + secret,
		"bare secret":       secret,
		"malformed base64":  "Basic !!!!not-base64!!!!",
		"no colon in blob":  "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolonhere")),
		"empty credentials": "Basic " + base64.StdEncoding.EncodeToString([]byte(":")),
		"just the prefix":   "Basic ",
	}
	for name, header := range cases {
		if awxVerifyBasic(header, secret) {
			t.Errorf("%s must fail closed", name)
		}
	}

	// An empty secret can never authenticate anything — launch ALWAYS mints one, so
	// its absence means lost state or a forgery.
	if awxVerifyBasic(awxBasicHeader(awxBasicUser, ""), "") {
		t.Error("an empty secret must fail closed")
	}
	// Case-insensitive scheme, per RFC 7235.
	if !awxVerifyBasic("basic "+base64.StdEncoding.EncodeToString([]byte(awxBasicUser+":"+secret)), secret) {
		t.Error("the auth scheme is case-insensitive")
	}
}

func TestAWXInboundDecision(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	good := awxBasicHeader(awxBasicUser, secret)
	state := func(logical ...string) *awxWebhookState {
		return &awxWebhookState{TemplateID: "42", Secret: secret, Logical: logical}
	}
	body := func(status string) []byte {
		return []byte(fmt.Sprintf(`{"id":42,"name":"Demo","status":%q,"url":"https://awx/#/jobs/playbook/42"}`, status))
	}

	t.Run("good credential fires", func(t *testing.T) {
		code, event := awxInboundDecision(state(awxEventSuccessful), good, body("successful"))
		if code != 200 || event == nil {
			t.Fatalf("got (%d, %v), want (200, an event)", code, event)
		}
		if event["event"] != awxEventSuccessful || event["job_id"] != "42" || event["status"] != "successful" {
			t.Errorf("event shaped wrong: %#v", event)
		}
		// The job id arrives as a JSON *number*; the shared asString returns "" for
		// those, which is why awxEventStr exists.
		if event["job_id"] == "" {
			t.Error("the numeric job id must survive stringification")
		}
		if event["failed"] != false {
			t.Error("a successful job is not failed")
		}
	})

	t.Run("bad and absent credentials do not fire", func(t *testing.T) {
		for name, header := range map[string]string{
			"absent":       "",
			"wrong secret": awxBasicHeader(awxBasicUser, "wrong"),
			"wrong user":   awxBasicHeader("mallory", secret),
		} {
			code, event := awxInboundDecision(state(awxEventSuccessful), header, body("successful"))
			if code != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401", name, code)
			}
			if event != nil {
				t.Errorf("%s: the flow must NOT fire", name)
			}
		}
	})

	t.Run("no state and no secret fail closed", func(t *testing.T) {
		if code, event := awxInboundDecision(nil, good, body("successful")); code != 401 || event != nil {
			t.Errorf("nil state: got (%d, %v), want (401, nil)", code, event)
		}
		if code, event := awxInboundDecision(&awxWebhookState{TemplateID: "42"}, good, body("successful")); code != 401 || event != nil {
			t.Errorf("no secret: got (%d, %v), want (401, nil)", code, event)
		}
	})

	t.Run("an unsubscribed event is acknowledged but not fired", func(t *testing.T) {
		// AWX's `error` relation carries failures AND cancellations. A trigger that
		// only wants cancellations must drop the failures itself — otherwise it is
		// woken by every failure on the box.
		code, event := awxInboundDecision(state(awxEventCanceled), good, body("failed"))
		if code != http.StatusOK {
			t.Errorf("status = %d, want a 200 acknowledgement", code)
		}
		if event != nil {
			t.Fatal("a failure must not fire a canceled-only trigger")
		}
		// ...but the cancellation it did subscribe to still fires, through that same
		// shared relation.
		if _, event := awxInboundDecision(state(awxEventCanceled), good, body("canceled")); event == nil {
			t.Fatal("the cancellation must fire")
		} else if event["event"] != awxEventCanceled || event["failed"] != true {
			t.Errorf("cancellation shaped wrong: %#v", event)
		}
	})

	t.Run("the started family maps to one logical event", func(t *testing.T) {
		for _, status := range []string{"pending", "waiting", "running"} {
			_, event := awxInboundDecision(state(awxEventStarted), good, body(status))
			if event == nil || event["event"] != awxEventStarted {
				t.Errorf("status %q should map to the started event, got %#v", status, event)
			}
		}
	})

	t.Run("an unmodelled status is acknowledged, not guessed at", func(t *testing.T) {
		code, event := awxInboundDecision(state(awxEventSuccessful), good, body("some-new-awx-state"))
		if code != http.StatusOK || event != nil {
			t.Errorf("got (%d, %v), want (200, nil)", code, event)
		}
	})

	t.Run("a malformed body is a 400", func(t *testing.T) {
		if code, event := awxInboundDecision(state(awxEventSuccessful), good, []byte("{not json")); code != 400 || event != nil {
			t.Errorf("got (%d, %v), want (400, nil)", code, event)
		}
	})
}

// TestAWXInboundHandler drives the real gin handler end to end: a good Basic
// credential must answer 200 AND fire the flow; a bad or absent one must answer
// 401 and fire NOTHING.
func TestAWXInboundHandler(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	gin.SetMode(gin.TestMode)

	// Swap the dispatch seam so a fire is observable without a trigger service.
	var mu sync.Mutex
	var fired []map[string]interface{}
	orig := awxDispatch
	awxDispatch = func(_ *Service, _ *launch.Trigger, event map[string]interface{}) {
		mu.Lock()
		fired = append(fired, event)
		mu.Unlock()
	}
	t.Cleanup(func() { awxDispatch = orig })

	firedCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(fired)
	}

	// skip_awx_verification keeps the handler off the callback path, so the Basic
	// gate is what is under test here (awxVerifyJob has its own test below).
	tr := &launch.Trigger{
		ID:   "11111111-1111-1111-1111-111111111111",
		Type: launch.TriggerTypeAWXWebhook,
		Data: json.RawMessage(`{"__node_id":"node-7","skip_awx_verification":true}`),
	}
	state := &awxWebhookState{TemplateID: "42", Secret: secret, Logical: []string{awxEventSuccessful}}
	body := `{"id":42,"name":"Demo Job Template","status":"successful"}`

	// Through a real gin engine, not gin.CreateTestContext: gin's ResponseWriter
	// buffers the status code and only flushes it at the end of the handler chain,
	// so a directly-invoked handler leaves the recorder on its default 200 and a
	// 401 assertion would pass vacuously.
	svc := &Service{}
	router := gin.New()
	router.POST("/webhook/:id", func(c *gin.Context) {
		svc.serveAWXWebhook(c, tr, state, []byte(body))
	})

	serve := func(authHeader string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/webhook/"+tr.ID, strings.NewReader(body))
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		router.ServeHTTP(w, req)
		return w
	}

	t.Run("good credential: 200 and the flow fires", func(t *testing.T) {
		w := serve(awxBasicHeader(awxBasicUser, secret))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		// The dispatch is on a goroutine — the 200 goes out first, by design.
		deadline := time.Now().Add(2 * time.Second)
		for firedCount() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if firedCount() != 1 {
			t.Fatalf("the flow fired %d times, want 1", firedCount())
		}
		mu.Lock()
		event := fired[0]
		mu.Unlock()
		if event["__node_id"] != "node-7" {
			t.Error("__node_id must be carried through, or a multi-trigger flow injects into the wrong node")
		}
		if event["job_id"] != "42" || event["event"] != awxEventSuccessful {
			t.Errorf("event shaped wrong: %#v", event)
		}
	})

	t.Run("bad and absent credentials: 401 and the flow does NOT fire", func(t *testing.T) {
		before := firedCount()
		for name, header := range map[string]string{
			"absent":        "",
			"wrong secret":  awxBasicHeader(awxBasicUser, "forged"),
			"wrong user":    awxBasicHeader("mallory", secret),
			"not basic":     "Bearer " + secret,
			"bad base64":    "Basic @@@@",
			"empty creds":   "Basic " + base64.StdEncoding.EncodeToString([]byte(":")),
			"just a prefix": "Basic ",
		} {
			w := serve(header)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401", name, w.Code)
			}
		}
		// Give any (incorrectly) spawned goroutine time to fire before asserting.
		time.Sleep(50 * time.Millisecond)
		if firedCount() != before {
			t.Fatalf("★ a rejected delivery fired the flow (%d -> %d)", before, firedCount())
		}
	})
}

// ---------------------------------------------------------------------------
// The callback verification
// ---------------------------------------------------------------------------

func TestAWXVerifyJob(t *testing.T) {
	var jobStatus string
	var jobFound bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !jobFound {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"id": 42, "status": jobStatus})
	}))
	defer srv.Close()

	cr := awxCreds{base: srv.URL, method: "token", token: "tok"}
	state := &awxWebhookState{Root: "/api/v2/"}
	event := func(logical, status string) map[string]interface{} {
		return map[string]interface{}{"job_id": "42", "event": logical, "status": status}
	}

	jobFound, jobStatus = true, "successful"
	if err := awxVerifyJob(context.Background(), cr, state, event(awxEventSuccessful, "successful")); err != nil {
		t.Errorf("a truthful terminal notification must verify: %v", err)
	}

	// A forged terminal notification: AWX disagrees.
	jobStatus = "failed"
	if err := awxVerifyJob(context.Background(), cr, state, event(awxEventSuccessful, "successful")); err == nil {
		t.Error("a notification AWX disagrees with must be rejected")
	}

	// ★ The started race. A short playbook goes running -> successful in seconds, so
	// by the time the callback lands the job has often already finished. Demanding
	// an exact match there would drop legitimate started events.
	jobStatus = "successful"
	if err := awxVerifyJob(context.Background(), cr, state, event(awxEventStarted, "running")); err != nil {
		t.Errorf("a started notification must not be dropped just because the job has since finished: %v", err)
	}

	// A job AWX has never heard of.
	jobFound = false
	if err := awxVerifyJob(context.Background(), cr, state, event(awxEventStarted, "running")); err == nil {
		t.Error("a notification for a job AWX does not know must be rejected")
	}

	// No job id at all.
	if err := awxVerifyJob(context.Background(), cr, state, map[string]interface{}{"event": awxEventStarted}); err == nil {
		t.Error("a notification with no job id must be rejected")
	}
}

// ---------------------------------------------------------------------------
// API root discovery
// ---------------------------------------------------------------------------

func TestAWXResolveRoot(t *testing.T) {
	t.Run("upstream AWX", func(t *testing.T) {
		f := newFakeAWX(t)
		root, err := awxResolveRoot(context.Background(), f.creds())
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if root != "/api/v2/" {
			t.Errorf("root = %q, want /api/v2/", root)
		}
	})

	t.Run("AAP 2.5 gateway", func(t *testing.T) {
		// The gateway's /api/ banner LACKS available_versions. We detect on that
		// absence, never on a guessed gateway key.
		f := newFakeAWX(t)
		f.gateway = true
		root, err := awxResolveRoot(context.Background(), f.creds())
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if root != "/api/controller/v2/" {
			t.Errorf("root = %q, want /api/controller/v2/", root)
		}
	})

	t.Run("an explicit api_prefix short-circuits the sweep", func(t *testing.T) {
		f := newFakeAWX(t)
		cr := f.creds()
		cr.apiPrefix = "api/custom/v2" // no leading or trailing slash
		root, err := awxResolveRoot(context.Background(), cr)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if root != "/api/custom/v2/" {
			t.Errorf("root = %q, want the normalised override", root)
		}
		if calls, _, _ := f.snapshot(); len(calls) != 0 {
			t.Errorf("an override must cost no round-trip, got %v", calls)
		}
	})

	t.Run("a rejected credential is not a missing API", func(t *testing.T) {
		// A 401 means the prefix is RIGHT and the token is wrong. Sweeping on would
		// report "API not found" and send the operator hunting the wrong bug.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/" {
				writeJSON(w, 200, map[string]interface{}{"available_versions": map[string]interface{}{"v2": "/api/v2/"}})
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		_, err := awxResolveRoot(context.Background(), awxCreds{base: srv.URL, method: "token", token: "bad"})
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "credential") {
			t.Errorf("the error must say the credential was rejected, not that the API is missing: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Config parsing
// ---------------------------------------------------------------------------

func TestAWXBaseURL(t *testing.T) {
	cases := map[string]string{
		"awx.example.com":                           "https://awx.example.com",
		"  awx.example.com/  ":                      "https://awx.example.com",
		"https://awx.example.com":                   "https://awx.example.com",
		"https://awx.example.com/":                  "https://awx.example.com",
		"http://192.168.80.27":                      "http://192.168.80.27",
		"https://awx.example.com/api/v2":            "https://awx.example.com",
		"https://awx.example.com/api/v2/":           "https://awx.example.com",
		"https://aap.example.com/api/control":       "https://aap.example.com/api/control",
		"https://aap.example.com/api/controller/v2": "https://aap.example.com",
		"https://aap.example.com/api":               "https://aap.example.com",
		"https://awx.example.com/tower":             "https://awx.example.com/tower",
		"https://user:pass@awx.example.com":         "https://awx.example.com", // userinfo stripped
		"":                                          "",
		"   ":                                       "",
		"${secrets.AWX_URL}":                        "", // an unresolved ref is not a URL
		"ftp://awx.example.com":                     "",
	}
	for in, want := range cases {
		if got := awxBaseURL(in); got != want {
			t.Errorf("awxBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAWXEvents(t *testing.T) {
	// Empty → all four logical events.
	if got := awxEvents(""); !awxSameEventSet(got, awxAllEvents) {
		t.Errorf("an empty selection should mean all events, got %v", got)
	}
	// JSON-array form (the multi-select's on-the-wire shape).
	if got := awxEvents(`["started","failed"]`); !awxSameEventSet(got, []string{"started", "failed"}) {
		t.Errorf("JSON-array parse wrong: %v", got)
	}
	// CSV form with spaces.
	if got := awxEvents(" successful , canceled "); !awxSameEventSet(got, []string{"successful", "canceled"}) {
		t.Errorf("CSV parse wrong: %v", got)
	}
	// Unknown names are dropped, not passed through to AWX as a bogus relation.
	if got := awxEvents(`["successful","not-an-event"]`); !awxSameEventSet(got, []string{"successful"}) {
		t.Errorf("unknown events must be dropped, got %v", got)
	}
	// A selection of nothing but junk falls back to all, rather than to silence.
	if got := awxEvents("nonsense"); !awxSameEventSet(got, awxAllEvents) {
		t.Errorf("an entirely unknown selection should fall back to all, got %v", got)
	}
	// Duplicates collapse.
	if got := awxEvents("started,started"); len(got) != 1 {
		t.Errorf("duplicates should collapse, got %v", got)
	}
}

func TestAWXRelations(t *testing.T) {
	cases := []struct {
		logical []string
		want    []string
	}{
		{[]string{"started"}, []string{"started"}},
		{[]string{"successful"}, []string{"success"}},
		// ★ It is `error`, NOT `failure`.
		{[]string{"failed"}, []string{"error"}},
		{[]string{"canceled"}, []string{"error"}},
		// failed + canceled collapse onto ONE error relation.
		{[]string{"failed", "canceled"}, []string{"error"}},
		{awxAllEvents, []string{"started", "success", "error"}},
	}
	for _, tc := range cases {
		got := awxRelations(tc.logical)
		if !awxSameEventSet(got, tc.want) {
			t.Errorf("awxRelations(%v) = %v, want %v", tc.logical, got, tc.want)
		}
		if len(got) != len(tc.want) {
			t.Errorf("awxRelations(%v) = %v — a duplicate relation would be attached twice", tc.logical, got)
		}
	}
}

func TestAWXBoolInput(t *testing.T) {
	// resolveTriggerCreds only surfaces STRING values, so booleans are read off the
	// raw trigger data — and a variable-bound checkbox arrives as a string.
	tr := &launch.Trigger{Data: json.RawMessage(`{"allow_insecure":true,"skip_awx_verification":"true","other":"false"}`)}
	if !awxBoolInput(tr, "allow_insecure") {
		t.Error("a JSON bool must be read")
	}
	if !awxBoolInput(tr, "skip_awx_verification") {
		t.Error(`a "true" string must be read (a variable-bound checkbox arrives this way)`)
	}
	if awxBoolInput(tr, "other") {
		t.Error(`"false" must be false`)
	}
	if awxBoolInput(tr, "missing") {
		t.Error("an absent input must be false")
	}
	if awxBoolInput(&launch.Trigger{Data: json.RawMessage(`not json`)}, "allow_insecure") {
		t.Error("unparseable data must be false, not a panic")
	}
}

func TestAWXStateCurrentShortCircuit(t *testing.T) {
	cr := awxCreds{base: "https://awx.example.com", kind: awxKindJobTemplate, templateID: "7", logical: []string{"successful"}}
	current := &awxWebhookState{
		TemplateID:    "42",
		Secret:        "s3cr3t-and-long-enough",
		Base:          "https://awx.example.com",
		Kind:          awxKindJobTemplate,
		TemplateRefID: "7",
		Logical:       []string{"successful"},
		Attached:      []string{"success"},
	}
	if !awxStateCurrent(current, cr) {
		t.Fatal("an unchanged config must short-circuit — createTrigger runs on EVERY flow save")
	}

	// Every field that must invalidate it.
	for name, mutate := range map[string]func(*awxWebhookState){
		"no template id":   func(s *awxWebhookState) { s.TemplateID = "" },
		"no secret":        func(s *awxWebhookState) { s.Secret = "" },
		"different AWX":    func(s *awxWebhookState) { s.Base = "https://other.example.com" },
		"different kind":   func(s *awxWebhookState) { s.Kind = awxKindWorkflowTemplate },
		"different templ":  func(s *awxWebhookState) { s.TemplateRefID = "8" },
		"different events": func(s *awxWebhookState) { s.Logical = []string{"failed"} },
		"partial attach":   func(s *awxWebhookState) { s.Attached = nil },
	} {
		stale := *current
		stale.Logical = append([]string(nil), current.Logical...)
		stale.Attached = append([]string(nil), current.Attached...)
		mutate(&stale)
		if awxStateCurrent(&stale, cr) {
			t.Errorf("%s must invalidate the state and force a rebuild", name)
		}
	}
	if awxStateCurrent(nil, cr) {
		t.Error("no state at all must not short-circuit")
	}
}

func TestAWXEventStr(t *testing.T) {
	// AWX ids arrive as JSON numbers; the shared asString returns "" for those.
	if got := awxEventStr(float64(42)); got != "42" {
		t.Errorf("awxEventStr(42.0) = %q, want \"42\" (asString would give \"\")", got)
	}
	if got := asString(float64(42)); got != "" {
		t.Fatal("asString has changed — awxEventStr may no longer be needed")
	}
	if got := awxEventStr("running"); got != "running" {
		t.Errorf("strings pass through: %q", got)
	}
	if got := awxEventStr(nil); got != "" {
		t.Errorf("null (finished, on a started event) = %q, want \"\"", got)
	}
}

func TestAWXResourcePath(t *testing.T) {
	if got := awxResourcePath(awxKindJobTemplate); got != "job_templates" {
		t.Errorf("job template path = %q", got)
	}
	if got := awxResourcePath(awxKindWorkflowTemplate); got != "workflow_job_templates" {
		t.Errorf("workflow template path = %q", got)
	}
	if got := awxResourcePath(""); got != "job_templates" {
		t.Errorf("an unset kind must default to a job template, got %q", got)
	}
}
