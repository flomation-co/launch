// Package slack: OAuth helpers for "Sign in with Slack" (OpenID Connect).
//
// Used by the R3 Phase 2 identity flow on the profile Identities tab —
// the user clicks "Connect with Slack" and the OIDC flow returns their
// Slack user_id (claim "https://slack.com/user_id"), which we store as
// the user_identity row's external_id for the "slack" channel type.
//
// This is intentionally separate from the existing Events API code in
// this package: the bot-token-based webhook flow is configured per
// agent on its trigger node; OIDC sign-in is application-wide.
package slack

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
	slackOIDCAuthURL     = "https://slack.com/openid/connect/authorize"
	slackOIDCTokenURL    = "https://slack.com/api/openid.connect.token"
	slackOIDCUserInfoURL = "https://slack.com/api/openid.connect.userInfo"
)

// OAuthService provides Slack OIDC helpers. Distinct type name from the
// existing Service (Events API) so both can coexist in this package
// without a rename.
type OAuthService struct {
	Config *config.Config
	Client *http.Client
}

// NewOAuthService creates a Slack OAuth service.
func NewOAuthService(cfg *config.Config) *OAuthService {
	return &OAuthService{
		Config: cfg,
		Client: &http.Client{Timeout: 15 * time.Second},
	}
}

// BuildAuthURL constructs the Slack OIDC consent URL. Scopes are fixed
// for the identity flow (no other Slack OAuth purposes are wired yet).
func (s *OAuthService) BuildAuthURL(state string) string {
	redirectURI := fmt.Sprintf("%s/auth/slack/callback", s.Config.PublicURL)
	params := url.Values{
		"client_id":     {s.Config.Slack.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
	}
	return fmt.Sprintf("%s?%s", slackOIDCAuthURL, params.Encode())
}

// TokenResponse from Slack's OIDC token endpoint.
type TokenResponse struct {
	OK          bool   `json:"ok"`
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error,omitempty"`
}

// ExchangeCode exchanges an authorisation code for tokens.
func (s *OAuthService) ExchangeCode(code string) (*TokenResponse, error) {
	redirectURI := fmt.Sprintf("%s/auth/slack/callback", s.Config.PublicURL)
	form := url.Values{
		"client_id":     {s.Config.Slack.ClientID},
		"client_secret": {s.Config.Slack.ClientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequest(http.MethodPost, slackOIDCTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var out TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("slack OIDC token exchange failed: %s", out.Error)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("slack OIDC token response missing access_token")
	}
	return &out, nil
}

// UserIdentity is the canonical Sign-in-with-Slack identity tuple. The
// `sub` claim is "<user_id>.<team_id>" so we expose the components
// separately — the identity flow uses UserID (matching the Slack
// trigger's user_id output).
type UserIdentity struct {
	UserID      string // Slack user ID (e.g. "U01ABCDE")
	TeamID      string // Slack workspace / team ID (e.g. "T01F2G3H")
	Email       string
	DisplayName string
}

// FetchUserIdentity calls Slack's OIDC userInfo endpoint.
func (s *OAuthService) FetchUserIdentity(accessToken string) (*UserIdentity, error) {
	req, err := http.NewRequest(http.MethodGet, slackOIDCUserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	// Slack's OIDC claims include both standard names ("sub", "email",
	// "name") and Slack-namespaced fields prefixed with the team URL.
	// The user_id / team_id claims are the convenient pre-split form
	// of `sub` which would otherwise come back as "<user>.<team>".
	var info struct {
		OK          bool   `json:"ok"`
		Sub         string `json:"sub"`
		Email       string `json:"email"`
		Name        string `json:"name"`
		SlackUserID string `json:"https://slack.com/user_id"`
		SlackTeamID string `json:"https://slack.com/team_id"`
		Error       string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	if info.Error != "" {
		return nil, fmt.Errorf("slack userInfo error: %s", info.Error)
	}

	uid := info.SlackUserID
	tid := info.SlackTeamID
	if uid == "" || tid == "" {
		// Fall back to splitting `sub` ("<user_id>.<team_id>") if the
		// Slack-namespaced claims weren't returned (older Slack apps).
		if parts := strings.SplitN(info.Sub, ".", 2); len(parts) == 2 {
			if uid == "" {
				uid = parts[0]
			}
			if tid == "" {
				tid = parts[1]
			}
		}
	}
	if uid == "" {
		return nil, fmt.Errorf("slack userInfo response missing user_id")
	}

	return &UserIdentity{
		UserID:      uid,
		TeamID:      tid,
		Email:       info.Email,
		DisplayName: info.Name,
	}, nil
}
