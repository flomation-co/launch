package http

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/manualtrigger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// manualRunMaxBody bounds the JSON input blob a caller may POST to a
// manual-trigger run. 256 KiB is generous for a set of scalar inputs
// and keeps a hostile client from exhausting memory.
const manualRunMaxBody = 256 * 1024

// manualTriggerDispatcher is the slice of the trigger service the manual
// run handler needs. Declaring it as an interface keeps the handler's
// core logic unit-testable with a stub, without a live API or database.
type manualTriggerDispatcher interface {
	GetTriggerByID(id string) (*launch.Trigger, error)
	ResolveVariables(triggerID string, variables []string) (map[string]string, error)
	Trigger(trigger *launch.Trigger, data interface{}) error
}

// manualTriggerData is the shape of a manual trigger's registered Data
// blob (the contract with the API): the declared input schema plus an
// optional run token guarding programmatic invocation.
type manualTriggerData struct {
	TriggerInputs []manualtrigger.TriggerInput `json:"trigger_inputs"`
	RunToken      string                       `json:"run_token"`
}

// handleManualRun is the public, programmatic entry point for a manual
// trigger. It validates the caller-supplied JSON input blob against the
// trigger's declared schema and, if the trigger declares a run token,
// authenticates the request before dispatching the flow.
//
//	POST /trigger/:id/run
//	Authorization: Bearer <run_token>   (only when a run token is configured)
//	Body: {"field": value, ...}
func (s *Service) handleManualRun(c *gin.Context) {
	s.runManualTrigger(c, s.trigger)
}

// runManualTrigger holds the testable body of handleManualRun, taking
// the dispatcher as an argument so tests can supply a stub.
func (s *Service) runManualTrigger(c *gin.Context, disp manualTriggerDispatcher) {
	id := c.Param("id")
	if id == "" || uuid.Validate(id) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := disp.GetTriggerByID(id)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "id": id}).Error("manual run: trigger lookup failed")
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	// A missing trigger, or one that is not a manual trigger, is reported
	// as 404 so the endpoint never confirms the existence of non-manual
	// triggers to an unauthenticated caller.
	if tr == nil || tr.Type != launch.TriggerTypeManual {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	var cfg manualTriggerData
	if len(tr.Data) > 0 {
		if err := json.Unmarshal(tr.Data, &cfg); err != nil {
			log.WithFields(log.Fields{"error": err, "id": id}).Error("manual run: could not parse trigger data")
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
	}

	// Authentication. When a run token is configured it must be presented
	// as a bearer token and match in constant time. When no token is
	// configured the unguessable trigger id is the only guard.
	if strings.TrimSpace(cfg.RunToken) != "" {
		if !s.manualRunAuthorised(disp, tr.ID, cfg.RunToken, c.GetHeader("Authorization")) {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, manualRunMaxBody))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data := map[string]interface{}{}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &data); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
			return
		}
	}

	if fields := manualtrigger.ValidateTriggerInputs(cfg.TriggerInputs, data); len(fields) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "missing or invalid inputs",
			"fields": fields,
		})
		return
	}

	if err := disp.Trigger(tr, data); err != nil {
		log.WithFields(log.Fields{"error": err, "id": id}).Error("manual run: dispatch failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "unable to dispatch flow"})
		return
	}

	// The dispatch API returns no execution id (it is fire-and-forget),
	// so we acknowledge acceptance.
	c.JSON(http.StatusAccepted, gin.H{"status": "accepted"})
}

// manualRunAuthorised resolves the (possibly ${secrets.X}) run token and
// compares it in constant time against the presented bearer token.
func (s *Service) manualRunAuthorised(disp manualTriggerDispatcher, triggerID, runToken, authHeader string) bool {
	expected := strings.TrimSpace(runToken)
	if strings.Contains(expected, "${") {
		resolved, err := disp.ResolveVariables(triggerID, []string{runToken})
		if err != nil || resolved[runToken] == "" {
			log.WithFields(log.Fields{"id": triggerID}).Warn("manual run: unable to resolve run token")
			return false
		}
		expected = resolved[runToken]
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		return false
	}
	presented := strings.TrimSpace(parts[1])
	if presented == "" {
		return false
	}

	return hmac.Equal([]byte(presented), []byte(expected))
}
