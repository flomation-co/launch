package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flomation.app/automate/launch"
	"github.com/gin-gonic/gin"
	. "github.com/onsi/gomega"
)

// stubDispatcher is a test double for the trigger service slice used by
// the manual run handler.
type stubDispatcher struct {
	trigger      *launch.Trigger
	getErr       error
	resolved     map[string]string
	resolveErr   error
	dispatchErr  error
	dispatched   bool
	dispatchData interface{}
}

func (s *stubDispatcher) GetTriggerByID(id string) (*launch.Trigger, error) {
	return s.trigger, s.getErr
}

func (s *stubDispatcher) ResolveVariables(triggerID string, variables []string) (map[string]string, error) {
	return s.resolved, s.resolveErr
}

func (s *stubDispatcher) Trigger(trigger *launch.Trigger, data interface{}) error {
	s.dispatched = true
	s.dispatchData = data
	return s.dispatchErr
}

const testTriggerID = "11111111-1111-1111-1111-111111111111"

func manualTrigger(t *testing.T, data map[string]interface{}) *launch.Trigger {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal trigger data: %v", err)
	}
	return &launch.Trigger{ID: testTriggerID, Type: launch.TriggerTypeManual, FlowID: "flow-1", Data: b}
}

// invoke drives runManualTrigger through a gin router with the given
// stub, body, and Authorization header.
func invoke(disp manualTriggerDispatcher, id, body, auth string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	s := &Service{}
	engine.POST("/trigger/:id/run", func(c *gin.Context) {
		s.runManualTrigger(c, disp)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/trigger/"+id+"/run", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	engine.ServeHTTP(w, req)
	return w
}

func TestManualRun_NotFoundForNonManualTrigger(t *testing.T) {
	RegisterTestingT(t)
	disp := &stubDispatcher{trigger: &launch.Trigger{ID: testTriggerID, Type: launch.TriggerTypeWebhook}}
	w := invoke(disp, testTriggerID, `{}`, "")
	Expect(w.Code).To(Equal(http.StatusNotFound))
	Expect(disp.dispatched).To(BeFalse())
}

func TestManualRun_NotFoundForMissingTrigger(t *testing.T) {
	RegisterTestingT(t)
	disp := &stubDispatcher{trigger: nil}
	w := invoke(disp, testTriggerID, `{}`, "")
	Expect(w.Code).To(Equal(http.StatusNotFound))
}

func TestManualRun_BadTriggerID(t *testing.T) {
	RegisterTestingT(t)
	disp := &stubDispatcher{}
	w := invoke(disp, "not-a-uuid", `{}`, "")
	Expect(w.Code).To(Equal(http.StatusBadRequest))
}

func TestManualRun_UnauthorisedWhenTokenMissing(t *testing.T) {
	RegisterTestingT(t)
	tr := manualTrigger(t, map[string]interface{}{"run_token": "s3cret"})
	disp := &stubDispatcher{trigger: tr}
	// No Authorization header.
	w := invoke(disp, testTriggerID, `{}`, "")
	Expect(w.Code).To(Equal(http.StatusUnauthorized))
	Expect(disp.dispatched).To(BeFalse())
}

func TestManualRun_UnauthorisedWhenTokenWrong(t *testing.T) {
	RegisterTestingT(t)
	tr := manualTrigger(t, map[string]interface{}{"run_token": "s3cret"})
	disp := &stubDispatcher{trigger: tr}
	w := invoke(disp, testTriggerID, `{}`, "Bearer wrong")
	Expect(w.Code).To(Equal(http.StatusUnauthorized))
	Expect(disp.dispatched).To(BeFalse())
}

func TestManualRun_AuthorisedWithCorrectToken(t *testing.T) {
	RegisterTestingT(t)
	tr := manualTrigger(t, map[string]interface{}{"run_token": "s3cret"})
	disp := &stubDispatcher{trigger: tr}
	w := invoke(disp, testTriggerID, `{}`, "Bearer s3cret")
	Expect(w.Code).To(Equal(http.StatusAccepted))
	Expect(disp.dispatched).To(BeTrue())
}

func TestManualRun_ResolvesSecretRunToken(t *testing.T) {
	RegisterTestingT(t)
	tr := manualTrigger(t, map[string]interface{}{"run_token": "${secrets.RUN}"})
	disp := &stubDispatcher{trigger: tr, resolved: map[string]string{"${secrets.RUN}": "actual-secret"}}
	// Wrong token → 401.
	Expect(invoke(disp, testTriggerID, `{}`, "Bearer nope").Code).To(Equal(http.StatusUnauthorized))
	// Correct resolved token → 202.
	Expect(invoke(disp, testTriggerID, `{}`, "Bearer actual-secret").Code).To(Equal(http.StatusAccepted))
}

func TestManualRun_MissingRequiredInputNamesFields(t *testing.T) {
	RegisterTestingT(t)
	tr := manualTrigger(t, map[string]interface{}{
		"trigger_inputs": []map[string]interface{}{
			{"name": "email", "type": "string", "required": true},
		},
	})
	disp := &stubDispatcher{trigger: tr}
	w := invoke(disp, testTriggerID, `{}`, "")
	Expect(w.Code).To(Equal(http.StatusBadRequest))
	Expect(disp.dispatched).To(BeFalse())

	var resp struct {
		Error  string   `json:"error"`
		Fields []string `json:"fields"`
	}
	Expect(json.Unmarshal(w.Body.Bytes(), &resp)).To(Succeed())
	Expect(resp.Error).To(Equal("missing or invalid inputs"))
	Expect(resp.Fields).To(ConsistOf("email"))
}

func TestManualRun_BadJSONBody(t *testing.T) {
	RegisterTestingT(t)
	tr := manualTrigger(t, map[string]interface{}{})
	disp := &stubDispatcher{trigger: tr}
	w := invoke(disp, testTriggerID, `{not json`, "")
	Expect(w.Code).To(Equal(http.StatusBadRequest))
}

func TestManualRun_SuccessDispatchesData(t *testing.T) {
	RegisterTestingT(t)
	tr := manualTrigger(t, map[string]interface{}{
		"trigger_inputs": []map[string]interface{}{
			{"name": "email", "type": "string", "required": true},
		},
	})
	disp := &stubDispatcher{trigger: tr}
	w := invoke(disp, testTriggerID, `{"email":"a@b.com"}`, "")
	Expect(w.Code).To(Equal(http.StatusAccepted))
	Expect(disp.dispatched).To(BeTrue())

	data, ok := disp.dispatchData.(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(data["email"]).To(Equal("a@b.com"))

	var resp map[string]string
	Expect(json.Unmarshal(w.Body.Bytes(), &resp)).To(Succeed())
	Expect(resp["status"]).To(Equal("accepted"))
}

func TestManualRun_BodyIsBounded(t *testing.T) {
	RegisterTestingT(t)
	tr := manualTrigger(t, map[string]interface{}{})
	disp := &stubDispatcher{trigger: tr}
	// Build a valid-JSON body far larger than the 256 KiB cap. The
	// LimitReader truncates it, so json.Unmarshal sees a cut-off document
	// and returns a parse error → 400 (never OOMs, never dispatches).
	huge := `{"blob":"` + strings.Repeat("a", manualRunMaxBody+1024) + `"}`
	w := invoke(disp, testTriggerID, huge, "")
	Expect(w.Code).To(Equal(http.StatusBadRequest))
	Expect(disp.dispatched).To(BeFalse())
}
