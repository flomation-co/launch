package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// VerifySignature validates the HMAC-SHA256 signature in the X-Hub-Signature-256
// header against the request body.
func VerifySignature(secret string, body []byte, r *http.Request) error {
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		return fmt.Errorf("missing X-Hub-Signature-256 header")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

// ParseEvent extracts structured event data from a GitHub webhook payload.
// eventHeader is the value of the X-GitHub-Event header.
func ParseEvent(eventHeader string, body []byte) (map[string]interface{}, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse webhook body: %w", err)
	}

	data := map[string]interface{}{
		"event_type":   eventHeader,
		"body":         string(body),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}

	// Action (common across many event types)
	if v, ok := raw["action"].(string); ok {
		data["action"] = v
	}

	// Sender
	if sender, ok := raw["sender"].(map[string]interface{}); ok {
		if v, ok := sender["login"].(string); ok {
			data["sender_login"] = v
		}
	}

	// Repository
	if repo, ok := raw["repository"].(map[string]interface{}); ok {
		if v, ok := repo["name"].(string); ok {
			data["repository_name"] = v
		}
		if v, ok := repo["full_name"].(string); ok {
			data["repository_full_name"] = v
		}
		if v, ok := repo["html_url"].(string); ok {
			data["repository_url"] = v
		}
	}

	// Ref (push events)
	if v, ok := raw["ref"].(string); ok {
		data["ref"] = v
	}

	// Pull request
	if pr, ok := raw["pull_request"].(map[string]interface{}); ok {
		if v, ok := pr["number"].(float64); ok {
			data["pull_request_number"] = fmt.Sprintf("%.0f", v)
		}
		if v, ok := pr["title"].(string); ok {
			data["pull_request_title"] = v
		}
		if v, ok := pr["state"].(string); ok {
			data["pull_request_state"] = v
		}
	}

	// Issue (for issue_comment events)
	if issue, ok := raw["issue"].(map[string]interface{}); ok {
		// If this issue has a pull_request key, it's a PR comment
		if pr, ok := issue["pull_request"].(map[string]interface{}); ok && pr != nil {
			if v, ok := issue["number"].(float64); ok {
				data["pull_request_number"] = fmt.Sprintf("%.0f", v)
			}
			if v, ok := issue["title"].(string); ok {
				data["pull_request_title"] = v
			}
		}
	}

	// Comment body
	if comment, ok := raw["comment"].(map[string]interface{}); ok {
		if v, ok := comment["body"].(string); ok {
			data["comment_body"] = v
		}
	}

	// Review body
	if review, ok := raw["review"].(map[string]interface{}); ok {
		if v, ok := review["body"].(string); ok && data["comment_body"] == nil {
			data["comment_body"] = v
		}
	}

	// Workflow run
	if run, ok := raw["workflow_run"].(map[string]interface{}); ok {
		if v, ok := run["id"].(float64); ok {
			data["workflow_run_id"] = fmt.Sprintf("%.0f", v)
		}
		if v, ok := run["status"].(string); ok {
			data["workflow_run_status"] = v
		}
	}

	return data, nil
}

// MatchesFilter checks whether the event type matches the comma-separated filter.
// An empty filter matches all events.
func MatchesFilter(eventType, filter string) bool {
	if filter == "" {
		return true
	}
	for _, f := range strings.Split(filter, ",") {
		if strings.TrimSpace(f) == eventType {
			return true
		}
	}
	return false
}
