package gwauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	. "github.com/onsi/gomega"
)

func hash(secret, salt string) string {
	sum := sha256.Sum256([]byte(salt + secret))
	return hex.EncodeToString(sum[:])
}

func req(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestOpenAlwaysOK(t *testing.T) {
	RegisterTestingT(t)
	Expect(Open{}.Authenticate(req(nil)).OK).To(BeTrue())
}

func TestAPIKey(t *testing.T) {
	RegisterTestingT(t)
	a := APIKey{Header: "X-API-Key", Hash: hash("k3y", "salt"), Salt: "salt"}
	Expect(a.Authenticate(req(map[string]string{"X-API-Key": "k3y"})).OK).To(BeTrue())
	Expect(a.Authenticate(req(map[string]string{"X-API-Key": "wrong"})).OK).To(BeFalse())
	Expect(a.Authenticate(req(nil)).OK).To(BeFalse()) // missing header
	// default header name when unset
	d := APIKey{Hash: hash("k3y", "salt"), Salt: "salt"}
	Expect(d.Authenticate(req(map[string]string{"X-API-Key": "k3y"})).OK).To(BeTrue())
}

func TestBasic(t *testing.T) {
	RegisterTestingT(t)
	b := Basic{Username: "alice", Hash: hash("pw", "s"), Salt: "s"}
	cred := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:pw"))
	Expect(b.Authenticate(req(map[string]string{"Authorization": cred})).OK).To(BeTrue())
	bad := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:nope"))
	r := b.Authenticate(req(map[string]string{"Authorization": bad}))
	Expect(r.OK).To(BeFalse())
	Expect(r.Challenge).To(ContainSubstring("Basic realm="))
	wrongUser := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:pw"))
	Expect(b.Authenticate(req(map[string]string{"Authorization": wrongUser})).OK).To(BeFalse())
	Expect(b.Authenticate(req(nil)).OK).To(BeFalse())
}

func TestOIDC(t *testing.T) {
	RegisterTestingT(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyfunc := func(*jwt.Token) (interface{}, error) { return &key.PublicKey, nil }

	sign := func(claims jwt.MapClaims) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		s, _ := tok.SignedString(key)
		return s
	}
	exp := time.Now().Add(time.Hour).Unix()

	o := OIDC{Issuer: "https://id.test", Audience: "my-api", RequiredClaims: map[string]interface{}{"role": "admin"}, Keyfunc: keyfunc}

	good := sign(jwt.MapClaims{"iss": "https://id.test", "aud": "my-api", "exp": exp, "role": "admin", "sub": "u-1"})
	r := o.Authenticate(req(map[string]string{"Authorization": "Bearer " + good}))
	Expect(r.OK).To(BeTrue())
	Expect(r.Claims).To(HaveKeyWithValue("sub", "u-1"))

	// wrong audience
	Expect(o.Authenticate(req(map[string]string{"Authorization": "Bearer " + sign(jwt.MapClaims{"iss": "https://id.test", "aud": "other", "exp": exp, "role": "admin"})})).OK).To(BeFalse())
	// wrong issuer
	Expect(o.Authenticate(req(map[string]string{"Authorization": "Bearer " + sign(jwt.MapClaims{"iss": "https://evil", "aud": "my-api", "exp": exp, "role": "admin"})})).OK).To(BeFalse())
	// missing required claim → 403
	miss := o.Authenticate(req(map[string]string{"Authorization": "Bearer " + sign(jwt.MapClaims{"iss": "https://id.test", "aud": "my-api", "exp": exp})}))
	Expect(miss.OK).To(BeFalse())
	Expect(miss.Status).To(Equal(http.StatusForbidden))
	// expired
	Expect(o.Authenticate(req(map[string]string{"Authorization": "Bearer " + sign(jwt.MapClaims{"iss": "https://id.test", "aud": "my-api", "exp": time.Now().Add(-time.Hour).Unix(), "role": "admin"})})).OK).To(BeFalse())
	// no token
	Expect(o.Authenticate(req(nil)).OK).To(BeFalse())
}

// alg=none must never authenticate.
func TestOIDCRejectsNoneAlg(t *testing.T) {
	RegisterTestingT(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	o := OIDC{Issuer: "https://id.test", Keyfunc: func(*jwt.Token) (interface{}, error) { return &key.PublicKey, nil }}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": "https://id.test", "exp": time.Now().Add(time.Hour).Unix()})
	unsigned, _ := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	Expect(o.Authenticate(req(map[string]string{"Authorization": "Bearer " + unsigned})).OK).To(BeFalse())
}
