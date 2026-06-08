// Package facebook: OAuth helpers for "Login with Facebook".
//
// Used by the R3 Phase 2 identity flow on the profile Identities tab —
// the user clicks "Connect with Facebook" and the OAuth flow returns
// their App-Scoped User ID (ASID). For apps with Page-Scoped IDs
// enabled (the default for newer Facebook apps) the ASID equals the
// Page-Scoped User ID (PSID) that Messenger webhooks emit as
// sender.id — so storing it as user_identity.external_id lets the
// inbound Messenger webhook resolver match declared identities.
//
// This is intentionally separate from the existing Messenger webhook
// code in this package: webhook verification uses AppSecret /
// VerifyToken; identity OAuth uses ClientID / ClientSecret. Both
// credential pairs live on the same FacebookConfig block.
package facebook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"flomation.app/automate/launch/internal/config"
)

const (
	facebookOAuthAuthURL  = "https://www.facebook.com/v18.0/dialog/oauth"
	facebookOAuthTokenURL = "https://graph.facebook.com/v18.0/oauth/access_token" // #nosec G101 -- public Facebook OAuth token endpoint URL, not a credential
	facebookGraphMeURL    = "https://graph.facebook.com/v18.0/me"
)

// OAuthService provides Facebook Login OAuth helpers. Distinct type
// name from any existing Messenger-webhook types so both surfaces can
// coexist in this package.
type OAuthService struct {
	Config *config.Config
	Client *http.Client
}

// NewOAuthService creates a Facebook OAuth service.
func NewOAuthService(cfg *config.Config) *OAuthService {
	return &OAuthService{
		Config: cfg,
		Client: &http.Client{Timeout: 15 * time.Second},
	}
}

// BuildAuthURL constructs the Facebook OAuth consent URL.
func (s *OAuthService) BuildAuthURL(state string) string {
	redirectURI := fmt.Sprintf("%s/auth/facebook/callback", s.Config.PublicURL)
	params := url.Values{
		"client_id":     {s.Config.Facebook.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {"email,public_profile"},
		"state":         {state},
	}
	return fmt.Sprintf("%s?%s", facebookOAuthAuthURL, params.Encode())
}

// TokenResponse from Facebook's OAuth token endpoint.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in,omitempty"`
}

// ExchangeCode exchanges an authorisation code for an access token.
func (s *OAuthService) ExchangeCode(code string) (*TokenResponse, error) {
	redirectURI := fmt.Sprintf("%s/auth/facebook/callback", s.Config.PublicURL)
	params := url.Values{
		"client_id":     {s.Config.Facebook.ClientID},
		"client_secret": {s.Config.Facebook.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	exchangeURL := fmt.Sprintf("%s?%s", facebookOAuthTokenURL, params.Encode())

	resp, err := s.Client.Get(exchangeURL) // #nosec G107 -- URL built from configured trusted endpoint + caller-supplied code
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var fbErr struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    int    `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&fbErr)
		return nil, fmt.Errorf("facebook token exchange failed: %s (%s)", fbErr.Error.Message, fbErr.Error.Type)
	}

	var out TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("facebook token response missing access_token")
	}
	return &out, nil
}

// UserIdentity is the canonical Login-with-Facebook identity tuple.
// ID is the App-Scoped User ID (= PSID when the app has Page-Scoped IDs
// enabled, which matches what Messenger webhooks deliver).
type UserIdentity struct {
	ID          string // App-Scoped User ID
	Email       string // optional; user may have declined email permission
	DisplayName string
}

// FetchUserIdentity calls Facebook's Graph /me endpoint to resolve the
// signed-in user.
func (s *OAuthService) FetchUserIdentity(accessToken string) (*UserIdentity, error) {
	params := url.Values{
		"fields":       {"id,name,email"},
		"access_token": {accessToken},
	}
	meURL := fmt.Sprintf("%s?%s", facebookGraphMeURL, params.Encode())

	resp, err := s.Client.Get(meURL) // #nosec G107 -- URL built from configured trusted endpoint + bearer access token
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("facebook /me returned HTTP %d", resp.StatusCode)
	}

	var info struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	if info.ID == "" {
		return nil, fmt.Errorf("facebook /me response missing id")
	}

	return &UserIdentity{
		ID:          info.ID,
		Email:       info.Email,
		DisplayName: info.Name,
	}, nil
}
