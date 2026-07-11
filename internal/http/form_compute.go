package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"flomation.app/automate/launch"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// formComputeMaxBytes bounds the request body of the /form/:id/compute
// endpoint. A form's answer map is small; anything larger is abusive. The
// read is done through an io.LimitReader so an oversized/streamed body can
// never exhaust memory.
const formComputeMaxBytes int64 = 256 * 1024

// computeFormFieldBody is the /form/:id/compute request: the name of the field
// to compute and the current (client-authored) answers. The answers are
// sanitised before they reach the compute flow.
type computeFormFieldBody struct {
	Field   string                 `json:"field"`
	Answers map[string]interface{} `json:"answers"`
}

// computeFieldComponent finds the named field and returns it ONLY if it
// declares a value_source. ok=false (no such field, or the field is not a
// compute field) is the check that stops /form/:id/compute running an
// arbitrary or undeclared flow — a caller may only run the flow the form
// author attached to a specific field, never a flow id of their choosing.
func computeFieldComponent(def formDefinition, field string) (formComponent, bool) {
	for _, page := range def.Pages {
		for _, c := range page.Components {
			if c.Name == field && strings.TrimSpace(c.ValueSource) != "" {
				return c, true
			}
		}
	}
	return formComponent{}, false
}

// computeFormField computes a display field's value by running the flow the
// form author bound to it (value_source), fed the current form answers as
// ${input.X}. It exists for NON-payment computed fields — a payment field's
// amount is computed server-side at checkout, never here. Security:
//
//   - the id must name a live form trigger;
//   - the named field must exist AND declare a value_source (else 400) — a
//     caller can never run an arbitrary flow, only the author's declared one;
//   - a per-form token bucket bounds how often the compute flow can be driven;
//   - the answers pass through the full submit sanitisation pipeline before
//     they reach the flow, so a crafted POST can't smuggle non-whitelisted
//     options or hidden-branch values into ${input.X}.
func (s *Service) computeFormField(c *gin.Context) {
	id := c.Param("id")
	if id == "" || uuid.Validate(id) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if tr == nil || tr.Type != launch.TriggerTypeForm {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	// Per-form rate limit — bound how often a client can drive the compute
	// flow. Checked before any expensive work (parse / sanitise / execute).
	if s.formComputeLimiter != nil && !s.formComputeLimiter.allow(id) {
		c.AbortWithStatus(http.StatusTooManyRequests)
		return
	}

	def, _ := parseFormDefinition(tr.Data)

	// Login gate parity with the form itself — a login-required form must not
	// compute (and thereby leak flow output) for an anonymous caller.
	cookie, _ := c.Cookie("flomation-token")
	token := extractSessionToken(c.GetHeader("Authorization"), cookie)
	userID := s.resolveSessionUser(token)
	if def.RequireLogin && userID == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	// Bound the body read explicitly (never trust Content-Length).
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, formComputeMaxBytes))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	var body computeFormFieldBody
	if err := json.Unmarshal(raw, &body); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Only a field the author marked value_source may be computed.
	comp, ok := computeFieldComponent(def, body.Field)
	if !ok {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	answers := body.Answers
	if answers == nil {
		answers = map[string]interface{}{}
	}
	delete(answers, "__submission_id")

	// Sanitise the answers exactly as submitForm does before they reach the
	// flow: resolve the definition (baking any dynamic options), then run the
	// option/matrix whitelist + display/computed/read-only/hidden strips.
	ctx := substitutionContext{QueryParams: queryParamsMap(c)}
	if userID != "" {
		if vars, verr := s.loadUserVariables(userID); verr == nil {
			ctx.UserVariables = vars
		}
	}
	var dataOutputs map[string]interface{}
	if def.DataSource != nil && def.DataSource.FlowID != "" {
		if formUsesDataNamespace(def) || formHasDynamicOptions(def) {
			dataOutputs = s.formData.ResolveRaw(def.DataSource.FlowID, def.DataSource.TimeoutSeconds)
			ctx.DataVariables = flattenOutputs(dataOutputs)
		}
	}
	if formHasDynamicOptions(def) {
		def = bakeDynamicOptions(def, dataOutputs)
	}
	resolved := resolveFormForRender(def, ctx)
	sanitised := sanitiseFormSubmission(answers, resolved)

	out := s.formData.ResolveComputed(comp.ValueSource, sanitised, 0)
	c.JSON(http.StatusOK, gin.H{"value": out[computeOutputKey(comp)]})
}

// computeRateLimiter is a tiny per-key token bucket used to bound how often a
// single form can drive its compute flow. Each key (a form trigger id) refills
// at refillPerSec tokens/second up to burst. Hand-rolled to avoid a new
// dependency. Safe for concurrent use.
type computeRateLimiter struct {
	mu           sync.Mutex
	buckets      map[string]*tokenBucket
	burst        float64
	refillPerSec float64
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newComputeRateLimiter(refillPerSec, burst float64) *computeRateLimiter {
	return &computeRateLimiter{
		buckets:      map[string]*tokenBucket{},
		burst:        burst,
		refillPerSec: refillPerSec,
	}
}

// allow reports whether a request for key may proceed, consuming one token.
// The first request for a new key always passes (the bucket starts full).
func (l *computeRateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &tokenBucket{tokens: l.burst - 1, last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * l.refillPerSec
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
