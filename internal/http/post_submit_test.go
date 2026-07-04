package http

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
)

// The post-submission config must survive the server's parse → resolve →
// re-marshal round-trip (it's embedded into the form page as JSON); an unknown
// field would be silently dropped.
func TestFormSubmit_RoundTripsThroughJSON(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{
		Title: "T",
		Pages: []formPage{{Components: []formComponent{{Name: "a", Type: "text"}}}},
		Submit: &formSubmit{
			SuccessMessage:       "Cheers ${user.first_name}",
			OnSubmit:             "redirect",
			RedirectURL:          "https://example.com/thanks",
			RedirectDelaySeconds: 3,
		},
	}

	b, err := json.Marshal(def)
	Expect(err).NotTo(HaveOccurred())
	var back formDefinition
	Expect(json.Unmarshal(b, &back)).To(Succeed())
	Expect(back.Submit).NotTo(BeNil())
	Expect(back.Submit.OnSubmit).To(Equal("redirect"))
	Expect(back.Submit.RedirectURL).To(Equal("https://example.com/thanks"))
	Expect(back.Submit.RedirectDelaySeconds).To(Equal(3))
	Expect(back.Submit.SuccessMessage).To(Equal("Cheers ${user.first_name}"))
}

// resolveFormForRender interpolates the thank-you message like any label, and
// must not mutate the (possibly cached) source definition.
func TestResolveFormForRender_InterpolatesSuccessMessage(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{
		Submit: &formSubmit{SuccessMessage: "Thanks ${user.first_name}!", OnSubmit: "message"},
	}
	ctx := substitutionContext{UserVariables: map[string]string{"first_name": "Ada"}}

	resolved := resolveFormForRender(def, ctx)
	Expect(resolved.Submit.SuccessMessage).To(Equal("Thanks Ada!"))
	Expect(resolved.Submit.OnSubmit).To(Equal("message"))
	// Source definition unchanged — no aliasing of the cached form.
	Expect(def.Submit.SuccessMessage).To(Equal("Thanks ${user.first_name}!"))
}
