// Package linkedin: OAuth helpers for "Sign in with LinkedIn".
//
// Used by the R3 Phase 2 identity flow on the profile Identities tab —
// the user clicks "Connect with LinkedIn" and the OAuth flow returns
// their LinkedIn member ID (the `sub` claim from OIDC userinfo). This
// mirrors the LinkedIn provider that already lives in Sentinel for
// social login (shared/sentinel/internal/listener/oauth.go).
//
// Distinct from the existing trigger-polling Service in this package
// (which polls LinkedIn posts for comments/reactions and needs no user
// identity).
package linkedin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flomation.app/automate/launch/internal/config"
)

const (
	linkedinOAuthAuthURL  = "https://www.linkedin.com/oauth/v2/authorization"
	linkedinOAuthTokenURL = "https://www.linkedin.com/oauth/v2/accessToken" // #nosec G101 -- public LinkedIn OAuth token endpoint URL, not a credential
	linkedinUserInfoURL   = "https://api.linkedin.com/v2/userinfo"
)

// OAuthService provides LinkedIn OIDC helpers. Distinct type name from
// the existing trigger-polling Service so both can coexist in this
// package.
type OAuthService struct {
	Config *config.Config
	Client *http.Client
}

// NewOAuthService creates a LinkedIn OAuth service.
func NewOAuthService(cfg *config.Config) *OAuthService {
	return &OAuthService{
		Config: cfg,
		Client: &http.Client{Timeout: 15 * time.Second},
	}
}

// BuildAuthURL constructs the LinkedIn OIDC consent URL.
func (s *OAuthService) BuildAuthURL(state string) string {
	redirectURI := fmt.Sprintf("%s/auth/linkedin/callback", s.Config.PublicURL)
	params := url.Values{
		"client_id":     {s.Config.LinkedIn.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {"openid profile email"},
		"state":         {state},
	}
	return fmt.Sprintf("%s?%s", linkedinOAuthAuthURL, params.Encode())
}

// TokenResponse from LinkedIn's OAuth token endpoint.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type,omitempty"`
	IDToken     string `json:"id_token,omitempty"`
}

// ExchangeCode exchanges an authorisation code for an access token.
func (s *OAuthService) ExchangeCode(code string) (*TokenResponse, error) {
	redirectURI := fmt.Sprintf("%s/auth/linkedin/callback", s.Config.PublicURL)
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {s.Config.LinkedIn.ClientID},
		"client_secret": {s.Config.LinkedIn.ClientSecret},
	}
	req, err := http.NewRequest(http.MethodPost, linkedinOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linkedin token exchange failed: HTTP %d", resp.StatusCode)
	}

	var out TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("linkedin token response missing access_token")
	}
	return &out, nil
}

// UserIdentity is the canonical Sign-in-with-LinkedIn identity tuple.
// Sub is the LinkedIn member ID (a stable per-member identifier scoped
// to your LinkedIn app — sometimes called the "member URN" suffix).
type UserIdentity struct {
	Sub         string // LinkedIn member ID
	Email       string
	DisplayName string
}

// FetchUserIdentity calls LinkedIn's OIDC userinfo endpoint. Matches
// Sentinel's fetchLinkedInProfile implementation byte-for-byte (same
// endpoint, same expected response shape).
func (s *OAuthService) FetchUserIdentity(accessToken string) (*UserIdentity, error) {
	req, err := http.NewRequest(http.MethodGet, linkedinUserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linkedin userinfo returned HTTP %d", resp.StatusCode)
	}

	var info struct {
		Sub   string `json:"sub"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	if info.Sub == "" {
		return nil, fmt.Errorf("linkedin userinfo response missing sub")
	}

	return &UserIdentity{
		Sub:         info.Sub,
		Email:       info.Email,
		DisplayName: info.Name,
	}, nil
}
