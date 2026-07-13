package http

import (
	"net/http"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

// The draft submission id is a transport-only field: it must be pulled out of
// the submission body so it can never masquerade as a form answer in the
// trigger data.
func TestExtractSubmissionID_StripsAndReturns(t *testing.T) {
	RegisterTestingT(t)

	body := map[string]interface{}{
		"name":            "Ada",
		"__submission_id": "11111111-1111-1111-1111-111111111111",
	}
	sid := extractSubmissionID(body)

	Expect(sid).To(Equal("11111111-1111-1111-1111-111111111111"))
	_, present := body["__submission_id"]
	Expect(present).To(BeFalse())
	// Genuine answers are untouched.
	Expect(body["name"]).To(Equal("Ada"))
}

// A body with no submission id yields an empty string and is otherwise
// unchanged — older clients submit unguarded.
func TestExtractSubmissionID_AbsentIsEmpty(t *testing.T) {
	RegisterTestingT(t)

	body := map[string]interface{}{"name": "Grace"}
	Expect(extractSubmissionID(body)).To(Equal(""))
	Expect(body["name"]).To(Equal("Grace"))
}

// A non-string submission id (crafted client) is treated as absent and still
// removed, so it can never reach the trigger.
func TestExtractSubmissionID_NonStringDropped(t *testing.T) {
	RegisterTestingT(t)

	body := map[string]interface{}{"__submission_id": 42}
	Expect(extractSubmissionID(body)).To(Equal(""))
	_, present := body["__submission_id"]
	Expect(present).To(BeFalse())
}

func TestAutosaveBodyStatus_AcceptsJSONObject(t *testing.T) {
	RegisterTestingT(t)
	Expect(autosaveBodyStatus([]byte(`{"a":1,"b":"two"}`))).To(Equal(0))
	Expect(autosaveBodyStatus([]byte(`{}`))).To(Equal(0))
}

func TestAutosaveBodyStatus_RejectsNonObject(t *testing.T) {
	RegisterTestingT(t)
	Expect(autosaveBodyStatus([]byte(`not json`))).To(Equal(http.StatusBadRequest))
	Expect(autosaveBodyStatus([]byte(`[1,2,3]`))).To(Equal(http.StatusBadRequest))
	Expect(autosaveBodyStatus([]byte(`"a string"`))).To(Equal(http.StatusBadRequest))
	Expect(autosaveBodyStatus([]byte(``))).To(Equal(http.StatusBadRequest))
}

func TestAutosaveBodyStatus_RejectsOversize(t *testing.T) {
	RegisterTestingT(t)

	// Simulate the handler's read: LimitReader(body, max+1) yields at most
	// max+1 bytes, and anything over the cap must be rejected as 413.
	oversize := []byte(`{"x":"` + strings.Repeat("a", formDraftMaxBytes) + `"}`)
	Expect(len(oversize) > formDraftMaxBytes).To(BeTrue())
	Expect(autosaveBodyStatus(oversize)).To(Equal(http.StatusRequestEntityTooLarge))

	// A payload right at the cap is size-accepted (and, being a JSON object,
	// passes overall).
	atCap := make([]byte, formDraftMaxBytes)
	atCap[0] = '{'
	for i := 1; i < formDraftMaxBytes-1; i++ {
		atCap[i] = ' '
	}
	atCap[formDraftMaxBytes-1] = '}'
	Expect(autosaveBodyStatus(atCap)).To(Equal(0))
}
