package launch

import (
	"encoding/json"
	"time"
)

const (
	TriggerTypeManual            = "manual"
	TriggerTypeScheduled         = "schedule"
	TriggerTypeQR                = "qr"
	TriggerTypeImage             = "image"
	TriggerTypeEmail             = "email"
	TriggerTypeTelegram          = "telegram"
	TriggerTypeForm              = "form"
	TriggerTypeWebhook           = "webhook"
	TriggerTypeGitPoll           = "git-poll"
	TriggerTypeS3                = "s3"
	TriggerTypeGitLabWebhook     = "gitlab-webhook"
	TriggerTypeGitHubWebhook     = "github-webhook"
	TriggerTypeFacebookMessenger = "facebook-messenger"
	TriggerTypeFacebookFeed      = "facebook-feed"
	TriggerTypeLinkedInPoll      = "linkedin-poll"
)

type Trigger struct {
	ID         string          `json:"id" db:"id"`
	Type       string          `json:"type" db:"type"`
	Data       json.RawMessage `json:"data" db:"data"`
	FlowID     string          `json:"flow_id" db:"flow_id"`
	CreatedAt  time.Time       `json:"created_at" db:"created_at"`
	DisabledAt *time.Time      `json:"disabled_at" db:"disabled_at"`
}

// AgentRegistration represents an agent registered with the Launch service
// for runtime management (heartbeat, webhook handling, polling).
type AgentRegistration struct {
	AgentID                  string          `json:"agent_id" db:"agent_id"`
	OrchestratorFlowID       *string         `json:"orchestrator_flow_id" db:"orchestrator_flow_id"`
	TriggerID                *string         `json:"trigger_id" db:"trigger_id"`
	Channels                 json.RawMessage `json:"channels" db:"channels"`
	EnvironmentID            *string         `json:"environment_id" db:"environment_id"`
	MaxExecutionsPerHour     int             `json:"max_executions_per_hour" db:"max_executions_per_hour"`
	RequiresApproval         bool            `json:"requires_approval" db:"requires_approval"`
	SystemPrompt             *string         `json:"system_prompt,omitempty" db:"system_prompt"`
	APIURL                   string          `json:"api_url" db:"api_url"`
	ConversationHistoryLimit int             `json:"conversation_history_limit" db:"conversation_history_limit"`
	RegisteredAt             time.Time       `json:"registered_at" db:"registered_at"`
	DisabledAt               *time.Time      `json:"disabled_at" db:"disabled_at"`
}

// GoogleAuthState represents a pending OAuth flow linking a browser
// consent redirect back to the agent_user who initiated it.
type GoogleAuthState struct {
	State       string
	AgentID     string
	AgentUserID string
	TriggerID   string // non-empty for trigger-scoped connections
	Purpose     string // "calendar", "email_read", "email_send"
}
