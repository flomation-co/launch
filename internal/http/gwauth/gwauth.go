// Package gwauth implements the Flomation Gateway's pluggable authenticator
// subsystem. Each Gateway API declares an auth type; the edge builds the matching
// Authenticator from the API's (server-supplied) policy and runs it before
// dispatching a request to the target flow.
//
// Providers: open, api_key (header match), basic (HTTP Basic), oidc (bearer JWT
// verified against a JWKS). The "flomation" provider (Sentinel session → org RBAC)
// is NOT here — it needs an API round-trip, so the caller handles it.
//
// Secrets: api_key/basic never see plaintext at rest — the API stores a salted
// SHA-256 and hands the (hash, salt) to the edge, which hashes the presented
// value and compares in constant time.
package gwauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// Result is an authenticator's verdict.
type Result struct {
	// OK is true when the request is authenticated.
	OK bool
	// Claims are verified token claims (oidc) surfaced to the flow as ${claims}.
	Claims map[string]interface{}
	// Challenge, when set, is the WWW-Authenticate header value for a 401 (basic).
	Challenge string
	// Status is the suggested failure status (401/403); 0 is treated as 401.
	Status int
}

func deny(status int) Result { return Result{OK: false, Status: status} }

// Authenticator verifies an inbound request against a Gateway API's auth policy.
type Authenticator interface {
	Authenticate(r *http.Request) Result
}

// secretMatches constant-time compares sha256(salt+presented) against the stored
// hex hash.
func secretMatches(hexHash, salt, presented string) bool {
	sum := sha256.Sum256([]byte(salt + presented))
	got := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(hexHash)) == 1
}

func bearer(r *http.Request) string {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// ── open ──

type Open struct{}

func (Open) Authenticate(*http.Request) Result { return Result{OK: true} }

// ── api_key ──

type APIKey struct {
	Header string // defaults to X-API-Key
	Hash   string
	Salt   string
}

func (a APIKey) Authenticate(r *http.Request) Result {
	header := a.Header
	if header == "" {
		header = "X-API-Key"
	}
	presented := r.Header.Get(header)
	if presented == "" || a.Hash == "" {
		return deny(http.StatusUnauthorized)
	}
	if !secretMatches(a.Hash, a.Salt, presented) {
		return deny(http.StatusUnauthorized)
	}
	return Result{OK: true}
}

// ── basic ──

type Basic struct {
	Username string
	Realm    string
	Hash     string
	Salt     string
}

func (b Basic) Authenticate(r *http.Request) Result {
	realm := b.Realm
	if realm == "" {
		realm = "Flomation Gateway"
	}
	challenge := `Basic realm="` + realm + `"`
	raw := r.Header.Get("Authorization")
	const prefix = "Basic "
	if !strings.HasPrefix(raw, prefix) {
		return Result{Status: http.StatusUnauthorized, Challenge: challenge}
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, prefix))
	if err != nil {
		return Result{Status: http.StatusUnauthorized, Challenge: challenge}
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	if !ok || b.Hash == "" {
		return Result{Status: http.StatusUnauthorized, Challenge: challenge}
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(b.Username)) == 1
	passOK := secretMatches(b.Hash, b.Salt, pass)
	if !userOK || !passOK {
		return Result{Status: http.StatusUnauthorized, Challenge: challenge}
	}
	return Result{OK: true}
}

// ── oidc ──

// OIDC verifies a bearer JWT against a JWKS. The Keyfunc is supplied by the
// caller (cached per jwks_uri so keys aren't refetched every request).
type OIDC struct {
	Issuer         string
	Audience       string
	RequiredClaims map[string]interface{}
	Keyfunc        jwt.Keyfunc
}

// oidcValidMethods restricts accepted signature algorithms — critically excludes
// "none" so an unsigned token can never authenticate.
var oidcValidMethods = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512"}

func (o OIDC) Authenticate(r *http.Request) Result {
	tok := bearer(r)
	if tok == "" || o.Keyfunc == nil {
		return deny(http.StatusUnauthorized)
	}
	opts := []jwt.ParserOption{jwt.WithValidMethods(oidcValidMethods)}
	if o.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(o.Issuer))
	}
	if o.Audience != "" {
		opts = append(opts, jwt.WithAudience(o.Audience))
	}
	parsed, err := jwt.Parse(tok, o.Keyfunc, opts...)
	if err != nil || !parsed.Valid {
		return deny(http.StatusUnauthorized)
	}
	claims, _ := parsed.Claims.(jwt.MapClaims)
	for k, want := range o.RequiredClaims {
		if got, ok := claims[k]; !ok || got != want {
			return deny(http.StatusForbidden)
		}
	}
	return Result{OK: true, Claims: map[string]interface{}(claims)}
}

// ── config ──

// Config is the parsed, NON-secret auth policy (from the API's resolve payload).
type Config struct {
	Header             string                 `json:"header"`
	Username           string                 `json:"username"`
	Realm              string                 `json:"realm"`
	Issuer             string                 `json:"issuer"`
	JWKSURI            string                 `json:"jwks_uri"`
	Audience           string                 `json:"audience"`
	RequiredClaims     map[string]interface{} `json:"required_claims"`
	RequiredPermission string                 `json:"required_permission"`
	RequiredRole       string                 `json:"required_role"`
}

// ParseConfig decodes an auth_config blob (tolerating empty/nil).
func ParseConfig(raw []byte) Config {
	var c Config
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}
