package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"flomation.app/automate/launch/internal/microsoft"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleMicrosoftAuthInitiateIdentity handles GET /auth/microsoft/identity —
// entry point for the R3 Phase 2 Microsoft identity flow. Mirrors the
// Google equivalent: validates the user via the flomation-token cookie,
// stores state in microsoft_auth_state, redirects to Microsoft consent.
func (s *Service) handleMicrosoftAuthInitiateIdentity(c *gin.Context) {
	channelType := c.Query("channel_type")
	organisationID := c.Query("organisation_id")
	if channelType == "" {
		c.String(http.StatusBadRequest, "Missing channel_type")
		return
	}
	if s.config.Microsoft == nil || s.config.Microsoft.ClientID == "" {
		c.String(http.StatusServiceUnavailable, "Microsoft integration is not configured")
		return
	}

	userID, err := s.identityFromCookieOrHeader(c)
	if err != nil || userID == "" {
		log.WithError(err).Warn("microsoft identity OAuth initiate without valid session")
		c.String(http.StatusUnauthorized, "Sign in first; this page must be opened from the editor.")
		return
	}

	state := generateState()
	if state == "" {
		c.String(http.StatusInternalServerError, "Failed to generate state")
		return
	}

	if err := s.db.CreateMicrosoftAuthStateIdentity(state, userID, organisationID, channelType); err != nil {
		log.WithError(err).Error("failed to store Microsoft auth state (identity)")
		c.String(http.StatusInternalServerError, "Failed to initiate connection")
		return
	}

	ms := microsoft.NewService(s.config)
	authURL := ms.BuildAuthURL(state, "identity")
	c.Redirect(http.StatusTemporaryRedirect, authURL)
}

// handleMicrosoftAuthCallback handles GET /auth/microsoft/callback —
// shared callback for any Microsoft OAuth flow. Currently only identity
// is wired (agent/trigger Microsoft OAuth flows aren't live yet); future
// flows can add cases here alongside the identity routing.
func (s *Service) handleMicrosoftAuthCallback(c *gin.Context) {
	if errParam := c.Query("error"); errParam != "" {
		c.String(http.StatusBadRequest, fmt.Sprintf("Microsoft denied access: %s", errParam))
		return
	}

	state := c.Query("state")
	code := c.Query("code")
	if state == "" || code == "" {
		c.String(http.StatusBadRequest, "Missing state or code")
		return
	}
	if s.config.Microsoft == nil {
		c.String(http.StatusServiceUnavailable, "Microsoft integration is not configured")
		return
	}

	authState, err := s.db.ConsumeMicrosoftAuthState(state)
	if err != nil {
		c.String(http.StatusBadRequest, fmt.Sprintf("Invalid or expired link: %v", err))
		return
	}

	ms := microsoft.NewService(s.config)
	tokenResp, err := ms.ExchangeCode(code)
	if err != nil {
		log.WithError(err).Error("Microsoft token exchange failed")
		c.String(http.StatusInternalServerError, fmt.Sprintf("Connection failed: %v", err))
		return
	}

	ident, err := ms.FetchUserIdentity(tokenResp.AccessToken)
	if err != nil {
		log.WithError(err).Error("failed to fetch Microsoft user identity")
		c.String(http.StatusInternalServerError, "Failed to identify Microsoft account")
		return
	}

	// The Teams trigger emits AAD Object ID as its user_id and the API
	// normalises the inbound channel_type from "teams" to "microsoft"
	// (see normaliseChannelType in api/internal/agent/inbound.go), so
	// the identity row records the AAD Object ID under channel_type
	// "microsoft" — covering Teams today and any future Microsoft
	// surface that also identifies users by AAD Object ID.
	externalID := ident.ID
	if externalID == "" {
		c.String(http.StatusBadRequest, "Microsoft did not return a usable identifier")
		return
	}

	body := map[string]interface{}{
		"user_id":      authState.UserID,
		"channel_type": authState.ChannelType,
		"external_id":  externalID,
		"display_name": ident.DisplayName,
	}
	if authState.OrganisationID != "" {
		body["organisation_id"] = authState.OrganisationID
	}
	payload, _ := json.Marshal(body)

	apiURL := fmt.Sprintf("%s/api/v1/internal/user-identity", s.config.InternalAPIURL())
	apiResp, err := s.apiClient.Post(apiURL, "application/json", bytes.NewReader(payload)) // #nosec G107 — internal service-to-service call
	if err != nil {
		log.WithError(err).Error("failed to write Microsoft user identity via API")
		c.String(http.StatusInternalServerError, "Failed to save connection")
		return
	}
	defer func() { _ = apiResp.Body.Close() }()

	if apiResp.StatusCode != http.StatusCreated && apiResp.StatusCode != http.StatusOK && apiResp.StatusCode != http.StatusConflict {
		c.String(http.StatusInternalServerError, "Failed to save connection")
		return
	}

	log.WithFields(log.Fields{
		"user_id":      authState.UserID,
		"channel":      authState.ChannelType,
		"external_id":  externalID,
		"display_name": ident.DisplayName,
	}).Info("Microsoft identity connected")

	// Lightweight HTML page that closes the popup. Editor polls for
	// popup.closed and refetches identities — same UX as Google.
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, `<!doctype html><html><head><title>Connected</title></head>
<body style="font-family: -apple-system, sans-serif; background: #1a1a1f; color: #e0e0e0; display: flex; flex-direction: column; align-items: center; justify-content: center; height: 100vh; text-align: center;">
<h2 style="color: #00aa9c;">Connected ✓</h2>
<p>You can close this window.</p>
<script>setTimeout(function(){ window.close(); }, 600);</script>
</body></html>`)
}
