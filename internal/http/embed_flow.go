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
// :id is the flow id), reusing the shared publishable-key + origin + opt-in gate.
func (s *Service) embedFlowGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if uuid.Validate(id) != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
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

	// Build the trigger data. These keys become the Web Trigger node's bare
	// outputs (${method}, ${id}, ${name}, …) via the executor's InjectTriggerData.
	data := map[string]interface{}{"method": c.Request.Method}
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
			// Spread top-level JSON body keys as bare fields too.
			var bodyObj map[string]interface{}
			if json.Unmarshal(raw, &bodyObj) == nil {
				for k, v := range bodyObj {
					data[k] = v
				}
			}
		}
	}

	body, _ := json.Marshal(data)
	// Forward the end-user's Sentinel JWT (if any) so the API can resolve the
	// logged-in visitor and populate ${user.X}. Anonymous when absent.
	authToken := c.GetHeader("Authorization")
	executionID, outputs, done := s.runWebInvoke(flowID, body, authToken)

	if !done {
		// Still running past the hang budget — hand back the execution id to poll.
		c.JSON(http.StatusAccepted, gin.H{"execution_id": executionID})
		return
	}

	if wr, ok := parseWebResponse(outputs[webResponseKey]); ok {
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

// runWebInvoke creates an execution for the flow with the given trigger data and
// polls it until it completes or the hang budget elapses. Returns the execution
// id, the flow outputs (when completed), and whether it completed in time.
func (s *Service) runWebInvoke(flowID string, body []byte, authToken string) (string, map[string]interface{}, bool) {
	executionID, err := s.createFlowExecution(flowID, body, authToken)
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

func (s *Service) createFlowExecution(flowID string, body []byte, authToken string) (string, error) {
	url := fmt.Sprintf("%s/api/v1/internal/flo/%s/execute", s.config.InternalAPIURL(), flowID)
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
	return data.Result, true
}

// webResponse is the parsed Web Response capture.
type webResponse struct {
	body        string
	status      int
	contentType string
	headers     map[string]string
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
