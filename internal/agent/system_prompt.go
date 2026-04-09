package agent

// Phase 2b of the Agent Memory feature. See plans/agent_memory.md
// §"Server-side system prompt assembly".
//
// Until Phase 2b, `ExecutionContext.SystemPrompt` was just the agent's
// configured persona — a single string the flow author wrote in the
// agent settings. Phase 2b replaces that with a server-assembled view
// built on every dispatch from:
//
//   1. The agent's configured persona (unchanged input).
//   2. A Layer 0 honesty directive (relaxed automatically in Phase 3).
//   3. A "what you know about this user" block of pinned memories.
//   4. A channel directive block (formatting hints per channel type).
//   5. Any open pending-action confirmation instructions.
//
// The assembled result replaces the raw persona as the value of the
// reserved `system_prompt` trigger data key, which means the flow
// author's AI action sees the full context via `${flow.system_prompt}`
// without having to wire any of this up themselves. Zero flow-author
// burden was the whole point of building this server-side.
//
// Design rules:
//   - Every HTTP fetch has a short timeout and fails open — a failed
//     memory lookup must never block the reply path. The user waiting
//     on the agent gets a slightly less context-aware response rather
//     than a dropped message.
//   - The pure assembly logic is isolated in buildSystemPrompt so it
//     can be exhaustively unit-tested without touching HTTP or the DB.
//   - Section ordering matches the plan doc: persona → honesty directive
//     → memories → channel → pending confirmation. Pending confirmations
//     come last so they're the freshest instruction in the model's
//     working memory when it composes a reply.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flomation.app/automate/launch"
	log "github.com/sirupsen/logrus"
)

// assemblyHTTPTimeout is deliberately shorter than apiTimeout. Memory
// and pending-action fetches are on the hot reply path and must never
// block for more than a short window. Three seconds is generous enough
// for a healthy round-trip and strict enough that a slow or hung API
// can't take down reply latency.
const assemblyHTTPTimeout = 3 * apiTimeoutQuarter

// apiTimeoutQuarter is apiTimeout / 4. Expressed this way so that any
// future change to apiTimeout propagates proportionally without needing
// two constants in lockstep.
const apiTimeoutQuarter = apiTimeout / 4

// layerZeroHonestyDirective is the Phase 3 version of the honesty rule.
// The platform can now honour time-bounded commitments via the commitment
// poller, so the model is allowed to make them.
const layerZeroHonestyDirective = "" +
	"You may make time-bounded commitments to the user. Examples: " +
	"'I'll get back to you in 30 minutes', 'remind me tomorrow at 9am', " +
	"'I'll check on that in an hour', 'I'll remind you in 1 minute'. " +
	"The platform will honour these automatically — you do not need to " +
	"remember them yourself. Any duration is valid, including 1 minute " +
	"or even 30 seconds. Never refuse or suggest a longer duration than " +
	"the user requested. Always confirm the exact timeframe the user asked for. " +
	"Do NOT make open-ended commitments without a specific time or condition " +
	"(e.g. avoid 'I'll look into it' with no timeframe)."

// assembleSystemPrompt builds the final system prompt string that gets
// passed to the agent's orchestrator flow via `system_prompt` trigger
// data. Returns the original persona (or empty string) if fetches fail
// or if there's no agent_user_id to scope memories against.
func (s *Service) assembleSystemPrompt(
	reg *launch.AgentRegistration,
	msg InboundMessage,
	agentUserID *string,
) string {
	persona := ""
	if reg.SystemPrompt != nil {
		persona = *reg.SystemPrompt
	}

	// Without an agent_user_id we cannot scope memories or pending
	// actions to anyone, so the assembled view is just persona + honesty
	// directive + channel directive. This is the graceful-degradation
	// path that covers unresolved webhooks and first-contact messages
	// where identity resolution failed.
	if agentUserID == nil || *agentUserID == "" {
		return buildSystemPrompt(persona, nil, nil, msg.ChannelType)
	}

	memories := s.fetchPinnedMemories(reg, *agentUserID)
	pending := s.fetchOpenPendingActions(reg, *agentUserID)

	return buildSystemPrompt(persona, memories, pending, msg.ChannelType)
}

