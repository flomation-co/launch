package http

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flomation.app/automate/launch/internal/google"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleGoogleTokens handles GET /internal/google/tokens/:agent_user_id.
// Called by the executor's calendar tool actions at runtime. Fetches
// refresh tokens from the API, exchanges each for an access token using
// Launch's Google credentials, and returns the access tokens. The Google
// client_id/client_secret never leave Launch.
func (s *Service) handleGoogleTokens(c *gin.Context) {
	agentUserID := c.Param("agent_user_id")
	if agentUserID == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if s.google == nil || s.config.Google == nil {
		c.JSON(http.StatusOK, []tokenResponse{})
		return
	}

	apiURL := fmt.Sprintf("%s/api/v1/internal/agent-user/%s/google-refresh-tokens", s.config.InternalAPIURL(), agentUserID)
	if purpose := c.Query("purpose"); purpose != "" {
		apiURL += "?purpose=" + purpose
	}
	s.refreshAndRespond(c, apiURL)
}

// refreshAndRespond fetches raw refresh tokens from the given API URL,
// exchanges each for an access token, and returns the results as JSON.
// Shared by both agent-user and trigger-scoped token handlers.
func (s *Service) refreshAndRespond(c *gin.Context, apiURL string) {
	apiResp, err := s.apiClient.Get(apiURL) // #nosec G107 — internal service-to-service call
	if err != nil {
		log.WithError(err).Error("failed to fetch Google tokens from API")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	defer func() { _ = apiResp.Body.Close() }()

	if apiResp.StatusCode != http.StatusOK {
		c.JSON(http.StatusOK, []tokenResponse{})
		return
	}

	type apiAccount struct {
		Email        string `json:"email"`
		Label        string `json:"label"`
		RefreshToken string `json:"refresh_token"`
	}

	var accounts []apiAccount
	if err := json.NewDecoder(apiResp.Body).Decode(&accounts); err != nil {
		log.WithError(err).Error("failed to decode Google tokens from API")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	var results []tokenResponse
	for _, acct := range accounts {
		if acct.RefreshToken == "" {
			results = append(results, tokenResponse{
				Email: acct.Email,
				Label: acct.Label,
				Error: "no refresh token",
			})
			continue
		}

		accessToken, err := refreshGoogleToken(
			acct.RefreshToken,
			s.config.Google.ClientID,
			s.config.Google.ClientSecret,
		)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
				"email": acct.Email,
			}).Warn("failed to refresh Google token")
			results = append(results, tokenResponse{
				Email: acct.Email,
				Label: acct.Label,
				Error: fmt.Sprintf("refresh failed: %v", err),
			})
			continue
		}

		results = append(results, tokenResponse{
			Email:       acct.Email,
			Label:       acct.Label,
			AccessToken: accessToken,
		})
	}

	c.JSON(http.StatusOK, results)
}

// handleGoogleTokensTrigger handles GET /internal/google/tokens/trigger/:id.
// Same as handleGoogleTokens but fetches from trigger-scoped accounts.
func (s *Service) handleGoogleTokensTrigger(c *gin.Context) {
	triggerID := c.Param("id")
	if triggerID == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if s.google == nil || s.config.Google == nil {
		c.JSON(http.StatusOK, []tokenResponse{})
		return
	}

	apiURL := fmt.Sprintf("%s/api/v1/internal/trigger/%s/google-refresh-tokens", s.config.InternalAPIURL(), triggerID)
	if purpose := c.Query("purpose"); purpose != "" {
		apiURL += "?purpose=" + purpose
	}
	s.refreshAndRespond(c, apiURL)
}

type tokenResponse struct {
	Email       string `json:"email"`
	Label       string `json:"label,omitempty"`
	AccessToken string `json:"access_token,omitempty"`
	Error       string `json:"error,omitempty"`
}

func refreshGoogleToken(refreshToken, clientID, clientSecret string) (string, error) {
	data := url.Values{
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"grant_type":    {"refresh_token"},
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(google.GoogleTokenURL, "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("no access_token in response")
	}
	return result.AccessToken, nil
}
