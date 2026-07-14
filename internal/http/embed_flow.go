package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	// webInvokeMaxBody caps the request body we read into the flow.
	webInvokeMaxBody = 1 << 20 // 1 MiB
	// webResponseKey is the reserved flow-output key the Web Response action
	// writes (mirror of the executor's web_response.WebResponseKey).
	webResponseKey = "__web_response__"
	// threadHeader round-trips the conversation thread id to/from the client.
	threadHeader = "X-Flomation-Thread"
)

// Poll timing — vars (not consts) so tests can shrink them.
var (
	// webInvokeTimeout is how long the POST hangs before returning 202 so the
	// client can poll for the eventual result.
	webInvokeTimeout = 30 * time.Second
	// webInvokePollInterval is how often we poll the execution while hanging.
	webInvokePollInterval = 500 * time.Millisecond
)

// embedFlowGate guards the Web Trigger invoke by the FLOW resource directly (the
// embedFlowPreflight answers the CORS preflight (OPTIONS) for the Web Trigger
// invoke. Unlike the generic embedPreflight it advertises the trigger's ACCEPTED
// verbs in Access-Control-Allow-Methods (plus OPTIONS), so a browser only
// proceeds with a permitted method; it falls back to the generic set when the
// config is unavailable or declares no verb restriction. No key is checked (the
// browser sends none on preflight).
func (s *Service) embedFlowPreflight(c *gin.Context) {
	s.setEmbedCORS(c, c.GetHeader("Origin"))
	if id := c.Param("id"); uuid.Validate(id) == nil {
		if cfg := s.fetchWebTriggerConfig(id); cfg != nil && len(cfg.Methods) > 0 {
			methods := append(append([]string{}, cfg.Methods...), http.MethodOptions)
			c.Writer.Header().Set("Access-Control-Allow-Methods", strings.Join(methods, ", "))
		}
	}
	c.AbortWithStatus(http.StatusNoContent)
}

// :id is the flow id), reusing the shared publishable-key + origin + opt-in gate.
func (s *Service) embedFlowGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if uuid.Validate(id) != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		// The trigger's auth_mode decides whether a publishable key is required.
		// Fetch it once here and stash it so the handler reuses this round-trip.
		// A missing/nil config means "not a Web Trigger flow" or an API blip — we
		// fall through to the secure publishable gate rather than opening up.
		cfg := s.fetchWebTriggerConfig(id)
		if cfg != nil {
			c.Set(webTriggerCfgKey, cfg)
		}
		if cfg != nil && cfg.AuthMode == webAuthPublic {
			// Publicly open: no key, any origin. Reflect the caller's Origin so a
			// browser accepts the response — that is the intended "open" semantics.
			s.setEmbedCORS(c, c.GetHeader("Origin"))
			c.Next()
			return
		}
		if s.applyEmbedGate(c, embedResourceFlow, id) {
			c.Next()
		}
	}
}

