// Package microsoft provides Microsoft OAuth2 helpers for the
// Microsoft 365 integration. It handles token exchange and user info
// fetching but does NOT import the persistence package — DB operations
// are done by the caller (the HTTP handler) to avoid import cycles.
package microsoft

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
	MicrosoftAuthURL  = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	MicrosoftTokenURL = "https://login.microsoftonline.com/common/oauth2/v2.0/token" // #nosec G101 — public Microsoft endpoint
	MicrosoftGraphMe  = "https://graph.microsoft.com/v1.0/me"
)

// PurposeScopes maps each connection purpose to the exact OAuth scopes
// needed. Each purpose gets its own refresh token with minimal permissions.
var PurposeScopes = map[string]string{
	"mail_read":  "offline_access Mail.Read Mail.ReadBasic User.Read",
	"mail_send":  "offline_access Mail.Send Mail.ReadWrite User.Read",
	"mail_full":  "offline_access Mail.ReadWrite Mail.Send User.Read",
	"onedrive":   "offline_access Files.ReadWrite.All User.Read",
	"excel":      "offline_access Files.ReadWrite.All User.Read",
	"teams":      "offline_access ChannelMessage.Send Team.ReadBasic.All Channel.ReadBasic.All Chat.ReadWrite User.Read",
	"sharepoint": "offline_access Sites.ReadWrite.All User.Read",
	"calendar":   "offline_access Calendars.ReadWrite User.Read",
}

// DefaultScopes for general-purpose access.
const DefaultScopes = "offline_access Mail.Read User.Read"

// Service provides Microsoft OAuth helpers.
type Service struct {
	Config *config.Config
	Client *http.Client
}

// NewService creates a Microsoft OAuth service.
func NewService(cfg *config.Config) *Service {
	return &Service{
		Config: cfg,
		Client: &http.Client{Timeout: 15 * time.Second},
	}
}

// BuildAuthURL constructs the Microsoft OAuth consent URL with purpose-scoped
// permissions. If purpose is empty or unknown, defaults to mail_read scopes.
func (s *Service) BuildAuthURL(state, purpose string) string {
	scopes := PurposeScopes[purpose]
	if scopes == "" {
		scopes = DefaultScopes
	}
	redirectURI := fmt.Sprintf("%s/auth/microsoft/callback", s.Config.PublicURL)

	tenantID := "common"
	if s.Config.Microsoft != nil && s.Config.Microsoft.TenantID != "" {
		tenantID = s.Config.Microsoft.TenantID
	}

	authURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/authorize", tenantID)

	params := url.Values{
		"client_id":     {s.Config.Microsoft.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {scopes},
		"response_mode": {"query"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return fmt.Sprintf("%s?%s", authURL, params.Encode())
}

// TokenResponse from Microsoft's token endpoint.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// ExchangeCode exchanges an authorisation code for tokens.
func (s *Service) ExchangeCode(code string) (*TokenResponse, error) {
	redirectURI := fmt.Sprintf("%s/auth/microsoft/callback", s.Config.PublicURL)

	tenantID := "common"
	if s.Config.Microsoft != nil && s.Config.Microsoft.TenantID != "" {
		tenantID = s.Config.Microsoft.TenantID
	}

	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID)

	data := url.Values{
		"code":          {code},
		"client_id":     {s.Config.Microsoft.ClientID},
		"client_secret": {s.Config.Microsoft.ClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}

	resp, err := s.Client.PostForm(tokenURL, data)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("microsoft returned %d: %s", resp.StatusCode, string(body))
	}

	var result TokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// RefreshAccessToken uses a refresh token to obtain a new access token.
func (s *Service) RefreshAccessToken(refreshToken string) (*TokenResponse, error) {
	tenantID := "common"
	if s.Config.Microsoft != nil && s.Config.Microsoft.TenantID != "" {
		tenantID = s.Config.Microsoft.TenantID
	}

	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID)

	data := url.Values{
		"client_id":     {s.Config.Microsoft.ClientID},
		"client_secret": {s.Config.Microsoft.ClientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}

	resp, err := s.Client.PostForm(tokenURL, data)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("microsoft token refresh returned %d: %s", resp.StatusCode, string(body))
	}

	var result TokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// FetchUserEmail gets the email and display name of the authenticated user
// from the Microsoft Graph /me endpoint.
func (s *Service) FetchUserEmail(accessToken string) (string, string, error) {
	req, err := http.NewRequest(http.MethodGet, MicrosoftGraphMe, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := s.Client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var info struct {
		Mail              string `json:"mail"`
		UserPrincipalName string `json:"userPrincipalName"`
		DisplayName       string `json:"displayName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", "", err
	}

	email := info.Mail
	if email == "" {
		email = info.UserPrincipalName
	}
	if email == "" {
		return "", "", fmt.Errorf("no email in Microsoft Graph /me response")
	}
	return email, info.DisplayName, nil
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
