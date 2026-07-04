package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"

	slackpkg "flomation.app/automate/launch/internal/slack"
	telegrampkg "flomation.app/automate/launch/internal/telegram"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// hitlActionPrefix tags Slack button action_ids and Telegram callback_data for
// Human-in-the-Loop responses. Kept in sync with the executor's await renderer.
const hitlActionPrefix = "hitl:"

type hitlRespondRequest struct {
	Token       string `json:"token,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	OptionValue string `json:"option_value,omitempty"`
	AnsweredBy  string `json:"answered_by,omitempty"`
	Channel     string `json:"channel,omitempty"`
}

type hitlRespondResult struct {
	Status      string `json:"status"` // answered | already_answered | not_found
	ExecutionID string `json:"execution_id,omitempty"`
	Channels    []struct {
		ChannelType string `json:"channel_type"`
		NodeID      string `json:"node_id"`
		ChannelID   string `json:"channel_id,omitempty"`
		MessageRef  string `json:"message_ref,omitempty"`
	} `json:"channels,omitempty"`
}

// respondHITL posts a human's response to the API, which enforces
// first-response-wins and resumes the suspended execution on the winning call.
func (s *Service) respondHITL(ctx context.Context, body hitlRespondRequest) (*hitlRespondResult, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%v/api/v1/internal/hitl/respond", s.config.InternalAPIURL())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return &hitlRespondResult{Status: "not_found"}, nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("hitl respond returned %d: %s", resp.StatusCode, string(b))
	}

	var out hitlRespondResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// parseHITLActionID splits a Slack action_id of the form
// "hitl:<request_id>:<option>" into its parts.
func parseHITLActionID(actionID string) (requestID, option string, ok bool) {
	if !strings.HasPrefix(actionID, hitlActionPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(actionID, hitlActionPrefix)
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// firstHITLAction returns the request/option for the first Slack action that is
// a HITL response, if any.
func firstHITLAction(actions []slackpkg.InteractionAction) (requestID, option string, ok bool) {
	for _, a := range actions {
		if rid, opt, matched := parseHITLActionID(a.ActionID); matched {
			return rid, opt, true
		}
	}
	return "", "", false
}

// handleHITLSlackInteraction resolves a Slack Block Kit button press as a HITL
// response and updates the original message to reflect the outcome. Runs in a
// goroutine after the 3-second ack.
func (s *Service) handleHITLSlackInteraction(interaction *slackpkg.InteractionPayload, requestID, option, answeredBy string) {
	res, err := s.respondHITL(context.Background(), hitlRespondRequest{
		RequestID:   requestID,
		OptionValue: option,
		AnsweredBy:  answeredBy,
		Channel:     "slack",
	})
	if err != nil {
		log.WithError(err).Error("hitl: failed to post slack response")
		return
	}

	if interaction.ResponseURL == "" {
		return
	}
	var text string
	switch res.Status {
	case "answered":
		text = fmt.Sprintf(":white_check_mark: *%s* — chosen by %s", option, answeredBy)
	case "already_answered":
		text = ":information_source: This request has already been answered."
	default:
		text = ":warning: This request is no longer available."
	}
	if err := slackpkg.RespondToInteraction(interaction.ResponseURL, text, true); err != nil {
		log.WithError(err).Warn("hitl: failed to update slack message")
	}
}

// handleHITLTelegramCallback resolves a Telegram inline-keyboard button press
// (callback_data = "hitl:<token>") as a HITL response and disables the buttons.
func (s *Service) handleHITLTelegramCallback(cb *telegrampkg.ParsedCallback, botToken string) {
	token := strings.TrimPrefix(cb.Data, hitlActionPrefix)
	res, err := s.respondHITL(context.Background(), hitlRespondRequest{
		Token:      token,
		AnsweredBy: cb.FromName,
		Channel:    "telegram",
	})
	if err != nil {
		log.WithError(err).Error("hitl: failed to post telegram response")
		return
	}

	if botToken == "" {
		return
	}
	// Acknowledge the button and clear the keyboard so it can't be pressed again.
	var ack string
	switch res.Status {
	case "answered":
		ack = "Response recorded"
	case "already_answered":
		ack = "Already answered"
	default:
		ack = "No longer available"
	}
	_ = telegrampkg.AnswerCallbackQuery(botToken, cb.CallbackID, ack)
	if cb.ChatID != 0 && cb.MessageID != 0 {
		_ = telegrampkg.EditMessageReplyMarkup(botToken, cb.ChatID, cb.MessageID, "")
	}
}

// handleHITLWebConfirm serves GET /respond/:token — a lightweight confirmation
// page. A GET must not commit the answer (email clients prefetch links), so the
// actual response happens on the POST below.
func (s *Service) handleHITLWebConfirm(c *gin.Context) {
	token := c.Param("token")
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, hitlConfirmPage(token))
}

// handleHITLWebRespond serves POST /respond/:token — commits the response.
func (s *Service) handleHITLWebRespond(c *gin.Context) {
	token := c.Param("token")
	if token == "" {
		c.String(http.StatusBadRequest, "missing token")
		return
	}
	res, err := s.respondHITL(c.Request.Context(), hitlRespondRequest{
		Token:      token,
		AnsweredBy: "web",
		Channel:    "web",
	})
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		log.WithError(err).Error("hitl: web respond failed")
		c.String(http.StatusBadGateway, hitlResultPage("Something went wrong recording your response. Please try again."))
		return
	}
	switch res.Status {
	case "answered":
		c.String(http.StatusOK, hitlResultPage("Thank you — your response has been recorded."))
	case "already_answered":
		c.String(http.StatusOK, hitlResultPage("This request has already been answered."))
	default:
		c.String(http.StatusOK, hitlResultPage("This request is no longer available."))
	}
}

func hitlConfirmPage(token string) string {
	t := html.EscapeString(token)
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>Confirm your response</title><style>` + hitlPageCSS + `</style></head><body><div class="card">` +
		`<h1>Confirm your response</h1><p>Click the button below to record your response.</p>` +
		`<form method="POST" action="/respond/` + t + `"><button type="submit">Confirm</button></form>` +
		`</div></body></html>`
}

func hitlResultPage(message string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>Response recorded</title><style>` + hitlPageCSS + `</style></head><body><div class="card">` +
		`<h1>Flomation</h1><p>` + html.EscapeString(message) + `</p></div></body></html>`
}

const hitlPageCSS = `body{font-family:Inter,system-ui,sans-serif;background:#0b0b0f;color:#f4f4f5;display:flex;` +
	`min-height:100vh;align-items:center;justify-content:center;margin:0}` +
	`.card{background:#17171d;border:1px solid #2a2a33;border-radius:16px;padding:40px;max-width:420px;text-align:center;` +
	`box-shadow:0 20px 60px rgba(70,0,112,.35)}` +
	`h1{background:linear-gradient(90deg,#460070,#00aa9c);-webkit-background-clip:text;background-clip:text;` +
	`-webkit-text-fill-color:transparent;font-size:28px;margin:0 0 12px}` +
	`p{color:#c4c4cc;line-height:1.5}` +
	`button{margin-top:16px;background:linear-gradient(90deg,#460070,#00aa9c);color:#fff;border:0;border-radius:10px;` +
	`padding:14px 32px;font-size:16px;font-weight:600;cursor:pointer}`
