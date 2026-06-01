package http

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"flomation.app/automate/launch/internal/google"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleGoogleAuthInitiate handles GET /auth/google/:agent_user_id.
// Agent-user scoped OAuth flow. The ?purpose= parameter determines
// which scopes are requested.
func (s *Service) handleGoogleAuthInitiate(c *gin.Context) {
	agentUserID := c.Param("agent_user_id")
	agentID := c.Query("agent_id")
	purpose := c.DefaultQuery("purpose", "calendar")
	if agentUserID == "" || agentID == "" {
		c.String(http.StatusBadRequest, "Missing agent_user_id or agent_id")
		return
	}
	if s.google == nil {
		c.String(http.StatusServiceUnavailable, "Google integration is not configured")
		return
	}

	state := generateState()
	if state == "" {
		c.String(http.StatusInternalServerError, "Failed to generate state")
		return
	}

	if err := s.db.CreateGoogleAuthState(state, agentID, agentUserID, purpose, ""); err != nil {
		log.WithError(err).Error("failed to store Google auth state")
		c.String(http.StatusInternalServerError, "Failed to initiate connection")
		return
	}

	authURL := s.google.BuildAuthURL(state, purpose)
	c.Redirect(http.StatusTemporaryRedirect, authURL)
}

// handleGoogleAuthInitiateTrigger handles GET /auth/google/trigger/:trigger_id.
// Trigger-scoped OAuth flow — stores the token against the trigger instead
// of an agent_user. Used by the "Add Account" button in the email trigger
// property editor.
func (s *Service) handleGoogleAuthInitiateTrigger(c *gin.Context) {
	triggerID := c.Param("trigger_id")
	purpose := c.DefaultQuery("purpose", "email_read")
	if triggerID == "" {
		c.String(http.StatusBadRequest, "Missing trigger_id")
		return
	}
	if s.google == nil {
		c.String(http.StatusServiceUnavailable, "Google integration is not configured")
		return
	}

	state := generateState()
	if state == "" {
		c.String(http.StatusInternalServerError, "Failed to generate state")
		return
	}

	if err := s.db.CreateGoogleAuthState(state, "", "", purpose, triggerID); err != nil {
		log.WithError(err).Error("failed to store Google auth state (trigger)")
		c.String(http.StatusInternalServerError, "Failed to initiate connection")
		return
	}

	authURL := s.google.BuildAuthURL(state, purpose)
	c.Redirect(http.StatusTemporaryRedirect, authURL)
}

// handleGoogleAuthCallback handles GET /auth/google/callback.
// Routes the token storage to either agent-user or trigger based on
// what was recorded in the auth state.
func (s *Service) handleGoogleAuthCallback(c *gin.Context) {
	if errParam := c.Query("error"); errParam != "" {
		c.String(http.StatusBadRequest, fmt.Sprintf("Google denied access: %s", errParam))
		return
	}

	state := c.Query("state")
	code := c.Query("code")
	if state == "" || code == "" {
		c.String(http.StatusBadRequest, "Missing state or code")
		return
	}
	if s.google == nil {
		c.String(http.StatusServiceUnavailable, "Google integration is not configured")
		return
	}

	authState, err := s.db.ConsumeGoogleAuthState(state)
	if err != nil {
		c.String(http.StatusBadRequest, fmt.Sprintf("Invalid or expired link: %v", err))
		return
	}

	tokenResp, err := s.google.ExchangeCode(code)
	if err != nil {
		log.WithError(err).Error("Google token exchange failed")
		c.String(http.StatusInternalServerError, fmt.Sprintf("Connection failed: %v", err))
		return
	}
	if tokenResp.RefreshToken == "" {
		c.String(http.StatusBadRequest, "No refresh token received. Please revoke access at https://myaccount.google.com/permissions and try again.")
		return
	}

	email, err := s.google.FetchUserEmail(tokenResp.AccessToken)
	if err != nil {
		log.WithError(err).Error("failed to fetch Google email")
		c.String(http.StatusInternalServerError, "Failed to identify Google account")
		return
	}

	label := google.InferLabel(email)

	payload, _ := json.Marshal(map[string]string{
		"google_email":  email,
		"refresh_token": tokenResp.RefreshToken,
		"label":         label,
		"purpose":       authState.Purpose,
	})

	// Route to the correct storage endpoint based on auth state
	var apiURL string
	if authState.TriggerID != "" {
		apiURL = fmt.Sprintf("%s/api/v1/internal/trigger/%s/google-account",
			s.config.InternalAPIURL(), authState.TriggerID)
	} else {
		apiURL = fmt.Sprintf("%s/api/v1/internal/agent-user/%s/google-account",
			s.config.InternalAPIURL(), authState.AgentUserID)
	}

	apiResp, err := s.apiClient.Post(apiURL, "application/json", bytes.NewReader(payload)) // #nosec G107 — internal service-to-service call
	if err != nil {
		log.WithError(err).Error("failed to store Google token via API")
		c.String(http.StatusInternalServerError, "Failed to save connection")
		return
	}
	defer func() { _ = apiResp.Body.Close() }()

	if apiResp.StatusCode != http.StatusCreated && apiResp.StatusCode != http.StatusOK {
		c.String(http.StatusInternalServerError, "Failed to save connection")
		return
	}

	scope := "agent"
	if authState.TriggerID != "" {
		scope = "trigger " + authState.TriggerID
	}
	log.WithFields(log.Fields{
		"scope":   scope,
		"email":   email,
		"label":   label,
		"purpose": authState.Purpose,
	}).Info("Google account connected")

	// Purpose-specific display title
	purposeTitle := map[string]string{
		"calendar":   "Calendar Connected",
		"email_read": "Email Read Access Connected",
		"email_send": "Email Send Access Connected",
		"drive":      "Drive Access Connected",
	}
	title := purposeTitle[authState.Purpose]
	if title == "" {
		title = "Account Connected"
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>%s</title>
<style>body{font-family:system-ui;display:flex;align-items:center;justify-content:center;min-height:100vh;background:#111;color:#fff;margin:0}
.card{text-align:center;padding:40px;background:#1a1a1a;border-radius:16px;border:1px solid rgba(255,255,255,0.1)}
h1{color:#00aa9c;margin-bottom:8px}p{color:rgba(255,255,255,0.6)}.email{color:#c084fc}</style></head>
<body><div class="card"><h1>%s</h1><p><span class="email">%s</span> has been linked as <strong>%s</strong>.</p><p>You can close this tab and return to the conversation.</p></div></body></html>`,
		title, title,
		strings.ReplaceAll(email, "<", "&lt;"), label))
}

func generateState() string {
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return ""
	}
	return hex.EncodeToString(stateBytes)
}
