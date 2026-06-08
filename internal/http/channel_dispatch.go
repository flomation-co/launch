package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	log "github.com/sirupsen/logrus"
)

// postTriggerDispatch POSTs a parsed channel message to the API's
// unified trigger dispatch endpoint
// (POST /api/v1/internal/trigger/:trigger_id/dispatch). The API decides
// whether to run the message through the agent inbound pipeline (when
// the trigger's flow is an agent's orchestrator) or fire the flow as a
// standalone trigger execution.
//
// Each channel handler is responsible for first looking up the URL
// :id as a trigger via s.trigger.GetTriggerByID(id) — when a trigger
// is found, the handler calls this; otherwise it falls back to its
// legacy agent-keyed path. This split keeps the lookup result
// available to both credential resolution and dispatch routing without
// hitting the DB twice.
func (s *Service) postTriggerDispatch(triggerID, channelType, sender, content string, metadata map[string]interface{}) {
	body := map[string]interface{}{
		"channel_type": channelType,
		"sender":       sender,
		"content":      content,
		"metadata":     metadata,
	}
	payload, _ := json.Marshal(body)

	apiURL := fmt.Sprintf("%s/api/v1/internal/trigger/%s/dispatch", s.config.InternalAPIURL(), triggerID)
	resp, err := s.apiClient.Post(apiURL, "application/json", bytes.NewReader(payload)) // #nosec G107 -- internal service-to-service call
	if err != nil {
		log.WithFields(log.Fields{
			"trigger_id": triggerID,
			"error":      err,
		}).Error("postTriggerDispatch: API request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		log.WithFields(log.Fields{
			"trigger_id":  triggerID,
			"status_code": resp.StatusCode,
			"response":    string(respBody),
		}).Error("postTriggerDispatch: API returned error")
	}
}

// resolveTriggerCreds returns the resolved credentials map for a
// specific trigger ID (analogue of resolveChannelCreds for trigger-
// keyed dispatch). Returns nil if the trigger isn't known or its data
// can't be parsed.
func (s *Service) resolveTriggerCreds(triggerID string) map[string]string {
	tr, err := s.trigger.GetTriggerByID(triggerID)
	if err != nil || tr == nil {
		return nil
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(tr.Data, &raw); err != nil {
		return nil
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
		if resolved, err := s.trigger.ResolveVariables(tr.ID, refs); err == nil {
			for k, v := range creds {
				creds[k] = substituteVars(v, resolved)
			}
		}
	}
	return creds
}
