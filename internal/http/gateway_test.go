package http

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	. "github.com/onsi/gomega"
)

func TestMatchPattern(t *testing.T) {
	RegisterTestingT(t)

	params, ok, static := matchPattern("/users/:id", []string{"users", "42"})
	Expect(ok).To(BeTrue())
	Expect(params).To(HaveKeyWithValue("id", "42"))
	Expect(static).To(Equal(1))

	_, ok, _ = matchPattern("/users/:id", []string{"users"}) // length mismatch
	Expect(ok).To(BeFalse())

	_, ok, _ = matchPattern("/users/list", []string{"users", "42"}) // static mismatch
	Expect(ok).To(BeFalse())
}

func TestMatchGatewayEndpoint_StaticBeatsParam(t *testing.T) {
	RegisterTestingT(t)
	eps := []gatewayEndpoint{
		{Method: "GET", PathPattern: "/users/:id", FlowID: "flow-param"},
		{Method: "GET", PathPattern: "/users/me", FlowID: "flow-static"},
	}
	// /users/me matches BOTH; the static route wins.
	m, _, _ := matchGatewayEndpoint(eps, "GET", "/users/me")
	Expect(m).ToNot(BeNil())
	Expect(m.FlowID).To(Equal("flow-static"))
	// /users/42 only matches the param route.
	m, params, _ := matchGatewayEndpoint(eps, "GET", "/users/42")
	Expect(m.FlowID).To(Equal("flow-param"))
	Expect(params).To(HaveKeyWithValue("id", "42"))
}

func TestMatchGatewayEndpoint_MethodNotAllowed(t *testing.T) {
	RegisterTestingT(t)
	eps := []gatewayEndpoint{
		{Method: "GET", PathPattern: "/users/:id"},
		{Method: "DELETE", PathPattern: "/users/:id"},
	}
	m, _, allowed := matchGatewayEndpoint(eps, "POST", "/users/42")
	Expect(m).To(BeNil())
	Expect(allowed).To(ConsistOf("GET", "DELETE"))
}

func hashSecret(secret, salt string) string {
	sum := sha256.Sum256([]byte(salt + secret))
	return hex.EncodeToString(sum[:])
}

// gatewayAPI builds a fake API serving the gateway resolve + flow execute/poll,
// so the handler can be exercised end-to-end. resolution is returned verbatim.
func gatewayAPI(resolution map[string]interface{}) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/internal/gateway/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(resolution)
	})
	mux.HandleFunc("/api/v1/internal/flo/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "exec-1"})
	})
	mux.HandleFunc("/api/v1/internal/execution/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"execution_status": "executed", "completion_status": "success",
			"result": map[string]interface{}{"outputs": map[string]interface{}{
				webResponseKey: map[string]interface{}{"body": `{"ok":true}`, "status_code": float64(200), "content_type": "application/json"},
			}},
		})
	})
	return httptest.NewServer(mux)
}

func doGateway(url, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/gw/:apiId/*path", newInvokeService(url).handleGateway)
	req := httptest.NewRequest(method, target, strings.NewReader(""))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGatewayInvoke_OpenAuthDispatches(t *testing.T) {
	RegisterTestingT(t)
	defer withFastPolling(2*time.Second, 2*time.Millisecond)()

	srv := gatewayAPI(map[string]interface{}{
		"api_id": "abc", "auth_type": "open",
		"endpoints": []map[string]interface{}{
			{"method": "GET", "path_pattern": "/users/:id", "flow_id": "flow-1", "trigger_id": "trig-1"},
		},
	})
	defer srv.Close()

	w := doGateway(srv.URL, http.MethodGet, "/gw/abc/users/42", nil)
	Expect(w.Code).To(Equal(http.StatusOK))
	Expect(w.Body.String()).To(ContainSubstring(`"ok":true`))
}

func TestGatewayInvoke_APIKeyRejectsWithoutKey(t *testing.T) {
	RegisterTestingT(t)

	srv := gatewayAPI(map[string]interface{}{
		"api_id": "abc", "auth_type": "api_key",
		"auth_config":      map[string]interface{}{"header": "X-API-Key"},
		"auth_secret_hash": hashSecret("k3y", "s"), "auth_secret_salt": "s",
		"endpoints": []map[string]interface{}{
			{"method": "GET", "path_pattern": "/ping", "flow_id": "flow-1", "trigger_id": "trig-1"},
		},
	})
	defer srv.Close()

	Expect(doGateway(srv.URL, http.MethodGet, "/gw/abc/ping", nil).Code).To(Equal(http.StatusUnauthorized))
	Expect(doGateway(srv.URL, http.MethodGet, "/gw/abc/ping", map[string]string{"X-API-Key": "wrong"}).Code).To(Equal(http.StatusUnauthorized))
}

func TestGatewayInvoke_APIKeyAcceptsValidKey(t *testing.T) {
	RegisterTestingT(t)
	defer withFastPolling(2*time.Second, 2*time.Millisecond)()

	srv := gatewayAPI(map[string]interface{}{
		"api_id": "abc", "auth_type": "api_key",
		"auth_secret_hash": hashSecret("k3y", "s"), "auth_secret_salt": "s",
		"endpoints": []map[string]interface{}{
			{"method": "GET", "path_pattern": "/ping", "flow_id": "flow-1", "trigger_id": "trig-1"},
		},
	})
	defer srv.Close()

	Expect(doGateway(srv.URL, http.MethodGet, "/gw/abc/ping", map[string]string{"X-API-Key": "k3y"}).Code).To(Equal(http.StatusOK))
}

func TestGatewayInvoke_MethodNotAllowed(t *testing.T) {
	RegisterTestingT(t)
	srv := gatewayAPI(map[string]interface{}{
		"api_id": "abc", "auth_type": "open",
		"endpoints": []map[string]interface{}{
			{"method": "GET", "path_pattern": "/ping", "flow_id": "flow-1", "trigger_id": "trig-1"},
		},
	})
	defer srv.Close()

	w := doGateway(srv.URL, http.MethodPost, "/gw/abc/ping", nil)
	Expect(w.Code).To(Equal(http.StatusMethodNotAllowed))
	Expect(w.Header().Get("Allow")).To(ContainSubstring("GET"))
}

func TestGatewayInvoke_UnknownAPI404(t *testing.T) {
	RegisterTestingT(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/internal/gateway/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	Expect(doGateway(srv.URL, http.MethodGet, "/gw/nope/x", nil).Code).To(Equal(http.StatusNotFound))
}
