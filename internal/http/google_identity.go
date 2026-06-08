package http

import (
	"net/http"
	"strings"

	"github.com/flomation-co/sentinel-client"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleGoogleAuthInitiateIdentity handles GET /auth/google/identity —
// the entry point for the R3 Phase 2 user-identity OAuth flow. The
// editor opens this URL in a popup; the user authenticates with Google;
// the callback (shared with the existing agent/trigger flows) writes
// a row to user_identity via the API's internal endpoint instead of
// storing a credential token.
//
// Required query params:
//   - channel_type: the user_identity row's channel_type (e.g. "email")
//
// Optional query params:
//   - organisation_id: scopes the declaration to an org; absent = personal mode
//
// Authentication: reads the flomation-token cookie (browser-driven popup
// flow — Bearer header isn't available across cross-domain redirects).
func (s *Service) handleGoogleAuthInitiateIdentity(c *gin.Context) {
	channelType := c.Query("channel_type")
	organisationID := c.Query("organisation_id")
	if channelType == "" {
		c.String(http.StatusBadRequest, "Missing channel_type")
		return
	}
	if s.google == nil {
		c.String(http.StatusServiceUnavailable, "Google integration is not configured")
		return
	}

	userID, err := s.identityFromCookieOrHeader(c)
	if err != nil || userID == "" {
		log.WithError(err).Warn("identity OAuth initiate without valid session")
		c.String(http.StatusUnauthorized, "Sign in first; this page must be opened from the editor.")
		return
	}

	state := generateState()
	if state == "" {
		c.String(http.StatusInternalServerError, "Failed to generate state")
		return
	}

	if err := s.db.CreateGoogleAuthStateIdentity(state, userID, organisationID, channelType); err != nil {
		log.WithError(err).Error("failed to store Google auth state (identity)")
		c.String(http.StatusInternalServerError, "Failed to initiate connection")
		return
	}

	// "identity" purpose maps to PurposeScopes — Google.BuildAuthURL will
	// request just openid + email + profile (enough to identify the user).
	authURL := s.google.BuildAuthURL(state, "identity")
	c.Redirect(http.StatusTemporaryRedirect, authURL)
}

// identityFromCookieOrHeader resolves the calling user via the
// flomation-token cookie (browser popup flow), falling back to the
// Authorization header for completeness. Returns an empty string with a
// non-nil error if neither source produces a valid token.
func (s *Service) identityFromCookieOrHeader(c *gin.Context) (string, error) {
	var token string
	if cookie, err := c.Cookie("flomation-token"); err == nil && cookie != "" {
		token = cookie
	} else if header := c.GetHeader("Authorization"); header != "" {
		parts := strings.SplitN(header, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			token = parts[1]
		}
	}
	if token == "" {
		return "", nil
	}
	if s.config.Security.IdentityService == "" {
		return "", nil
	}
	userID, err := sentinel.GetUser(s.config.Security.IdentityService, token)
	if err != nil {
		return "", err
	}
	if userID == nil {
		return "", nil
	}
	return *userID, nil
}
