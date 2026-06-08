package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"flomation.app/automate/launch/internal/facebook"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleFacebookAuthInitiateIdentity handles GET /auth/facebook/identity —
// the entry point for the R3 Phase 2 Facebook identity flow
// ("Login with Facebook"). Mirrors the Google / Microsoft / Slack
// equivalents.
func (s *Service) handleFacebookAuthInitiateIdentity(c *gin.Context) {
	channelType := c.Query("channel_type")
	organisationID := c.Query("organisation_id")
	if channelType == "" {
		c.String(http.StatusBadRequest, "Missing channel_type")
		return
	}
	if s.config.Facebook == nil || s.config.Facebook.ClientID == "" {
		c.String(http.StatusServiceUnavailable, "Facebook integration is not configured")
		return
	}

	userID, err := s.identityFromCookieOrHeader(c)
	if err != nil || userID == "" {
		log.WithError(err).Warn("facebook identity OAuth initiate without valid session")
		c.String(http.StatusUnauthorized, "Sign in first; this page must be opened from the editor.")
		return
	}

	state := generateState()
	if state == "" {
		c.String(http.StatusInternalServerError, "Failed to generate state")
		return
	}

	if err := s.db.CreateFacebookAuthStateIdentity(state, userID, organisationID, channelType); err != nil {
		log.WithError(err).Error("failed to store Facebook auth state (identity)")
		c.String(http.StatusInternalServerError, "Failed to initiate connection")
		return
	}

	oauth := facebook.NewOAuthService(s.config)
	c.Redirect(http.StatusTemporaryRedirect, oauth.BuildAuthURL(state))
}

// handleFacebookAuthCallback handles GET /auth/facebook/callback —
// completes the Login-with-Facebook handshake, resolves the App-Scoped
// User ID, and writes a user_identity row via the API's internal
// endpoint.
func (s *Service) handleFacebookAuthCallback(c *gin.Context) {
	if errParam := c.Query("error"); errParam != "" {
		c.String(http.StatusBadRequest, fmt.Sprintf("Facebook denied access: %s", errParam))
		return
	}

	state := c.Query("state")
	code := c.Query("code")
	if state == "" || code == "" {
		c.String(http.StatusBadRequest, "Missing state or code")
		return
	}
	if s.config.Facebook == nil {
		c.String(http.StatusServiceUnavailable, "Facebook integration is not configured")
		return
	}

	authState, err := s.db.ConsumeFacebookAuthState(state)
	if err != nil {
		c.String(http.StatusBadRequest, fmt.Sprintf("Invalid or expired link: %v", err))
		return
	}

	oauth := facebook.NewOAuthService(s.config)
	tokenResp, err := oauth.ExchangeCode(code)
	if err != nil {
		log.WithError(err).Error("Facebook token exchange failed")
		c.String(http.StatusInternalServerError, fmt.Sprintf("Connection failed: %v", err))
		return
	}

	ident, err := oauth.FetchUserIdentity(tokenResp.AccessToken)
	if err != nil {
		log.WithError(err).Error("failed to fetch Facebook user identity")
		c.String(http.StatusInternalServerError, "Failed to identify Facebook account")
		return
	}

	displayName := ident.DisplayName
	if displayName == "" {
		displayName = ident.Email
	}

	body := map[string]interface{}{
		"user_id":      authState.UserID,
		"channel_type": authState.ChannelType,
		"external_id":  ident.ID,
		"display_name": displayName,
	}
	if authState.OrganisationID != "" {
		body["organisation_id"] = authState.OrganisationID
	}
	payload, _ := json.Marshal(body)

	apiURL := fmt.Sprintf("%s/api/v1/internal/user-identity", s.config.InternalAPIURL())
	apiResp, err := s.apiClient.Post(apiURL, "application/json", bytes.NewReader(payload)) // #nosec G107 -- internal service-to-service call
	if err != nil {
		log.WithError(err).Error("failed to write Facebook user identity via API")
		c.String(http.StatusInternalServerError, "Failed to save connection")
		return
	}
	defer func() { _ = apiResp.Body.Close() }()

	if apiResp.StatusCode != http.StatusCreated && apiResp.StatusCode != http.StatusOK && apiResp.StatusCode != http.StatusConflict {
		c.String(http.StatusInternalServerError, "Failed to save connection")
		return
	}

	log.WithFields(log.Fields{
		"user_id":     authState.UserID,
		"facebook_id": ident.ID,
		"display":     displayName,
	}).Info("Facebook identity connected")

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, `<!doctype html><html><head><title>Connected</title></head>
<body style="font-family: -apple-system, sans-serif; background: #1a1a1f; color: #e0e0e0; display: flex; flex-direction: column; align-items: center; justify-content: center; height: 100vh; text-align: center;">
<h2 style="color: #00aa9c;">Connected ✓</h2>
<p>You can close this window.</p>
<script>setTimeout(function(){ window.close(); }, 600);</script>
</body></html>`)
}
