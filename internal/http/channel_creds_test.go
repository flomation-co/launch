package http

import (
	"sort"
	"testing"

	. "github.com/onsi/gomega"
)

func TestExtractVarRefs(t *testing.T) {
	RegisterTestingT(t)

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"no refs", "xoxb-static-token", nil},
		{"single ref", "${secrets.slack_bot_token}", []string{"secrets.slack_bot_token"}},
		{"multiple refs", "prefix-${a}-${b}-suffix", []string{"a", "b"}},
		{"unterminated ref ignored", "${incomplete", nil},
		{"empty string", "", nil},
		{"mixed env and secret", "${env.HOST}/api/${secrets.token}", []string{"env.HOST", "secrets.token"}},
	}

	for _, tc := range cases {
		got := extractVarRefs(tc.in)
		Expect(got).To(Equal(tc.want), "case: %s", tc.name)
	}
}

func TestSubstituteVars(t *testing.T) {
	RegisterTestingT(t)

	resolved := map[string]string{
		"secrets.slack_bot_token":      "xoxb-real-token",
		"secrets.slack_signing_secret": "real-signing-secret",
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single substitution", "${secrets.slack_bot_token}", "xoxb-real-token"},
		{"multiple substitutions", "${secrets.slack_bot_token}|${secrets.slack_signing_secret}", "xoxb-real-token|real-signing-secret"},
		{"no substitution needed", "plain-value", "plain-value"},
		{"unresolved key left alone", "${secrets.missing}", "${secrets.missing}"},
	}

	for _, tc := range cases {
		got := substituteVars(tc.in, resolved)
		Expect(got).To(Equal(tc.want), "case: %s", tc.name)
	}
}

func TestParseLegacyChannelConfig(t *testing.T) {
	RegisterTestingT(t)

	raw := []byte(`[
		{"type": "telegram", "config": {"bot_token": "tg-token-123"}},
		{"type": "slack", "config": {"bot_token": "xoxb-slack", "signing_secret": "sig-789", "irrelevant_obj": {"nested": "value"}}}
	]`)

	slack := parseLegacyChannelConfig(raw, "slack")
	Expect(slack["bot_token"]).To(Equal("xoxb-slack"))
	Expect(slack["signing_secret"]).To(Equal("sig-789"))
	_, hasNested := slack["irrelevant_obj"]
	Expect(hasNested).To(BeFalse(), "non-string config values must be excluded")

	tg := parseLegacyChannelConfig(raw, "telegram")
	Expect(tg["bot_token"]).To(Equal("tg-token-123"))

	missing := parseLegacyChannelConfig(raw, "teams")
	Expect(missing).To(BeEmpty())

	invalid := parseLegacyChannelConfig([]byte("not json"), "slack")
	Expect(invalid).To(BeEmpty())
}

func TestExtractVarRefsOrder(t *testing.T) {
	RegisterTestingT(t)

	got := extractVarRefs("${z}-${a}-${m}")
	sort.Strings(got)
	Expect(got).To(Equal([]string{"a", "m", "z"}))
}
