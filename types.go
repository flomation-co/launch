package launch

import "time"

const (
	TriggerTypeManual    = "manual"
	TriggerTypeScheduled = "schedule"
	TriggerTypeQR        = "qr"
	TriggerTypeImage     = "image"
	TriggerTypeEmail     = "email"
	TriggerTypeTelegram  = "telegram"
	TriggerTypeForm      = "form"
	TriggerTypeWebhook   = "webhook"
	TriggerTypeGitPoll   = "git-poll"
)

type Trigger struct {
	ID         string     `json:"id" db:"id"`
	Type       string     `json:"type" db:"type"`
	Data       []byte     `json:"data" db:"data"`
	FlowID     string     `json:"flow_id" db:"flow_id"`
	CreatedAt  time.Time  `json:"created_at" db:"created_at"`
	DisabledAt *time.Time `json:"disabled_at" db:"disabled_at"`
}
