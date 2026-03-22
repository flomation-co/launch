package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"flomation.app/automate/launch/internal/config"
	"github.com/gin-gonic/gin"
	. "github.com/onsi/gomega"
)

func setupTestRouter(identityServiceURL string) *gin.Engine {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{
		Security: config.SecurityConfig{
			IdentityService: identityServiceURL,
		},
	}

	engine := gin.New()

	s := &Service{
		config: cfg,
		engine: engine,
	}

	admin := engine.Group("/trigger")
	admin.Use(s.jwtMiddleware)
	admin.POST("/:id", func(c *gin.Context) {
		accountID, _ := c.Get("account_id")
		c.JSON(http.StatusOK, gin.H{"account_id": accountID})
	})
	admin.DELETE("/:id", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	return engine
}

// mockSentinel creates an httptest server that mimics the Sentinel identity service.
func mockSentinel(validToken, userID string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "Bearer "+validToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": userID})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
}

func TestJwtMiddleware_NoHeader(t *testing.T) {
	RegisterTestingT(t)

	sentinel := mockSentinel("valid-token", "user-123")
	defer sentinel.Close()

	router := setupTestRouter(sentinel.URL)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/trigger/abc", nil)

	router.ServeHTTP(w, req)
	Expect(w.Code).To(Equal(http.StatusUnauthorized))
}

func TestJwtMiddleware_MissingBearerPrefix(t *testing.T) {
	RegisterTestingT(t)

	sentinel := mockSentinel("valid-token", "user-123")
	defer sentinel.Close()

	router := setupTestRouter(sentinel.URL)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/trigger/abc", nil)
	req.Header.Set("Authorization", "valid-token")

	router.ServeHTTP(w, req)
	Expect(w.Code).To(Equal(http.StatusUnauthorized))
}

func TestJwtMiddleware_NoIdentityServiceConfigured(t *testing.T) {
	RegisterTestingT(t)

	router := setupTestRouter("")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/trigger/abc", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	router.ServeHTTP(w, req)
	Expect(w.Code).To(Equal(http.StatusUnauthorized))
}

func TestJwtMiddleware_InvalidToken(t *testing.T) {
	RegisterTestingT(t)

	sentinel := mockSentinel("valid-token", "user-123")
	defer sentinel.Close()

	router := setupTestRouter(sentinel.URL)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/trigger/abc", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")

	router.ServeHTTP(w, req)
	Expect(w.Code).To(Equal(http.StatusUnauthorized))
}

func TestJwtMiddleware_ValidToken(t *testing.T) {
	RegisterTestingT(t)

	sentinel := mockSentinel("valid-token", "user-123")
	defer sentinel.Close()

	router := setupTestRouter(sentinel.URL)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/trigger/abc", nil)
	req.Header.Set("Authorization", "Bearer valid-token")

	router.ServeHTTP(w, req)
	Expect(w.Code).To(Equal(http.StatusOK))
}

func TestJwtMiddleware_DeleteEndpoint(t *testing.T) {
	RegisterTestingT(t)

	sentinel := mockSentinel("valid-token", "user-123")
	defer sentinel.Close()

	router := setupTestRouter(sentinel.URL)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/trigger/abc", nil)
	req.Header.Set("Authorization", "Bearer valid-token")

	router.ServeHTTP(w, req)
	Expect(w.Code).To(Equal(http.StatusOK))
}
