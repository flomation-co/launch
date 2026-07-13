package slack

import (
	"regexp"

	"github.com/slack-go/slack"
)

// mentionRe matches Slack mention tokens: <@U123ABC> or <@U123ABC|name>.
// Slack encodes "@Dan Marsh" as the mentioned user's ID inside the token,
// not as the literal display text, so a mention is detected by ID.
var mentionRe = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|[^>]*)?>`)

// ParseMentions extracts the set of mentioned Slack user IDs from message
// text. The result is a non-nil set (empty when there are no mentions),
// so callers can use len() and direct lookups without nil checks.
func ParseMentions(text string) map[string]bool {
	out := make(map[string]bool)
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		out[m[1]] = true
	}
	return out
}

// BotUserID resolves the bot user ID for a bot token via Slack's auth.test.
// This is the ID that appears inside <@...> tokens when the agent owning
// the token is @-mentioned in a channel.
func BotUserID(botToken string) (string, error) {
	resp, err := slack.New(botToken).AuthTest()
	if err != nil {
		return "", err
	}
	return resp.UserID, nil
}
