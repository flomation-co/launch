package launch

import (
	"encoding/json"
	"time"
)

const (
	TriggerTypeManual              = "manual"
	TriggerTypeScheduled           = "schedule"
	TriggerTypeQR                  = "qr"
	TriggerTypeImage               = "image"
	TriggerTypeEmail               = "email"
	TriggerTypeTelegram            = "telegram"
	TriggerTypeForm                = "form"
	TriggerTypeWebhook             = "webhook"
	TriggerTypeGitPoll             = "git-poll"
	TriggerTypeS3                  = "s3"
	TriggerTypeGitLabWebhook       = "gitlab-webhook"
	TriggerTypeGitHubWebhook       = "github-webhook"
	TriggerTypeFacebookMessenger   = "facebook-messenger"
	TriggerTypeFacebookFeed        = "facebook-feed"
	TriggerTypeLinkedInPoll        = "linkedin-poll"
	TriggerTypeGoogleDrive         = "google-drive"
	TriggerTypeMicrosoftOutlook    = "microsoft-outlook"
	TriggerTypeTeams               = "teams"
	TriggerTypeMailchimpWebhook    = "mailchimp-webhook"    // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeShopifyWebhook      = "shopify-webhook"      // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeCalendlyWebhook     = "calendly-webhook"     // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeZendeskWebhook      = "zendesk-webhook"      // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeCalcomWebhook       = "calcom-webhook"       // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeAcuityWebhook       = "acuity-webhook"       // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeStripeWebhook       = "stripe-webhook"       // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeWooCommerceWebhook  = "woocommerce-webhook"  // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeJiraWebhook         = "jira-webhook"         // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeTrelloWebhook       = "trello-webhook"       // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeAsanaWebhook        = "asana-webhook"        // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeMondayWebhook       = "monday-webhook"       // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeIntercomWebhook     = "intercom-webhook"     // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeSendGridWebhook     = "sendgrid-webhook"     // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeTypeformWebhook     = "typeform-webhook"     // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeJotformWebhook      = "jotform-webhook"      // #nosec G101 — trigger type identifier, not a credential
	TriggerTypeSurveyMonkeyWebhook = "surveymonkey-webhook" // #nosec G101 — trigger type identifier, not a credential
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
// FacebookAuthState records a pending Login-with-Facebook consent flow.
// Identity-only at the start; future Facebook OAuth purposes can extend.
type FacebookAuthState struct {
	State          string
	Purpose        string // "identity" — only purpose currently in use
	UserID         string
	OrganisationID string // empty for personal mode
	ChannelType    string // "facebook_messenger"
}

// LinkedInAuthState records a pending Sign-in-with-LinkedIn consent flow.
// Identity-only at the start; future LinkedIn OAuth purposes can extend.
type LinkedInAuthState struct {
	State          string
	Purpose        string // "identity" — only purpose currently in use
	UserID         string
	OrganisationID string // empty for personal mode
	ChannelType    string // "linkedin"
}

// SlackAuthState records a pending Slack "Sign in with Slack" (OIDC)
// consent flow. Identity-only at the start; future Slack OAuth purposes
// (e.g. workspace install via bot tokens for personal use) can extend.
type SlackAuthState struct {
	State          string
	Purpose        string // "identity" — only purpose currently in use
	UserID         string
	OrganisationID string // empty for personal mode
	ChannelType    string // "slack"
}

// MicrosoftAuthState records a pending Microsoft OAuth consent flow.
// Currently only used for the R3 Phase 2 identity flow; agent / trigger
// Microsoft flows can extend this struct (and the table) when added.
type MicrosoftAuthState struct {
	State          string
	Purpose        string // "identity" — only purpose currently in use
	UserID         string
	OrganisationID string // empty for personal mode
	ChannelType    string // "microsoft" — the user_identity row's channel_type
}

type GoogleAuthState struct {
	State       string
	AgentID     string
	AgentUserID string
	TriggerID   string // non-empty for trigger-scoped connections
	Purpose     string // "calendar", "email_read", "email_send", "identity"
	// Set only for the identity-OAuth flow (R3 Phase 2). When UserID is
	// non-empty the callback routes the resolved external_id to the API's
	// user_identity endpoint rather than the existing agent_user / trigger
	// endpoints. OrganisationID is empty for personal-mode declarations.
	UserID         string
	OrganisationID string
	ChannelType    string
}
