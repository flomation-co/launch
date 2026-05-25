package facebook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const graphAPIBase = "https://graph.facebook.com/v19.0" // #nosec G101

var httpClient = &http.Client{Timeout: 15 * time.Second}

// appsecretProof computes the HMAC-SHA256 proof required by Facebook's
// "Require App Secret" setting. Returns empty string if no secret.
func appsecretProof(appSecret, accessToken string) string {
	if appSecret == "" || accessToken == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte(accessToken))
	return hex.EncodeToString(mac.Sum(nil))
}

// appendProof adds appsecret_proof query parameter to a URL.
func appendProof(apiURL, appSecret, accessToken string) string {
	proof := appsecretProof(appSecret, accessToken)
	if proof == "" {
		return apiURL
	}
	return apiURL + "&appsecret_proof=" + proof
}

// pageTokenCache caches page access tokens derived from user tokens.
// Entries expire after 30 minutes to pick up token refreshes.
var pageTokenCache = struct {
	mu      sync.RWMutex
	entries map[string]cachedPageToken // key: userToken + ":" + pageID
}{entries: make(map[string]cachedPageToken)}

type cachedPageToken struct {
	Token     string
	ExpiresAt time.Time
}

const pageTokenCacheTTL = 30 * time.Minute

// SendMessage sends a text message to a Messenger user via the Page.
func SendMessage(pageAccessToken, appSecret, recipientPSID, text string) error {
	payload := map[string]interface{}{
		"recipient":      map[string]string{"id": recipientPSID},
		"messaging_type": "RESPONSE",
		"message":        map[string]string{"text": text},
	}
	return sendAPI(pageAccessToken, appSecret, payload)
}

// SendAction sends a sender action (e.g. "typing_on") to a Messenger user.
func SendAction(pageAccessToken, appSecret, recipientPSID, action string) error {
	payload := map[string]interface{}{
		"recipient":     map[string]string{"id": recipientPSID},
		"sender_action": action,
	}
	return sendAPI(pageAccessToken, appSecret, payload)
}

func sendAPI(pageAccessToken, secret string, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	url := appendProof(
		fmt.Sprintf("%s/me/messages?access_token=%s", graphAPIBase, pageAccessToken),
		secret, pageAccessToken,
	)
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(body)) // #nosec G107
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return fmt.Errorf("facebook API returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// GetPageToken exchanges a User Access Token for a Page Access Token
// by calling GET /me/accounts and finding the page matching pageID.
// Results are cached for 30 minutes to avoid repeated API calls.
func GetPageToken(userToken, secret, pageID string) (string, error) {
	cacheKey := userToken[:min(len(userToken), 16)] + ":" + pageID

	// Check cache
	pageTokenCache.mu.RLock()
	if entry, ok := pageTokenCache.entries[cacheKey]; ok && time.Now().Before(entry.ExpiresAt) {
		pageTokenCache.mu.RUnlock()
		return entry.Token, nil
	}
	pageTokenCache.mu.RUnlock()

	// Fetch from Graph API
	url := appendProof(
		fmt.Sprintf("%s/me/accounts?access_token=%s", graphAPIBase, userToken),
		secret, userToken,
	)
	resp, err := httpClient.Get(url) // #nosec G107
	if err != nil {
		return "", fmt.Errorf("fetch pages: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("graph API returned %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
	}

	var result struct {
		Data []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse pages response: %w", err)
	}

	for _, page := range result.Data {
		if page.ID == pageID {
			// Cache the result
			pageTokenCache.mu.Lock()
			pageTokenCache.entries[cacheKey] = cachedPageToken{
				Token:     page.AccessToken,
				ExpiresAt: time.Now().Add(pageTokenCacheTTL),
			}
			pageTokenCache.mu.Unlock()
			return page.AccessToken, nil
		}
	}

	return "", fmt.Errorf("page %s not found in user's managed pages", pageID)
}

// SubscribePageToApp subscribes a Page to this App's webhook events.
// This must be called with a valid Page Access Token to receive events.
func SubscribePageToApp(pageAccessToken, secret, pageID string, fields []string) error {
	url := appendProof(
		fmt.Sprintf("%s/%s/subscribed_apps?access_token=%s", graphAPIBase, pageID, pageAccessToken),
		secret, pageAccessToken,
	)

	payload := map[string]interface{}{
		"subscribed_fields": fields,
	}
	body, _ := json.Marshal(payload)

	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(body)) // #nosec G107
	if err != nil {
		return fmt.Errorf("subscribe page: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return fmt.Errorf("page subscription failed (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}
