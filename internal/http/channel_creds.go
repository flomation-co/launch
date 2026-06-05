package http

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"flomation.app/automate/launch"
	log "github.com/sirupsen/logrus"
)

// resolveChannelCreds returns the resolved credentials for an agent's channel
// trigger. It locates the trigger of the requested type on the agent's
// orchestrator flow, reads its Data (a JSON map of input → value, possibly
// containing ${secrets.X} / ${env.X} references), and asks the API to resolve
// any variable references via the trigger-scoped resolve endpoint.
//
// Returns (nil, false) when no trigger of the given type is configured for
// the agent — callers should fall back to the legacy agent.channels store.
// Results are cached for credCacheTTL to avoid hitting the API on every
// webhook (Slack expects a sub-3s ack).
func (s *Service) resolveChannelCreds(agentID, channelType string) (map[string]string, bool) {
	cacheKey := agentID + "|" + channelType
	if entry, ok := credCache.Load(cacheKey); ok {
		e := entry.(*credCacheEntry)
		if time.Now().Before(e.expiry) {
			return e.creds, true
		}
		credCache.Delete(cacheKey)
	}

	reg, err := s.agent.GetRegistration(agentID)
	if err != nil || reg == nil || reg.OrchestratorFlowID == nil || *reg.OrchestratorFlowID == "" {
		return nil, false
	}

	triggers, err := s.trigger.GetTriggersByFlowID(*reg.OrchestratorFlowID)
	if err != nil {
		log.WithFields(log.Fields{
			"error":   err,
			"flow_id": *reg.OrchestratorFlowID,
		}).Warn("resolveChannelCreds: unable to list triggers")
		return nil, false
	}

	var matched *launch.Trigger
	for _, t := range triggers {
		if t == nil || t.Type != channelType || t.DisabledAt != nil {
			continue
		}
		matched = t
		break
	}
	if matched == nil {
		return nil, false
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(matched.Data, &raw); err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": matched.ID,
		}).Warn("resolveChannelCreds: unable to parse trigger data")
		return nil, false
	}

	creds := map[string]string{}
	var refs []string
	for k, v := range raw {
		strVal, ok := v.(string)
		if !ok {
			continue
		}
		creds[k] = strVal
		refs = append(refs, extractVarRefs(strVal)...)
	}

	if len(refs) > 0 {
		resolved, err := s.trigger.ResolveVariables(matched.ID, refs)
		if err != nil {
			log.WithFields(log.Fields{
				"error":      err,
				"trigger_id": matched.ID,
			}).Warn("resolveChannelCreds: unable to resolve variables")
		}
		for k, v := range creds {
			creds[k] = substituteVars(v, resolved)
		}
	}

	credCache.Store(cacheKey, &credCacheEntry{
		creds:  creds,
		expiry: time.Now().Add(credCacheTTL),
	})
	return creds, true
}

// InvalidateChannelCredsCache clears any cached credentials for an agent.
// Called when the agent's orchestrator flow is updated so the next webhook
// picks up the new configuration immediately.
func (s *Service) InvalidateChannelCredsCache(agentID string) {
	credCache.Range(func(k, _ interface{}) bool {
		if key, ok := k.(string); ok && strings.HasPrefix(key, agentID+"|") {
			credCache.Delete(k)
		}
		return true
	})
}

type credCacheEntry struct {
	creds  map[string]string
	expiry time.Time
}

var (
	credCache    sync.Map
	credCacheTTL = 60 * time.Second
)

// extractVarRefs returns the names of all ${...} references in a string.
func extractVarRefs(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			return out
		}
		s = s[i+2:]
		j := strings.Index(s, "}")
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+1:]
	}
}

// substituteVars replaces ${name} occurrences using the supplied resolution map.
func substituteVars(s string, resolved map[string]string) string {
	if !strings.Contains(s, "${") || len(resolved) == 0 {
		return s
	}
	for k, v := range resolved {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	return s
}
