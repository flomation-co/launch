package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

// execAPIStub serves GET /api/v1/internal/execution/:id with a fixed flo id,
// status and result, so FetchExecution's parse + flo-id extraction (which the
// result-page poll endpoint's ownership check depends on) can be tested
// hermetically.
func execAPIStub(floID, status string, outputs map[string]interface{}, code int) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/internal/execution/", func(w http.ResponseWriter, r *http.Request) {
		if code != 0 && code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		var result map[string]interface{}
		if status == "executed" {
			result = map[string]interface{}{"outputs": outputs}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"flo_id":            floID,
			"execution_status":  status,
			"completion_status": "success",
			"result":            result,
		})
	})
	return httptest.NewServer(mux)
}

func execResolver(url string) *formDataResolver {
	return newFormDataResolver(&http.Client{Timeout: 2 * time.Second}, url)
}

// TestFetchExecution_Executed: a completed execution yields its flow id (for the
// ownership check) and the inner result.outputs map.
func TestFetchExecution_Executed(t *testing.T) {
	RegisterTestingT(t)
	srv := execAPIStub("flow-abc", "executed", map[string]interface{}{"public_ip": "203.0.113.9"}, 0)
	defer srv.Close()

	floID, status, outputs, ok := execResolver(srv.URL).FetchExecution("exec-1")
	Expect(ok).To(BeTrue())
	Expect(floID).To(Equal("flow-abc"))
	Expect(status).To(Equal("executed"))
	Expect(outputs).To(HaveKeyWithValue("public_ip", "203.0.113.9"))
}

// TestFetchExecution_Running: an in-progress execution reports its flow id and
// status but no outputs yet (the result page keeps spinning).
func TestFetchExecution_Running(t *testing.T) {
	RegisterTestingT(t)
	srv := execAPIStub("flow-abc", "running", nil, 0)
	defer srv.Close()

	floID, status, outputs, ok := execResolver(srv.URL).FetchExecution("exec-1")
	Expect(ok).To(BeTrue())
	Expect(floID).To(Equal("flow-abc"))
	Expect(status).To(Equal("running"))
	Expect(outputs).To(BeNil())
}

// TestFetchExecution_NotFound: a non-2xx response is a clean miss (ok=false), so
// the poll endpoint 404s rather than leaking anything.
func TestFetchExecution_NotFound(t *testing.T) {
	RegisterTestingT(t)
	srv := execAPIStub("", "", nil, http.StatusNotFound)
	defer srv.Close()

	_, _, _, ok := execResolver(srv.URL).FetchExecution("exec-1")
	Expect(ok).To(BeFalse())
}

// TestFetchExecution_NilResolver: a nil resolver (mTLS not configured) is a
// clean miss, not a panic.
func TestFetchExecution_NilResolver(t *testing.T) {
	RegisterTestingT(t)
	var r *formDataResolver
	_, _, _, ok := r.FetchExecution("exec-1")
	Expect(ok).To(BeFalse())
}