// handleEmbedFlowInvoke is the synchronous Web Trigger entrypoint: it turns the
// HTTP request into the flow's trigger data (method + query/body fields as bare
// outputs), runs the flow via the internal create+poll, and returns the flow's
// captured Web Response — or a default on completion, or 202 on timeout so the
// caller can poll. Identity (${user.X}) and history (${history}) are layered on
// in a later slice.
func (s *Service) handleEmbedFlowInvoke(c *gin.Context) {
	flowID := c.Param("id") // gate already validated uuid + opt-in

	method := c.Request.Method
	// Forward the end-user's Sentinel JWT (if any) so the API can resolve the
	// logged-in visitor and populate ${user.X}. Anonymous when absent.
	authToken := c.GetHeader("Authorization")

	// The Web Trigger's config drives verb enforcement + history. Best-effort:
	// on error, proceed as a plain any-verb, no-history invoke. The gate has
	// already fetched it and stashed it on the context — reuse that to avoid a
	// second API round-trip (and fall back to a fetch if invoked without the gate).
	var cfg *webTriggerCfg
	if v, ok := c.Get(webTriggerCfgKey); ok {
		cfg, _ = v.(*webTriggerCfg)
	} else {
		cfg = s.fetchWebTriggerConfig(flowID)
	}

	// Strict verb enforcement when the trigger declares accepted methods.
	if cfg != nil && len(cfg.Methods) > 0 && !containsFold(cfg.Methods, method) {
		c.Header("Allow", strings.Join(cfg.Methods, ", "))
		c.AbortWithStatus(http.StatusMethodNotAllowed)
		return
	}

	// Build the trigger data. These keys become the Web Trigger node's bare
	// outputs (${method}, ${id}, ${name}, …) via the executor's InjectTriggerData.
	data := map[string]interface{}{"method": method}
	for k, v := range c.Request.URL.Query() {
		if len(v) == 1 {
			data[k] = v[0]
		} else if len(v) > 1 {
			data[k] = v
		}
	}
	if c.Request.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(c.Request.Body, webInvokeMaxBody))
		if len(raw) > 0 {
			data["raw_body"] = string(raw)
			var bodyObj map[string]interface{}
			if json.Unmarshal(raw, &bodyObj) == nil {
				for k, v := range bodyObj {
					data[k] = v
				}
			}
		}
	}

	// Conversation history (opt-in via keep_history): mint/resume a thread, inject
	// the running history as ${history}, and record the user's message turn.
	var threadID string
	keepHistory := cfg != nil && cfg.KeepHistory
	messageField := "message"
	if cfg != nil && cfg.MessageField != "" {
		messageField = cfg.MessageField
	}
	if keepHistory {
		threadID = c.GetHeader(threadHeader)
		if threadID == "" {
			threadID = s.mintWebThread(flowID, authToken)
		}
		if threadID != "" {
			data["history"] = s.webThreadHistory(threadID)
			if msg, ok := data[messageField].(string); ok && msg != "" {
				s.appendWebThreadTurn(threadID, "user", msg)
			}
		}
	}

	body, _ := json.Marshal(data)
	triggerID := ""
	if cfg != nil {
		triggerID = cfg.TriggerID
	}
	executionID, outputs, done := s.runWebInvoke(flowID, triggerID, body, authToken)

	if threadID != "" {
		c.Header(threadHeader, threadID) // round-trip so the client persists it
	}

	if !done {
		c.JSON(http.StatusAccepted, gin.H{"execution_id": executionID, "thread_id": threadID})
		return
	}

	wr, hasWR := parseWebResponse(outputs[webResponseKey])

	// Record the assistant turn: the Web Response's history text, else its body.
	if keepHistory && threadID != "" && hasWR {
		assistant := wr.history
		if assistant == "" {
			assistant = wr.body
		}
		if assistant != "" {
			s.appendWebThreadTurn(threadID, "assistant", assistant)
		}
	}

	if hasWR {
		for k, v := range wr.headers {
			c.Header(k, v)
		}
		c.Header("Content-Type", wr.contentType)
		c.String(wr.status, wr.body)
		return
	}

	// No Web Response action ran — return the flow outputs as the default.
	delete(outputs, webResponseKey)
	c.JSON(http.StatusOK, outputs)
}

// Web Trigger auth modes (mirror of the API's webAuth* constants).
const (
	webAuthPublishable = "publishable"
	webAuthPublic      = "public"
)

// webTriggerCfgKey stashes the trigger config on the gin context so the gate and
// the handler share one API round-trip instead of fetching it twice.
const webTriggerCfgKey = "web_trigger_cfg"

// webTriggerCfg mirrors the API's Web Trigger config projection.
type webTriggerCfg struct {
	Found        bool              `json:"found"`
	KeepHistory  bool              `json:"keep_history"`
	MessageField string            `json:"message_field"`
	Methods      []string          `json:"methods"`
	Fields       map[string]string `json:"fields"`
	// AuthMode is the invoke gate: "publishable" (require an embed-app key, the
	// secure default) or "public" (open, no key). The API projects an unset value
	// as "publishable"; the gate treats a missing config as publishable too.
	AuthMode string `json:"auth_mode"`
	// TriggerID is the flow's "web" trigger record id — invoke via this trigger so
	// the execution starts from the Web Trigger node (not the flow's manual trigger).
	TriggerID string `json:"trigger_id"`
}

// fetchWebTriggerConfig reads the flow's Web Trigger config from the API. Returns
// nil on any error (the caller degrades to a plain invoke).
func (s *Service) fetchWebTriggerConfig(flowID string) *webTriggerCfg {
	url := fmt.Sprintf("%s/api/v1/internal/flo/%s/web-trigger", s.config.InternalAPIURL(), flowID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := s.apiClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var cfg webTriggerCfg
	if json.NewDecoder(resp.Body).Decode(&cfg) != nil || !cfg.Found {
		return nil
	}
	return &cfg
}

// mintWebThread creates a thread (bound to the forwarded user) and returns its id
// ("" on failure — history is then skipped for this turn).
func (s *Service) mintWebThread(flowID, authToken string) string {
	body, _ := json.Marshal(map[string]string{"flow_id": flowID})
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/internal/web-thread", s.config.InternalAPIURL()), bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", authToken)
	}
	resp, err := s.apiClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return ""
	}
	var out struct {
		ThreadID string `json:"thread_id"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	return out.ThreadID
}

// webThreadHistory fetches a thread's recent turns ([{role,content}], oldest-first).
func (s *Service) webThreadHistory(threadID string) []map[string]string {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/internal/web-thread/%s/history", s.config.InternalAPIURL(), threadID), nil)
	if err != nil {
		return nil
	}
	resp, err := s.apiClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Turns []map[string]string `json:"turns"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	return out.Turns
}

