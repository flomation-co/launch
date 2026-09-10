package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	. "github.com/onsi/gomega"
)

func TestFormSubmitRedirectDest(t *testing.T) {
	RegisterTestingT(t)

	// No submit config → back to the form with ?submitted=1.
	Expect(formSubmitRedirectDest("abc", formDefinition{})).To(Equal("/form/abc?submitted=1"))

	// Redirect mode with an http(s) URL → that URL.
	def := formDefinition{Submit: &formSubmit{OnSubmit: "redirect", RedirectURL: "https://example.com/thanks"}}
	Expect(formSubmitRedirectDest("abc", def)).To(Equal("https://example.com/thanks"))

	// Redirect mode but a non-http(s) URL (javascript:, relative, empty) → fallback.
	for _, bad := range []string{"javascript:alert(1)", "/local", ""} {
		d := formDefinition{Submit: &formSubmit{OnSubmit: "redirect", RedirectURL: bad}}
		Expect(formSubmitRedirectDest("abc", d)).To(Equal("/form/abc?submitted=1"), "url %q should fall back", bad)
	}

	// success_message set but OnSubmit is "message" (not redirect) → still fallback.
	msg := formDefinition{Submit: &formSubmit{OnSubmit: "message", RedirectURL: "https://x"}}
	Expect(formSubmitRedirectDest("abc", msg)).To(Equal("/form/abc?submitted=1"))
}

func TestParseFormPostBody_Urlencoded(t *testing.T) {
	RegisterTestingT(t)
	gin.SetMode(gin.TestMode)

	var got map[string]interface{}
	var isForm bool
	router := gin.New()
	router.POST("/p", func(c *gin.Context) {
		isForm = isBrowserFormPost(c)
		got = parseFormPostBody(c)
		c.Status(http.StatusOK)
	})

	// A single value → string; a repeated field (checkbox group) → array, order preserved.
	body := "name=Ada&features=alpha&features=gamma&note=hi"
	req := httptest.NewRequest(http.MethodPost, "/p", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	router.ServeHTTP(httptest.NewRecorder(), req)

	Expect(isForm).To(BeTrue())
	Expect(got["name"]).To(Equal("Ada"))
	Expect(got["note"]).To(Equal("hi"))
	Expect(got["features"]).To(Equal([]interface{}{"alpha", "gamma"}))
}

func TestParseFormPostBody_Multipart(t *testing.T) {
	RegisterTestingT(t)
	gin.SetMode(gin.TestMode)

	var got map[string]interface{}
	var isForm bool
	router := gin.New()
	router.POST("/p", func(c *gin.Context) {
		isForm = isBrowserFormPost(c)
		got = parseFormPostBody(c)
		c.Status(http.StatusOK)
	})

	// Minimal multipart body with two fields, one repeated.
	boundary := "X"
	part := func(name, val string) string {
		return "--" + boundary + "\r\nContent-Disposition: form-data; name=\"" + name + "\"\r\n\r\n" + val + "\r\n"
	}
	body := part("email", "a@b.com") + part("opt", "one") + part("opt", "two") + "--" + boundary + "--\r\n"
	req := httptest.NewRequest(http.MethodPost, "/p", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	router.ServeHTTP(httptest.NewRecorder(), req)

	Expect(isForm).To(BeTrue())
	Expect(got["email"]).To(Equal("a@b.com"))
	Expect(got["opt"]).To(Equal([]interface{}{"one", "two"}))
}

func TestIsBrowserFormPost_JSONIsNotAForm(t *testing.T) {
	RegisterTestingT(t)
	gin.SetMode(gin.TestMode)

	var isForm bool
	router := gin.New()
	router.POST("/p", func(c *gin.Context) { isForm = isBrowserFormPost(c); c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/p", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)
	Expect(isForm).To(BeFalse())
}
