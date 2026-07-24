package http

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	// defaultFormDataTimeout bounds how long a form load blocks on the
	// data-source flow on a cache MISS. Only the first uncached load pays
	// this; everyone else is served from cache.
	defaultFormDataTimeout = 20 * time.Second
	// formDataCacheTTL is how long a resolved result is reused before the
	// flow is run again. Short enough that prefill data stays reasonably
	// fresh, long enough that a burst of loads collapses to one execution.
	formDataCacheTTL = 30 * time.Second
	// formDataPollInterval is the cadence for polling execution status while
	// waiting for the data-source flow to finish.
	formDataPollInterval = 1 * time.Second
)

// formDataEntry is a cached data-source result with an expiry. The raw
// outputs are stored (not the flattened strings) so the same cached run can
// feed both ${data.X} scalar substitution and dynamic dropdown option lists.
type formDataEntry struct {
	outputs   map[string]interface{}
	expiresAt time.Time
}

// formDataResolver runs a form's data-source flow and exposes its outputs as
// ${data.X} substitution values. Results are cached per flow ID for a short
// TTL and de-duplicated with singleflight, so a burst of concurrent (often
// anonymous) form loads collapses to a SINGLE flow execution rather than one
// per viewer. The flow runs with no per-request input, so its output is the
// same for everyone — which is what makes flow-ID-only caching correct.
type formDataResolver struct {
	client       *http.Client
	apiURL       string
	timeout      time.Duration
	ttl          time.Duration
	pollInterval time.Duration

	group singleflight.Group
	mu    sync.Mutex
	cache map[string]formDataEntry
}

func newFormDataResolver(client *http.Client, apiURL string) *formDataResolver {
	return &formDataResolver{
		client:       client,
		apiURL:       strings.TrimRight(apiURL, "/"),
		timeout:      defaultFormDataTimeout,
		ttl:          formDataCacheTTL,
		pollInterval: formDataPollInterval,
		cache:        map[string]formDataEntry{},
	}
}

// Resolve returns the ${data.X} string map for a data-source flow — the raw
// outputs flattened to strings (arrays joined, whole floats de-".0"'d). See
// ResolveRaw for the caching / dedup / failure semantics.
func (r *formDataResolver) Resolve(flowID string, timeoutSeconds int) map[string]string {
	raw := r.ResolveRaw(flowID, timeoutSeconds)
	if raw == nil {
		return nil
	}
	return flattenOutputs(raw)
}

// ResolveRaw returns the raw outputs of a data-source flow. It never returns
// an error: on timeout or failure it returns an empty map, so the form still
// renders (just without the data), matching the "unknown reference resolves
// to empty" substitution semantic. A nil resolver (mTLS not configured,
// tests) short-circuits to nil. Results are cached per flow ID for a short
// TTL and concurrent loads are de-duplicated with singleflight, so a burst
// of (often anonymous) form loads collapses to a SINGLE flow execution.
func (r *formDataResolver) ResolveRaw(flowID string, timeoutSeconds int) map[string]interface{} {
	if r == nil || flowID == "" {
		return nil
	}

	if v, ok := r.cached(flowID); ok {
		return v
	}

	// Collapse concurrent loads of the same flow into one execution.
	v, _, _ := r.group.Do(flowID, func() (interface{}, error) {
		// Re-check inside the flight: another goroutine may have just filled
		// the cache while we were queued behind it.
		if v, ok := r.cached(flowID); ok {
			return v, nil
		}
		timeout := r.timeout
		if timeoutSeconds > 0 {
			timeout = time.Duration(timeoutSeconds) * time.Second
		}
		// The data-source path runs with no per-request input, so every viewer
		// shares one flow-ID-keyed cache entry. Body is the empty JSON object.
		outputs := r.run(flowID, []byte("{}"), timeout)
		r.store(flowID, outputs)
		return outputs, nil
	})
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

// ResolveComputed runs a flow with the given inputs (posted to it as
// ${input.X}) and returns its raw output map. Unlike ResolveRaw, the result
// depends on the inputs, so the cache/singleflight key is
// flowID + NUL + hex(sha256(inputsJSON)) — answer-specific, never the flat
// flow-id key. It never errors: a failed/timed-out run yields an empty map, so
// a computed field simply resolves to "" (matching the "unknown reference →
// empty" substitution semantic). A nil resolver short-circuits to nil.
//
// This is what makes a computed value SERVER-AUTHORITATIVE: the caller supplies
// the answers, the flow (not the client) produces the value.
func (r *formDataResolver) ResolveComputed(flowID string, inputs map[string]interface{}, timeoutSeconds int) map[string]interface{} {
	if r == nil || flowID == "" {
		return nil
	}

	inputsJSON, err := json.Marshal(inputs)
	if err != nil {
		log.WithError(err).Warn("form compute: could not marshal inputs; resolving to empty")
		return map[string]interface{}{}
	}
	sum := sha256.Sum256(inputsJSON)
	key := flowID + "\x00" + hex.EncodeToString(sum[:])

	if v, ok := r.cached(key); ok {
		return v
	}

	v, _, _ := r.group.Do(key, func() (interface{}, error) {
		if v, ok := r.cached(key); ok {
			return v, nil
		}
		timeout := r.timeout
		if timeoutSeconds > 0 {
			timeout = time.Duration(timeoutSeconds) * time.Second
		}
		outputs := r.run(flowID, inputsJSON, timeout)
		r.store(key, outputs)
		return outputs, nil
	})
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func (r *formDataResolver) cached(flowID string) (map[string]interface{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[flowID]
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.outputs, true
}

func (r *formDataResolver) store(flowID string, outputs map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[flowID] = formDataEntry{outputs: outputs, expiresAt: time.Now().Add(r.ttl)}
}

// run creates a data-source execution and polls for its outputs, mirroring
// the Start Flow action's create+poll contract against the internal API.
func (r *formDataResolver) run(flowID string, body []byte, timeout time.Duration) map[string]interface{} {
	executionID, err := r.createExecution(flowID, body)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "flow_id": flowID}).
			Warn("form data-source: could not start flow; rendering without ${data.X}")
		return map[string]interface{}{}
	}

	deadline := time.Now().Add(timeout)
	pollURL := fmt.Sprintf("%s/api/v1/internal/execution/%s", r.apiURL, executionID)
	for time.Now().Before(deadline) {
		time.Sleep(r.pollInterval)
		if outputs, done := r.pollResult(pollURL); done {
			if outputs == nil {
				return map[string]interface{}{}
			}
			return outputs
		}
	}
	log.WithFields(log.Fields{"flow_id": flowID, "execution_id": executionID}).
		Warn("form data-source: flow did not complete in time; rendering without ${data.X}")
	return map[string]interface{}{}
}

