package twilio

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"sort"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

func TestValidateSignature_EmptySignature(t *testing.T) {
	RegisterTestingT(t)

	valid := ValidateSignature("authtoken", "https://example.com", "", map[string]string{})
	Expect(valid).To(BeFalse())
}

func TestValidateSignature_WrongSignature(t *testing.T) {
	RegisterTestingT(t)

	valid := ValidateSignature("authtoken", "https://example.com", "wrongsig", map[string]string{
		"From": "+1234",
	})
	Expect(valid).To(BeFalse())
}

func TestValidateSignature_SelfConsistency(t *testing.T) {
	RegisterTestingT(t)

	authToken := "test-auth-token-12345"
	url := "https://launch.flomation.app/webhook/twilio/sms/agent-1"
	params := map[string]string{
		"From":       "+441234567890",
		"To":         "+449876543210",
		"Body":       "Hello test",
		"MessageSid": "SM123",
	}

	// Compute expected signature using the Twilio algorithm directly
	sig := computeSignature(authToken, url, params)

	// Should validate against itself
	Expect(ValidateSignature(authToken, url, sig, params)).To(BeTrue())

	// Should fail with wrong token
	Expect(ValidateSignature("wrong-token", url, sig, params)).To(BeFalse())

	// Should fail with wrong URL
	Expect(ValidateSignature(authToken, "https://other.com", sig, params)).To(BeFalse())

	// Should fail with modified params
	modifiedParams := map[string]string{
		"From":       "+441234567890",
		"To":         "+449876543210",
		"Body":       "Modified message",
		"MessageSid": "SM123",
	}
	Expect(ValidateSignature(authToken, url, sig, modifiedParams)).To(BeFalse())
}

func TestNormaliseE164(t *testing.T) {
	RegisterTestingT(t)

	Expect(NormaliseE164("+441234567890")).To(Equal("+441234567890"))
	Expect(NormaliseE164("441234567890")).To(Equal("+441234567890"))
	Expect(NormaliseE164("  +1234  ")).To(Equal("+1234"))
	Expect(NormaliseE164("")).To(Equal(""))
}

func TestExtractJSONField(t *testing.T) {
	RegisterTestingT(t)

	data := []byte(`{"sid": "SM123", "status": "queued"}`)
	Expect(extractJSONField(data, "sid")).To(Equal("SM123"))
	Expect(extractJSONField(data, "status")).To(Equal("queued"))
	Expect(extractJSONField(data, "missing")).To(Equal(""))

	// Without space after colon
	data2 := []byte(`{"sid":"SM456"}`)
	Expect(extractJSONField(data2, "sid")).To(Equal("SM456"))
}

// computeSignature replicates the Twilio HMAC-SHA1 algorithm for test verification.
func computeSignature(authToken, url string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(url)
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString(params[k])
	}

	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(sb.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}