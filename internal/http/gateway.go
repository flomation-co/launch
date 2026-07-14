package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"flomation.app/automate/launch/internal/http/gwauth"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"
)

// gatewayResolution mirrors the API's api.GatewayResolution.
type gatewayResolution struct {
	APIID          string            `json:"api_id"`
	OrganisationID *string           `json:"organisation_id"`
	OwnerID        string            `json:"owner_id"`
	AuthType       string            `json:"auth_type"`
	AuthConfig     json.RawMessage   `json:"auth_config"`
	AuthSecretHash string            `json:"auth_secret_hash"`
	AuthSecretSalt string            `json:"auth_secret_salt"`
	Endpoints      []gatewayEndpoint `json:"endpoints"`
}

type gatewayEndpoint struct {
	Method      string `json:"method"`
	PathPattern string `json:"path_pattern"`
	FlowID      string `json:"flow_id"`
	TriggerID   string `json:"trigger_id"`
}

// resolveGateway fetches a gateway api's policy + endpoints from the API (mTLS).
// Returns nil on unknown api_id or any error (the caller answers 404/502).
func (s *Service) resolveGateway(apiID string) *gatewayResolution {
	url := fmt.Sprintf("%s/api/v1/internal/gateway/%s/resolve", s.config.InternalAPIURL(), apiID)
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
	var res gatewayResolution
	if json.NewDecoder(resp.Body).Decode(&res) != nil {
		return nil
	}
	return &res
}

// splitGatewayPath normalises a request path into its non-empty segments.
func splitGatewayPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// matchPattern matches a path pattern (e.g. "/users/:id") against request
// segments, returning the extracted params, whether it matched, and how many
// segments were STATIC (higher = more specific, so a static route beats a param
// route on the same path).
func matchPattern(pattern string, reqSegs []string) (map[string]string, bool, int) {
	patSegs := splitGatewayPath(pattern)
	if len(patSegs) != len(reqSegs) {
		return nil, false, 0
	}
	params := map[string]string{}
	static := 0
	for i, seg := range patSegs {
		if strings.HasPrefix(seg, ":") {
			name := strings.TrimPrefix(seg, ":")
			if name == "" {
				return nil, false, 0
			}
			params[name] = reqSegs[i]
			continue
		}
		if seg != reqSegs[i] {
			return nil, false, 0
		}
		static++
	}
	return params, true, static
}

// matchGatewayEndpoint picks the endpoint for (method, path): the most-static
// pattern whose method matches. When a pattern matches the path but no method
// does, allowed lists the verbs for a 405. matched is nil when no path matches.
func matchGatewayEndpoint(eps []gatewayEndpoint, method, path string) (matched *gatewayEndpoint, params map[string]string, allowed []string) {
	reqSegs := splitGatewayPath(path)
	bestStatic := -1
	allowedSet := map[string]struct{}{}
	for i := range eps {
		p, ok, static := matchPattern(eps[i].PathPattern, reqSegs)
		if !ok {
			continue
		}
		allowedSet[strings.ToUpper(eps[i].Method)] = struct{}{}
		if strings.EqualFold(eps[i].Method, method) && static > bestStatic {
			ep := eps[i]
			matched = &ep
			params = p
			bestStatic = static
		}
	}
	for m := range allowedSet {
		allowed = append(allowed, m)
	}
	return matched, params, allowed
}

// oidcKeyfuncCache caches one keyfunc per jwks_uri so JWKS isn't refetched on
// every request (keyfunc refreshes in the background).
var (
	oidcKeyfuncMu    sync.Mutex
	oidcKeyfuncCache = map[string]keyfunc.Keyfunc{}
)

func oidcKeyfunc(jwksURI string) (jwt.Keyfunc, error) {
	oidcKeyfuncMu.Lock()
	defer oidcKeyfuncMu.Unlock()
	if kf, ok := oidcKeyfuncCache[jwksURI]; ok {
		return kf.Keyfunc, nil
	}
	kf, err := keyfunc.NewDefault([]string{jwksURI})
	if err != nil {
		return nil, err
	}
	oidcKeyfuncCache[jwksURI] = kf
	return kf.Keyfunc, nil
}

// buildAuthenticator constructs the pure (non-flomation) authenticator for a
// resolution. flomation is handled separately (it needs an API round-trip).
func (s *Service) buildAuthenticator(res *gatewayResolution, cfg gwauth.Config) (gwauth.Authenticator, bool) {
	switch res.AuthType {
	case "", "open":
		return gwauth.Open{}, true
	case "api_key":
		return gwauth.APIKey{Header: cfg.Header, Hash: res.AuthSecretHash, Salt: res.AuthSecretSalt}, true
	case "basic":
		return gwauth.Basic{Username: cfg.Username, Realm: cfg.Realm, Hash: res.AuthSecretHash, Salt: res.AuthSecretSalt}, true
	case "oidc":
		kf, err := oidcKeyfunc(cfg.JWKSURI)
		if err != nil {
			log.WithError(err).WithField("jwks_uri", cfg.JWKSURI).Error("gateway oidc: JWKS unavailable")
			return nil, false
		}
		return gwauth.OIDC{Issuer: cfg.Issuer, Audience: cfg.Audience, RequiredClaims: cfg.RequiredClaims, Keyfunc: kf}, true
	}
	return nil, false
}

