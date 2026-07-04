package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

// fakeAPI stands in for the internal API: POST /flo/:id/execute returns an
// execution id; GET /execution/:id reports "running" for the first
// pollsBeforeDone polls, then "executed" with the configured result.
type fakeAPI struct {
	executeCount int32
	pollCount    int32
	result       map[string]interface{}
	// pollsBeforeDone is how many polls return "running" before "executed".
	pollsBeforeDone int32
	// execDelay optionally slows the execute call so concurrent callers
	// overlap (used to exercise singleflight).
	execDelay time.Duration
}

func (f *fakeAPI) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/internal/flo/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.executeCount, 1)
		if f.execDelay > 0 {
			time.Sleep(f.execDelay)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "exec-1"})
	})
	mux.HandleFunc("/api/v1/internal/execution/", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&f.pollCount, 1)
		status := "running"
		var result map[string]interface{}
		if n > f.pollsBeforeDone {
			status = "executed"
			result = f.result
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"execution_status":  status,
			"completion_status": "success",
			"result":            result,
		})
	})
	return httptest.NewServer(mux)
}

func newTestResolver(url string) *formDataResolver {
	r := newFormDataResolver(&http.Client{Timeout: 2 * time.Second}, url)
	r.pollInterval = 2 * time.Millisecond // keep tests fast
	r.timeout = 2 * time.Second
	return r
}

func TestFormDataResolver_RunsFlowAndFlattensOutputs(t *testing.T) {
	RegisterTestingT(t)

	api := &fakeAPI{
		pollsBeforeDone: 1,
		result: map[string]interface{}{
			"customer_name": "Ada Lovelace",
			"seats":         float64(5),
			"active":        true,
		},
	}
	srv := api.server()
	defer srv.Close()

	vars := newTestResolver(srv.URL).Resolve("flow-1", 0)
	Expect(vars).To(HaveKeyWithValue("customer_name", "Ada Lovelace"))
	Expect(vars).To(HaveKeyWithValue("seats", "5")) // whole float loses the .0
	Expect(vars).To(HaveKeyWithValue("active", "true"))
	Expect(atomic.LoadInt32(&api.executeCount)).To(Equal(int32(1)))
}

func TestFormDataResolver_CachesWithinTTL(t *testing.T) {
	RegisterTestingT(t)

	api := &fakeAPI{pollsBeforeDone: 0, result: map[string]interface{}{"x": "1"}}
	srv := api.server()
	defer srv.Close()

	r := newTestResolver(srv.URL)
	first := r.Resolve("flow-1", 0)
	second := r.Resolve("flow-1", 0)
	Expect(first).To(Equal(second))
	// Second call served from cache — the flow ran exactly once.
	Expect(atomic.LoadInt32(&api.executeCount)).To(Equal(int32(1)))
}

func TestFormDataResolver_SingleFlightDeduplicatesConcurrentLoads(t *testing.T) {
	RegisterTestingT(t)

	// Slow execute so all callers pile up behind one in-flight execution.
	api := &fakeAPI{pollsBeforeDone: 0, execDelay: 40 * time.Millisecond, result: map[string]interface{}{"x": "1"}}
	srv := api.server()
	defer srv.Close()

	r := newTestResolver(srv.URL)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Resolve("flow-1", 0) }()
	}
	wg.Wait()

	// 20 concurrent loads collapsed into a SINGLE flow execution.
	Expect(atomic.LoadInt32(&api.executeCount)).To(Equal(int32(1)))
}

func TestFormDataResolver_TimeoutReturnsEmpty(t *testing.T) {
	RegisterTestingT(t)

	// Never completes — always "running".
	api := &fakeAPI{pollsBeforeDone: 1 << 30, result: map[string]interface{}{"x": "1"}}
	srv := api.server()
	defer srv.Close()

	r := newTestResolver(srv.URL)
	r.timeout = 30 * time.Millisecond
	vars := r.Resolve("flow-1", 0)
	Expect(vars).To(Equal(map[string]string{}))
}

func TestFormDataResolver_NilAndEmptyShortCircuit(t *testing.T) {
	RegisterTestingT(t)

	var nilResolver *formDataResolver
	Expect(nilResolver.Resolve("flow-1", 0)).To(BeNil())

	api := &fakeAPI{}
	srv := api.server()
	defer srv.Close()
	Expect(newTestResolver(srv.URL).Resolve("", 0)).To(BeNil())
	Expect(atomic.LoadInt32(&api.executeCount)).To(Equal(int32(0)))
}

func TestFlattenOutputs_Types(t *testing.T) {
	RegisterTestingT(t)

	out := flattenOutputs(map[string]interface{}{
		"str":  "hello",
		"num":  float64(42),
		"bool": false,
		"arr":  []interface{}{"a", "b"},
		"null": nil,
	})
	Expect(out).To(HaveKeyWithValue("str", "hello"))
	Expect(out).To(HaveKeyWithValue("num", "42"))
	Expect(out).To(HaveKeyWithValue("bool", "false"))
	Expect(out).To(HaveKeyWithValue("arr", "a,b"))
	Expect(out).To(HaveKeyWithValue("null", ""))
}

func TestApplySubstitutions_DataNamespace(t *testing.T) {
	RegisterTestingT(t)

	ctx := substitutionContext{DataVariables: map[string]string{"customer_name": "Ada"}}
	Expect(applySubstitutions("Hi ${data.customer_name}", ctx)).To(Equal("Hi Ada"))
	// Missing key resolves to empty, nil map is safe.
	Expect(applySubstitutions("${data.missing}", ctx)).To(Equal(""))
	Expect(applySubstitutions("${data.x}", substitutionContext{})).To(Equal(""))
}