// createExecution POSTs to the internal execute endpoint and returns the new
// execution ID. The flow runs via its manual trigger (chosen API-side). The
// body is a JSON object of inputs that reach the flow as ${input.X}; the
// data-source path passes "{}", the compute path passes the form answers.
func (r *formDataResolver) createExecution(flowID string, body []byte) (string, error) {
	url := fmt.Sprintf("%s/api/v1/internal/flo/%s/execute", r.apiURL, flowID)
	if len(body) == 0 {
		body = []byte("{}")
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
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

// pollResult fetches the execution once and returns (outputs, true) when it
// has finished. A transient fetch/parse error returns (nil, false) so the
// caller simply polls again until the deadline.
func (r *formDataResolver) pollResult(pollURL string) (map[string]interface{}, bool) {
	req, err := http.NewRequest(http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, false
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()

	// A client error (401/403 from an internal-auth misconfig, 404 not-found)
	// will NEVER resolve by polling — surfacing it and stopping is far better
	// than silently polling until the 20s timeout and returning empty (which
	// makes a computed field / table appear to "spin forever" then go blank).
	// A 5xx may be transient, so keep polling on those.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		log.WithFields(log.Fields{"status": resp.StatusCode, "url": pollURL}).
			Warn("form data-source: internal execution poll was rejected — check launch→API internal (mTLS) auth")
		return nil, true
	}
	if resp.StatusCode >= 500 {
		return nil, false
	}

	var data struct {
		ExecutionStatus string                 `json:"execution_status"`
		Result          map[string]interface{} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, false
	}
	if data.ExecutionStatus == "executed" {
		// The execution result nests the flow's Set Output values under an
		// "outputs" key (alongside id / logs / status / node_results). Surface
		// that inner map so ${data.X} and computed field/amount values read the
		// actual outputs (e.g. out["parking_charge"]) — reading the wrapper made
		// every computed value resolve to nil. Confirmed against the execution
		// result shape. Fall back to the whole result only if "outputs" is
		// absent (defensive).
		if outputs, ok := data.Result["outputs"].(map[string]interface{}); ok {
			return outputs, true
		}
		return data.Result, true
	}
	return nil, false
}

// FetchExecution fetches a single execution by id from the internal API, for a
// form's result-page polling (see handleFormExecution). It returns the
// execution's flow id (so the caller can verify ownership — the execution must
// belong to the form's own flow), its status, and — once "executed" — the flow's
// Set-Output values (the inner result.outputs map, mirroring pollResult). ok is
// false on a nil resolver, a transport/decode failure, or a non-2xx response;
// outputs is nil until the execution completes.
func (r *formDataResolver) FetchExecution(executionID string) (floID, status string, outputs map[string]interface{}, ok bool) {
	if r == nil || executionID == "" {
		return "", "", nil, false
	}
	url := fmt.Sprintf("%s/api/v1/internal/execution/%s", r.apiURL, executionID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", "", nil, false
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", "", nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", "", nil, false
	}
	var data struct {
		FloID           string                 `json:"flo_id"`
		ExecutionStatus string                 `json:"execution_status"`
		Result          map[string]interface{} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", "", nil, false
	}
	if data.ExecutionStatus == "executed" {
		if o, inner := data.Result["outputs"].(map[string]interface{}); inner {
			return data.FloID, data.ExecutionStatus, o, true
		}
		return data.FloID, data.ExecutionStatus, data.Result, true
	}
	return data.FloID, data.ExecutionStatus, nil, true
}

// flattenOutputs turns a flow's output map into ${data.X} string values.
// Nested objects/arrays stringify via answerString (shared with the
// conditional-visibility evaluator); scalar outputs are the intended target.
func flattenOutputs(outputs map[string]interface{}) map[string]string {
	out := make(map[string]string, len(outputs))
	for k, v := range outputs {
		out[k] = answerString(v)
	}
	return out
}
