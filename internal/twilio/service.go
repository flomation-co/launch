// Package twilio handles Twilio API interactions for SMS and voice.
// Provides webhook signature validation, SMS sending, and phone number
// formatting utilities.
package twilio

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 — required by Twilio's HMAC-SHA1 signature validation
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	twilioAPIBase = "https://api.twilio.com/2010-04-01"
	httpTimeout   = 15 * time.Second
)

// Service manages Twilio interactions for agents.
type Service struct {
	mu          sync.RWMutex
	webhookBase string            // e.g. "https://launch.flomation.app"
	registered  map[string]string // agentID → phone number
	client      *http.Client
}

// NewService creates a Twilio service. webhookBase is the public URL
// of the Launch service.
func NewService(webhookBase string) *Service {
	return &Service{
		webhookBase: webhookBase,
		registered:  make(map[string]string),
		client:      &http.Client{Timeout: httpTimeout},
	}
}

// SendSMS sends an SMS message via the Twilio REST API.
func (s *Service) SendSMS(accountSID, authToken, from, to, body string) (string, error) {
	endpoint := fmt.Sprintf("%s/Accounts/%s/Messages.json", twilioAPIBase, accountSID)

	data := url.Values{}
	data.Set("From", from)
	data.Set("To", to)
	data.Set("Body", body)

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.SetBasicAuth(accountSID, authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send SMS: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("twilio returned %d: %s", resp.StatusCode, string(respBody))
	}

	// Extract message SID from response (simplified — full JSON parsing if needed)
	// Response is JSON with "sid" field
	sid := extractJSONField(respBody, "sid")
	return sid, nil
}

// ValidateSignature verifies the X-Twilio-Signature header using HMAC-SHA1.
// See: https://www.twilio.com/docs/usage/security#validating-requests
func ValidateSignature(authToken, requestURL, signature string, params map[string]string) bool {
	// Sort parameter keys alphabetically
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Concatenate URL + sorted key-value pairs
	var sb strings.Builder
	sb.WriteString(requestURL)
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString(params[k])
	}

	// HMAC-SHA1
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(sb.String()))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

// NormaliseE164 ensures a phone number is in E.164 format.
func NormaliseE164(number string) string {
	number = strings.TrimSpace(number)
	if number == "" {
		return number
	}
	if !strings.HasPrefix(number, "+") {
		number = "+" + number
	}
	return number
}

// RegisterAgent records that an agent has a Twilio channel configured.
func (s *Service) RegisterAgent(agentID, phoneNumber string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered[agentID] = phoneNumber
	log.WithFields(log.Fields{
		"agent_id": agentID,
		"phone":    phoneNumber,
	}).Info("Twilio channel registered")
}

// DeregisterAgent removes a Twilio channel registration.
func (s *Service) DeregisterAgent(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.registered, agentID)
}

// extractJSONField is a simple helper to extract a string field from JSON bytes
// without a full unmarshal. Used for extracting "sid" from Twilio responses.
func extractJSONField(data []byte, field string) string {
	needle := fmt.Sprintf(`"%s": "`, field)
	s := string(data)
	idx := strings.Index(s, needle)
	if idx == -1 {
		// Try without space after colon
		needle = fmt.Sprintf(`"%s":"`, field)
		idx = strings.Index(s, needle)
		if idx == -1 {
			return ""
		}
	}
	start := idx + len(needle)
	end := strings.Index(s[start:], `"`)
	if end == -1 {
		return ""
	}
	return s[start : start+end]
}
