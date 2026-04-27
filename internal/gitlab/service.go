package gitlab

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// VerifyToken validates the X-Gitlab-Token header against the stored secret
// using constant-time comparison to prevent timing attacks.
func VerifyToken(secret string, r *http.Request) error {
	token := r.Header.Get("X-Gitlab-Token")
	if token == "" {
		return fmt.Errorf("missing X-Gitlab-Token header")
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
		return fmt.Errorf("invalid webhook token")
	}
	return nil
}

// ParseEvent extracts structured event data from a GitLab webhook payload.
// eventHeader is the value of the X-Gitlab-Event header.
func ParseEvent(eventHeader string, body []byte) (map[string]interface{}, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse webhook body: %w", err)
	}

	data := map[string]interface{}{
		"event_type":   normaliseEventType(eventHeader),
		"body":         string(body),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}

	// object_kind
	if v, ok := raw["object_kind"].(string); ok {
		data["object_kind"] = v
	}

	// ref
	if v, ok := raw["ref"].(string); ok {
		data["ref"] = v
	}

	// Project info
	if project, ok := raw["project"].(map[string]interface{}); ok {
		if v, ok := project["id"].(float64); ok {
			data["project_id"] = fmt.Sprintf("%.0f", v)
		}
		if v, ok := project["name"].(string); ok {
			data["project_name"] = v
		}
		if v, ok := project["web_url"].(string); ok {
			data["project_url"] = v
		}
	}

	// User info
	if v, ok := raw["user_name"].(string); ok {
		data["user_name"] = v
	}
	if v, ok := raw["user_username"].(string); ok {
		data["user_username"] = v
	}
	// Also check nested user object
	if user, ok := raw["user"].(map[string]interface{}); ok {
		if _, exists := data["user_name"]; !exists {
			if v, ok := user["name"].(string); ok {
				data["user_name"] = v
			}
		}
		if _, exists := data["user_username"]; !exists {
			if v, ok := user["username"].(string); ok {
				data["user_username"] = v
			}
		}
	}

	// Merge request attributes
	if attrs, ok := raw["object_attributes"].(map[string]interface{}); ok {
		objectKind, _ := raw["object_kind"].(string)

		switch objectKind {
		case "merge_request":
			if v, ok := attrs["iid"].(float64); ok {
				data["merge_request_iid"] = fmt.Sprintf("%.0f", v)
			}
			if v, ok := attrs["title"].(string); ok {
				data["merge_request_title"] = v
			}
			if v, ok := attrs["state"].(string); ok {
				data["merge_request_state"] = v
			}
			if v, ok := attrs["action"].(string); ok {
				data["merge_request_action"] = v
			}

		case "note":
			if v, ok := attrs["note"].(string); ok {
				data["comment_body"] = v
			}
			// For notes on MRs, also extract MR data
			if mr, ok := raw["merge_request"].(map[string]interface{}); ok {
				if v, ok := mr["iid"].(float64); ok {
					data["merge_request_iid"] = fmt.Sprintf("%.0f", v)
				}
				if v, ok := mr["title"].(string); ok {
					data["merge_request_title"] = v
				}
			}

		case "pipeline":
			if v, ok := attrs["id"].(float64); ok {
				data["pipeline_id"] = fmt.Sprintf("%.0f", v)
			}
			if v, ok := attrs["status"].(string); ok {
				data["pipeline_status"] = v
			}
			if v, ok := attrs["ref"].(string); ok {
				data["ref"] = v
			}
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

// normaliseEventType converts the X-Gitlab-Event header to a simple event name.
// e.g. "Merge Request Hook" → "merge_request", "Push Hook" → "push"
func normaliseEventType(header string) string {
	header = strings.TrimSuffix(header, " Hook")
	header = strings.TrimSuffix(header, " Event")
	header = strings.ToLower(header)
	header = strings.ReplaceAll(header, " ", "_")
	return header
}
