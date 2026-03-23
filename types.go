package launch

import (
	"encoding/json"
	"time"
)

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
	TriggerTypeS3        = "s3"
)

type Trigger struct {
	ID         string     `json:"id" db:"id"`
	Type       string     `json:"type" db:"type"`
	Data       json.RawMessage `json:"data" db:"data"`
	FlowID     string     `json:"flow_id" db:"flow_id"`
	CreatedAt  time.Time  `json:"created_at" db:"created_at"`
	DisabledAt *time.Time `json:"disabled_at" db:"disabled_at"`
}
