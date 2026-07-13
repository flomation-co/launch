package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"flomation.app/automate/launch/internal/config"
	"github.com/gin-gonic/gin"
	. "github.com/onsi/gomega"
)

// historyAPI is a stateful fake API for the history-orchestration test: it serves
// the web-trigger config (keep_history on), the web_thread endpoints, execute, and
// an execution whose Web Response carries a distinct history text.
func historyAPI(appended *[]map[string]string, executeBody *map[string]interface{}, mu *sync.Mutex) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/internal/flo/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/web-trigger") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"found": true, "keep_history": true, "message_field": "message",
			})
			return
		}
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(executeBody)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "exec-1"})
	})
	mux.HandleFunc("/api/v1/internal/execution/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"execution_status": "executed",
			"result": map[string]interface{}{"outputs": map[string]interface{}{
				webResponseKey: map[string]interface{}{"body": `{"content":"Hi"}`, "history": "Hi there"},
			}},
		})
	})
	mux.HandleFunc("/api/v1/internal/web-thread", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"thread_id": "t-new"})
	})
	mux.HandleFunc("/api/v1/internal/web-thread/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/history") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"turns": []map[string]string{{"role": "user", "content": "earlier"}},
			})
			return
		}
		var turn map[string]string
		_ = json.NewDecoder(r.Body).Decode(&turn)
		mu.Lock()
		*appended = append(*appended, turn)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})
	return httptest.NewServer(mux)
}

func TestWebInvoke_HistoryOrchestration(t *testing.T) {
	RegisterTestingT(t)
	defer withFastPolling(2*time.Second, 2*time.Millisecond)()

	var appended []map[string]string
	var executeBody map[string]interface{}
	var mu sync.Mutex
	srv := historyAPI(&appended, &executeBody, &mu)
	defer srv.Close()

	w := doInvoke(newInvokeService(srv.URL), http.MethodPost, "/v1/embed/flow/"+testFlowID+"/invoke", `{"message":"hello"}`)

	Expect(w.Code).To(Equal(200))
	Expect(w.Header().Get(threadHeader)).To(Equal("t-new")) // minted thread round-tripped

	// ${history} injected from the thread.
	mu.Lock()
	defer mu.Unlock()
	Expect(executeBody["history"]).To(Not(BeNil()))
	// Both turns recorded; the assistant turn uses the Web Response history text, not the body.
	Expect(appended).To(HaveLen(2))
	Expect(appended[0]).To(Equal(map[string]string{"role": "user", "content": "hello"}))
	Expect(appended[1]).To(Equal(map[string]string{"role": "assistant", "content": "Hi there"}))
}