// appendWebThreadTurn records one turn (best-effort).
func (s *Service) appendWebThreadTurn(threadID, role, content string) {
	body, _ := json.Marshal(map[string]string{"role": role, "content": content})
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/internal/web-thread/%s/turn", s.config.InternalAPIURL(), threadID), bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := s.apiClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// runWebInvoke creates an execution for the flow with the given trigger data and
// polls it until it completes or the hang budget elapses. Returns the execution
// id, the flow outputs (when completed), and whether it completed in time.
func (s *Service) runWebInvoke(flowID, triggerID string, body []byte, authToken string) (string, map[string]interface{}, bool) {
	executionID, err := s.createFlowExecution(flowID, triggerID, body, authToken)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "flow_id": flowID}).Error("web invoke: could not start flow")
		return "", nil, false
	}

	pollURL := fmt.Sprintf("%s/api/v1/internal/execution/%s", s.config.InternalAPIURL(), executionID)
	deadline := time.Now().Add(webInvokeTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(webInvokePollInterval)
		if outputs, finished := s.pollExecution(pollURL); finished {
			if outputs == nil {
				outputs = map[string]interface{}{}
			}
			return executionID, outputs, true
		}
	}
	return executionID, nil, false
}

func (s *Service) createFlowExecution(flowID, triggerID string, body []byte, authToken string) (string, error) {
	// Invoke the Web Trigger specifically when we know it (entry = the Web Trigger
	// node). Without a trigger id the generic execute path picks the flow's manual
	// trigger, whose entry node is wrong/stale — "no start node specified".
	url := fmt.Sprintf("%s/api/v1/internal/flo/%s/execute", s.config.InternalAPIURL(), flowID)
	if triggerID != "" {
		url = fmt.Sprintf("%s/api/v1/internal/flo/%s/trigger/%s/execute", s.config.InternalAPIURL(), flowID, triggerID)
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// Forwarded end-user token — the API validates it and binds ${user.X}.
	if authToken != "" {
		req.Header.Set("Authorization", authToken)
	}
	resp, err := s.apiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("execute returned status %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("execute returned an empty execution id")
	}
	return out.ID, nil
}

// pollExecution fetches the execution once; returns (outputs, true) once it has
// finished (the flow's Set-Output values nested under result.outputs).
func (s *Service) pollExecution(pollURL string) (map[string]interface{}, bool) {
	req, err := http.NewRequest(http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, false
	}
	resp, err := s.apiClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	var data struct {
		ExecutionStatus string                 `json:"execution_status"`
		Result          map[string]interface{} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, false
	}
	if data.ExecutionStatus != "executed" {
		return nil, false
	}
	if outputs, ok := data.Result["outputs"].(map[string]interface{}); ok {
		return outputs, true
	}
	// No declared flow outputs — return an EMPTY map, never the whole result.
	// data.Result also carries privileged fields (logs, billing_duration, status)
	// that must never reach a Web Trigger caller; only the flow's own outputs
	// (or the Web Response) are public.
	return map[string]interface{}{}, true
}

// webResponse is the parsed Web Response capture.
type webResponse struct {
	body        string
	status      int
	contentType string
	headers     map[string]string
	history     string // clean assistant text to record (defaults to body)
}

// parseWebResponse coerces the reserved __web_response__ output (set by the Web
// Response action) into an HTTP response, applying sensible defaults.
func parseWebResponse(v interface{}) (webResponse, bool) {
	m, ok := v.(map[string]interface{})
	if !ok {
		return webResponse{}, false
	}
	wr := webResponse{status: http.StatusOK, contentType: "application/json", headers: map[string]string{}}
	if b, ok := m["body"]; ok && b != nil {
		wr.body = fmt.Sprint(b)
	}
	if s := toInt(m["status_code"]); s != 0 {
		wr.status = s
	}
	if ct, ok := m["content_type"].(string); ok && strings.TrimSpace(ct) != "" {
		wr.contentType = ct
	}
	wr.headers = toHeaders(m["headers"])
	if h, ok := m["history"].(string); ok {
		wr.history = h
	}
	return wr, true
}

func toInt(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n
		}
	}
	return 0
}

// toHeaders accepts either a JSON object or a JSON-string of headers.
func toHeaders(v interface{}) map[string]string {
	out := map[string]string{}
	switch x := v.(type) {
	case map[string]interface{}:
		for k, val := range x {
			out[k] = fmt.Sprint(val)
		}
	case string:
		var m map[string]interface{}
		if json.Unmarshal([]byte(x), &m) == nil {
			for k, val := range m {
				out[k] = fmt.Sprint(val)
			}
		}
	}
	return out
}