// buildSystemPrompt is the pure-function core of the assembler. Given
// already-fetched data it composes the final string deterministically,
// with no I/O. This is the function that unit tests should target —
// every edge case in section inclusion and ordering lives here.
//
// Rules:
//   - Empty persona still produces a valid prompt (just starts with the
//     honesty directive).
//   - Empty memories / nil memories → the "What you know about this
//     user" section is omitted entirely rather than rendered as an empty
//     bullet list.
//   - Unknown channel types fall through to no channel-directive section
//     (rather than a generic "respond however you like" directive which
//     would just be noise).
//   - Sections are separated by the ━ divider pattern from the plan
//     document so the model sees a visually unambiguous boundary.
func buildSystemPrompt(
	persona string,
	pinnedMemories []assembledMemory,
	pendingActions []assembledPendingAction,
	channelType string,
) string {
	var b strings.Builder

	if persona != "" {
		b.WriteString(persona)
		b.WriteString("\n\n")
	}

	// Current date/time so the model can reason about scheduling,
	// deadlines, and time-relative references ("tomorrow", "next week").
	b.WriteString("━━━ Current time ━━━\n")
	b.WriteString(time.Now().Format("Monday, 2 January 2006 15:04 MST"))
	b.WriteString("\n\n")

	// Layer 0 honesty directive. Always included, regardless of whether
	// there are memories or pending actions — it's a baseline rule about
	// what the model can and cannot do, not a fact about the user.
	b.WriteString("━━━ Layer 0 ━━━\n")
	b.WriteString(layerZeroHonestyDirective)
	b.WriteString("\n\n")

	if len(pinnedMemories) > 0 {
		b.WriteString("━━━ What you know about this user ━━━\n")
		for _, mem := range pinnedMemories {
			// Title is the retrieval handle, body is the fact. Render as
			// "• <title>: <body>" for readability, falling back to just
			// the body when the title is empty or is a trivial duplicate
			// of the body.
			if mem.Title != "" && mem.Title != mem.Body {
				b.WriteString("• ")
				b.WriteString(mem.Title)
				b.WriteString(": ")
				b.WriteString(mem.Body)
			} else {
				b.WriteString("• ")
				b.WriteString(mem.Body)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if directive := channelDirective(channelType); directive != "" {
		b.WriteString("━━━ Current channel ━━━\n")
		b.WriteString(directive)
		b.WriteString("\n\n")
	}

	if len(pendingActions) > 0 {
		b.WriteString("━━━ Pending confirmation ━━━\n")
		for _, pa := range pendingActions {
			// The extraction pipeline wrote the verbatim user utterance
			// into Evidence, so surfacing it back gives the model a
			// concrete anchor for the confirmation wording.
			fmt.Fprintf(&b, "A %s was inferred from: %q. Naturally confirm this with the user in your reply.\n",
				pa.Type, pa.Evidence)
		}
		b.WriteString("\n")
	}

	// Trim trailing whitespace so the final string isn't padded with
	// blank lines the model might interpret as meaningful.
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// channelDirective returns the formatting hint block for a given channel
// type, or empty string for channels without a specific guideline.
//
// These are deliberately terse — every token in the system prompt costs
// latency and money, and the model already knows how to format for
// common channels. The directive only needs to disambiguate when the
// agent could reasonably output the wrong format (e.g. standard Markdown
// bold `**text**` into Slack, which renders as `**text**` literally).
func channelDirective(channelType string) string {
	switch channelType {
	case "slack":
		return "Responding via Slack — use mrkdwn formatting (*bold*, _italic_, `code`, triple-backtick code blocks). Do NOT use standard Markdown **bold** — it renders literally in Slack."
	case "telegram":
		return "Responding via Telegram — standard Markdown (**bold**, _italic_, `code`) is supported. Keep replies under 4096 characters."
	case "email":
		return "Responding via email — use plain text. Keep formatting minimal and professional."
	case "webhook":
		return "Responding via webhook — the caller may be a machine. Respond concisely; use JSON only if the caller explicitly requests structured data."
	default:
		return ""
	}
}

// --- Fetchers ---

// assembledMemory is the subset of api.AgentMemory the assembler needs.
// Defined here rather than importing the api types package to keep the
// Launch → API coupling to the JSON wire format only.
type assembledMemory struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// assembledPendingAction is the subset of api.AgentPendingAction the
// assembler needs. Same rationale as assembledMemory.
type assembledPendingAction struct {
	Type     string `json:"type"`
	Evidence string `json:"evidence"`
}

// fetchPinnedMemories calls the API's internal list-memories endpoint
// scoped to pinned=true. Returns nil on any error — the assembler
// treats nil as "no memories" and falls back to persona + directive.
// Never blocks the reply path.
func (s *Service) fetchPinnedMemories(reg *launch.AgentRegistration, agentUserID string) []assembledMemory {
	if reg.APIURL == "" || reg.AgentID == "" || agentUserID == "" {
		return nil
	}

	q := url.Values{}
	q.Set("agent_user_id", agentUserID)
	q.Set("pinned", "true")
	// Hard upper bound on memory block size. Pinned rows for a well-
	// tended profile will be in the low tens; 50 is a generous ceiling
	// that still caps the prompt budget.
	q.Set("limit", "50")

	endpoint := fmt.Sprintf(
		"%s/api/v1/internal/agent/%s/memory?%s",
		reg.APIURL, reg.AgentID, q.Encode(),
	)

	client := http.Client{Timeout: assemblyHTTPTimeout}
	resp, err := client.Get(endpoint)
	if err != nil {
		log.WithFields(log.Fields{
			"agent_id":      reg.AgentID,
			"agent_user_id": agentUserID,
			"error":         err,
		}).Warn("failed to fetch pinned memories for system prompt assembly")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result []assembledMemory
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	return result
}

// fetchOpenPendingActions calls the API's internal list-pending-actions
// endpoint. Same failure semantics as fetchPinnedMemories.
func (s *Service) fetchOpenPendingActions(reg *launch.AgentRegistration, agentUserID string) []assembledPendingAction {
	if reg.APIURL == "" || reg.AgentID == "" || agentUserID == "" {
		return nil
	}

	q := url.Values{}
	q.Set("agent_user_id", agentUserID)

	endpoint := fmt.Sprintf(
		"%s/api/v1/internal/agent/%s/pending-action?%s",
		reg.APIURL, reg.AgentID, q.Encode(),
	)

	client := http.Client{Timeout: assemblyHTTPTimeout}
	resp, err := client.Get(endpoint)
	if err != nil {
		log.WithFields(log.Fields{
			"agent_id":      reg.AgentID,
			"agent_user_id": agentUserID,
			"error":         err,
		}).Warn("failed to fetch open pending actions for system prompt assembly")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result []assembledPendingAction
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	return result
}