func TestWebInvoke_MethodNotAllowed(t *testing.T) {
	RegisterTestingT(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/internal/flo/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"found": true, "methods": []string{"POST"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := doInvoke(newInvokeService(srv.URL), http.MethodGet, "/v1/embed/flow/"+testFlowID+"/invoke", "")

	Expect(w.Code).To(Equal(http.StatusMethodNotAllowed))
	Expect(w.Header().Get("Allow")).To(ContainSubstring("POST"))
}

func TestWebInvoke_ForwardsAuthHeader(t *testing.T) {
	RegisterTestingT(t)
	defer withFastPolling(2*time.Second, 2*time.Millisecond)()

	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/internal/flo/", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "exec-1"})
	})
	mux.HandleFunc("/api/v1/internal/execution/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"execution_status": "executed",
			"result":           map[string]interface{}{"outputs": map[string]interface{}{}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/v1/embed/flow/:id/invoke", newInvokeService(srv.URL).handleEmbedFlowInvoke)
	req := httptest.NewRequest(http.MethodPost, "/v1/embed/flow/"+testFlowID+"/invoke", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer user-jwt")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	Expect(gotAuth).To(Equal("Bearer user-jwt")) // forwarded to the API execute call
}

func newInvokeService(url string) *Service {
	return &Service{
		config:    &config.Config{Automate: config.ServiceConfig{URL: url}},
		apiClient: &http.Client{Timeout: 2 * time.Second},
	}
}

// withFastPolling shrinks the hang timing for a test and restores it after.
func withFastPolling(timeout, interval time.Duration) func() {
	pt, pi := webInvokeTimeout, webInvokePollInterval
	webInvokeTimeout, webInvokePollInterval = timeout, interval
	return func() { webInvokeTimeout, webInvokePollInterval = pt, pi }
}

const testFlowID = "0d7c8f2e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"

func doInvoke(s *Service, method, target string, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/v1/embed/flow/:id/invoke", s.handleEmbedFlowInvoke)
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rdr)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestWebInvoke_ReturnsWebResponse(t *testing.T) {
	RegisterTestingT(t)
	defer withFastPolling(2*time.Second, 2*time.Millisecond)()

	api := &fakeAPI{
		pollsBeforeDone: 1,
		result: map[string]interface{}{
			"outputs": map[string]interface{}{
				webResponseKey: map[string]interface{}{
					"body":         `{"ok":true}`,
					"status_code":  float64(201),
					"content_type": "application/json",
					"headers":      `{"X-Test":"1"}`,
				},
			},
		},
	}
	srv := api.server()
	defer srv.Close()

	w := doInvoke(newInvokeService(srv.URL), http.MethodPost, "/v1/embed/flow/"+testFlowID+"/invoke", `{"name":"Ada"}`)

	Expect(w.Code).To(Equal(201))
	Expect(w.Body.String()).To(Equal(`{"ok":true}`))
	Expect(w.Header().Get("Content-Type")).To(Equal("application/json"))
	Expect(w.Header().Get("X-Test")).To(Equal("1"))
}

func TestWebInvoke_DefaultWhenNoWebResponse(t *testing.T) {
	RegisterTestingT(t)
	defer withFastPolling(2*time.Second, 2*time.Millisecond)()

	api := &fakeAPI{
		pollsBeforeDone: 0,
		result:          map[string]interface{}{"outputs": map[string]interface{}{"greeting": "hi"}},
	}
	srv := api.server()
	defer srv.Close()

	w := doInvoke(newInvokeService(srv.URL), http.MethodGet, "/v1/embed/flow/"+testFlowID+"/invoke", "")

	Expect(w.Code).To(Equal(200))
	Expect(w.Body.String()).To(ContainSubstring(`"greeting":"hi"`))
}

func TestWebInvoke_TimesOutWith202(t *testing.T) {
	RegisterTestingT(t)
	defer withFastPolling(40*time.Millisecond, 5*time.Millisecond)()

	api := &fakeAPI{pollsBeforeDone: 1000, result: map[string]interface{}{}} // never completes in time
	srv := api.server()
	defer srv.Close()

	w := doInvoke(newInvokeService(srv.URL), http.MethodPost, "/v1/embed/flow/"+testFlowID+"/invoke", "{}")

	Expect(w.Code).To(Equal(http.StatusAccepted))
	Expect(w.Body.String()).To(ContainSubstring("execution_id"))
}

func TestParseWebResponse(t *testing.T) {
	RegisterTestingT(t)

	// Full response with headers as a JSON string.
	wr, ok := parseWebResponse(map[string]interface{}{
		"body": "hello", "status_code": float64(418), "content_type": "text/plain",
		"headers": `{"X-A":"1"}`,
	})
	Expect(ok).To(BeTrue())
	Expect(wr.body).To(Equal("hello"))
	Expect(wr.status).To(Equal(418))
	Expect(wr.contentType).To(Equal("text/plain"))
	Expect(wr.headers).To(HaveKeyWithValue("X-A", "1"))

	// Defaults when fields are absent.
	wr2, ok2 := parseWebResponse(map[string]interface{}{})
	Expect(ok2).To(BeTrue())
	Expect(wr2.status).To(Equal(200))
	Expect(wr2.contentType).To(Equal("application/json"))

	// Not a map ⇒ not a Web Response.
	_, ok3 := parseWebResponse("nope")
	Expect(ok3).To(BeFalse())
}

func TestToIntAndToHeaders(t *testing.T) {
	RegisterTestingT(t)

	Expect(toInt(float64(200))).To(Equal(200))
	Expect(toInt("404")).To(Equal(404))
	Expect(toInt(nil)).To(Equal(0))

	// Headers as an object.
	h := toHeaders(map[string]interface{}{"A": "1", "B": float64(2)})
	Expect(h).To(HaveKeyWithValue("A", "1"))
	Expect(h).To(HaveKeyWithValue("B", "2"))
	// Headers as a JSON string.
	h2 := toHeaders(`{"Location":"/x"}`)
	Expect(h2).To(HaveKeyWithValue("Location", "/x"))
}

func TestEmbedFlowRoutesRegister_NoConflict(t *testing.T) {
	RegisterTestingT(t)

	gin.SetMode(gin.TestMode)
	Expect(func() {
		r := gin.New()
		noop := func(c *gin.Context) {}
		grp := r.Group("/v1/embed/flow/:id", func(c *gin.Context) { c.Next() })
		grp.GET("/invoke", noop)
		grp.POST("/invoke", noop)
		grp.PUT("/invoke", noop)
		grp.PATCH("/invoke", noop)
		grp.DELETE("/invoke", noop)
		r.OPTIONS("/v1/embed/flow/:id/invoke", noop)
		// Coexists with the form embed group on the same /v1/embed prefix.
		fgrp := r.Group("/v1/embed/form/:id", func(c *gin.Context) { c.Next() })
		fgrp.POST("", noop)
	}).ToNot(Panic())
}
