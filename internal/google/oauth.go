// Package google provides Google OAuth2 helpers for the calendar
// integration. It handles token exchange and user info fetching but
// does NOT import the persistence package — DB operations are done
// by the caller (the HTTP handler) to avoid import cycles.
package google

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flomation.app/automate/launch/internal/config"
)

const (
	GoogleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	GoogleTokenURL = "https://oauth2.googleapis.com/token" // #nosec G101 — not a credential, it's a public Google endpoint
	GoogleUserInfo = "https://www.googleapis.com/oauth2/v2/userinfo"
)

// PurposeScopes maps each connection purpose to the exact OAuth scopes
// needed. Each purpose gets its own refresh token with minimal permissions.
var PurposeScopes = map[string]string{
	"calendar":   "https://www.googleapis.com/auth/calendar.readonly https://www.googleapis.com/auth/calendar.events https://www.googleapis.com/auth/userinfo.email",
	"email_read": "https://www.googleapis.com/auth/gmail.readonly https://www.googleapis.com/auth/userinfo.email",
	"email_send": "https://www.googleapis.com/auth/gmail.send https://www.googleapis.com/auth/gmail.compose https://www.googleapis.com/auth/userinfo.email",
	"drive":      "https://www.googleapis.com/auth/drive https://www.googleapis.com/auth/userinfo.email",
}

// DefaultScopes for backwards compatibility (calendar).
const DefaultScopes = "https://www.googleapis.com/auth/calendar.readonly https://www.googleapis.com/auth/calendar.events https://www.googleapis.com/auth/userinfo.email"

// Service provides Google OAuth helpers.
type Service struct {
	Config *config.Config
	Client *http.Client
}

// NewService creates a Google OAuth service.
func NewService(cfg *config.Config) *Service {
	return &Service{
		Config: cfg,
		Client: &http.Client{Timeout: 15 * time.Second},
	}
}

// BuildAuthURL constructs the Google OAuth consent URL with purpose-scoped
// permissions. If purpose is empty or unknown, defaults to calendar scopes.
func (s *Service) BuildAuthURL(state, purpose string) string {
	scopes := PurposeScopes[purpose]
	if scopes == "" {
		scopes = DefaultScopes
	}
	redirectURI := fmt.Sprintf("%s/auth/google/callback", s.Config.PublicURL)
	params := url.Values{
		"client_id":     {s.Config.Google.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {scopes},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return fmt.Sprintf("%s?%s", GoogleAuthURL, params.Encode())
}

// TokenResponse from Google's token endpoint.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// ExchangeCode exchanges an authorization code for tokens.
func (s *Service) ExchangeCode(code string) (*TokenResponse, error) {
	redirectURI := fmt.Sprintf("%s/auth/google/callback", s.Config.PublicURL)
	data := url.Values{
		"code":          {code},
		"client_id":     {s.Config.Google.ClientID},
		"client_secret": {s.Config.Google.ClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}

	resp, err := s.Client.PostForm(GoogleTokenURL, data)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google returned %d: %s", resp.StatusCode, string(body))
	}

	var result TokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// FetchUserEmail gets the email of the authenticated user.
func (s *Service) FetchUserEmail(accessToken string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, GoogleUserInfo, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := s.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	if info.Email == "" {
		return "", fmt.Errorf("no email in userinfo response")
	}
	return info.Email, nil
}

// InferLabel guesses a label ("Work" or "Personal") from the email domain.
func InferLabel(email string) string {
	consumer := map[string]bool{
		"gmail.com": true, "googlemail.com": true, "outlook.com": true,
		"hotmail.com": true, "live.com": true, "yahoo.com": true,
		"icloud.com": true, "me.com": true, "protonmail.com": true,
	}
	parts := strings.SplitN(email, "@", 2)
	if len(parts) == 2 && consumer[strings.ToLower(parts[1])] {
		return "Personal"
	}
	return "Work"
}