// gatewaySessionToken pulls the Flomation session token from the Authorization
// bearer or the flomation-token cookie (browser).
func gatewaySessionToken(c *gin.Context) string {
	if parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return strings.TrimSpace(parts[1])
	}
	if ck, err := c.Cookie("flomation-token"); err == nil {
		return ck
	}
	return ""
}

type verifySessionRequest struct {
	Token              string `json:"token"`
	OrganisationID     string `json:"organisation_id"`
	OwnerID            string `json:"owner_id"`
	RequiredPermission string `json:"required_permission"`
}

type verifySessionResponse struct {
	OK     bool   `json:"ok"`
	UserID string `json:"user_id"`
}

// verifyFlomationSession delegates the flomation auth check to the API (JWT
// validation + org membership + RBAC live there). Returns (ok, forwardToken).
func (s *Service) verifyFlomationSession(apiID, token string, res *gatewayResolution, cfg gwauth.Config) bool {
	org := ""
	if res.OrganisationID != nil {
		org = *res.OrganisationID
	}
	body, _ := json.Marshal(verifySessionRequest{
		Token: token, OrganisationID: org, OwnerID: res.OwnerID, RequiredPermission: cfg.RequiredPermission,
	})
	url := fmt.Sprintf("%s/api/v1/internal/gateway/%s/verify-session", s.config.InternalAPIURL(), apiID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.apiClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var vr verifySessionResponse
	if json.NewDecoder(resp.Body).Decode(&vr) != nil {
		return false
	}
	return vr.OK
}

// handleGateway serves ANY /gw/:apiId/*path — the Flomation Gateway edge. It
// resolves the API, matches the route, runs the pluggable authenticator, builds
// the trigger data (method + path params + query + body, plus verified ${claims}),
// and dispatches the endpoint's flow via the Web Trigger path.
func (s *Service) handleGateway(c *gin.Context) {
	apiID := c.Param("apiId")
	path := c.Param("path")

	res := s.resolveGateway(apiID)
	if res == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	cfg := gwauth.ParseConfig(res.AuthConfig)
	origin := c.GetHeader("Origin")

	// CORS preflight: advertise the verbs available for this path (any auth
	// header is not sent on preflight, so no auth is checked here).
	if c.Request.Method == http.MethodOptions {
		s.setEmbedCORS(c, origin)
		if _, _, allowed := matchGatewayEndpoint(res.Endpoints, "", path); len(allowed) > 0 {
			c.Writer.Header().Set("Access-Control-Allow-Methods", strings.Join(append(allowed, http.MethodOptions), ", "))
		}
		c.AbortWithStatus(http.StatusNoContent)
		return
	}

	matched, params, allowed := matchGatewayEndpoint(res.Endpoints, c.Request.Method, path)
	if matched == nil {
		if len(allowed) > 0 {
			c.Header("Allow", strings.Join(allowed, ", "))
			c.AbortWithStatus(http.StatusMethodNotAllowed)
			return
		}
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	// Authenticate per the API's policy.
	sessionToken := gatewaySessionToken(c)
	var claims map[string]interface{}
	forwardToken := ""
	if res.AuthType == "flomation" {
		if sessionToken == "" || !s.verifyFlomationSession(apiID, sessionToken, res, cfg) {
			s.setEmbedCORS(c, origin)
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		// A verified Flomation user → forward the token so the flow resolves ${user.X}.
		forwardToken = "Bearer " + sessionToken
	} else {
		auth, ok := s.buildAuthenticator(res, cfg)
		if !ok {
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		r := auth.Authenticate(c.Request)
		if !r.OK {
			s.setEmbedCORS(c, origin)
			if r.Challenge != "" {
				c.Header("WWW-Authenticate", r.Challenge)
			}
			status := r.Status
			if status == 0 {
				status = http.StatusUnauthorized
			}
			c.AbortWithStatus(status)
			return
		}
		claims = r.Claims
	}
	s.setEmbedCORS(c, origin)

	// Build the trigger data: method + path params + query + body (all bare
	// outputs on the Web Trigger). Verified OIDC claims land as the bare ${claims}.
	data := map[string]interface{}{"method": c.Request.Method}
	for k, v := range params {
		data[k] = v
	}
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
					if _, exists := data[k]; !exists {
						data[k] = v
					}
				}
			}
		}
	}
	if len(claims) > 0 {
		data["claims"] = claims
	}

	body, _ := json.Marshal(data)
	executionID, outputs, done := s.runWebInvoke(matched.FlowID, matched.TriggerID, body, forwardToken)
	if !done {
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
	// No Web Response — return the flow's declared outputs (never the full result).
	delete(outputs, webResponseKey)
	c.JSON(http.StatusOK, outputs)
}
