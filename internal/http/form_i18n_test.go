package http

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
)

func TestFormLanguages_Defaults(t *testing.T) {
	RegisterTestingT(t)

	langs, def := formLanguages(formDefinition{})
	Expect(def).To(Equal("en"))
	Expect(langs).To(Equal([]string{"en"}))

	langs, def = formLanguages(formDefinition{DefaultLanguage: "cy", Languages: []string{"cy", "en"}})
	Expect(def).To(Equal("cy"))
	Expect(langs).To(Equal([]string{"cy", "en"}))

	// A default language but no explicit list falls back to just the default.
	langs, def = formLanguages(formDefinition{DefaultLanguage: "fr"})
	Expect(def).To(Equal("fr"))
	Expect(langs).To(Equal([]string{"fr"}))
}

func TestResolveLanguage(t *testing.T) {
	RegisterTestingT(t)

	langs := []string{"en", "cy", "fr"}
	Expect(resolveLanguage("", langs, "en")).To(Equal("en"))
	Expect(resolveLanguage("CY", langs, "en")).To(Equal("cy"))
	Expect(resolveLanguage("cy-GB", langs, "en")).To(Equal("cy"))
	Expect(resolveLanguage("fr", []string{"en", "fr-FR"}, "en")).To(Equal("fr-FR"))
	Expect(resolveLanguage("de", langs, "en")).To(Equal("en"))
}

func TestResolveLanguageFromHeader(t *testing.T) {
	RegisterTestingT(t)

	langs := []string{"en", "cy", "fr"}
	// Explicit ?lang wins.
	Expect(resolveLanguageFromHeader("cy", "fr,en;q=0.8", langs, "en")).To(Equal("cy"))
	// ?lang naming the default is honoured, not overridden by the header.
	Expect(resolveLanguageFromHeader("en-GB", "cy", langs, "en")).To(Equal("en"))
	// No ?lang → Accept-Language, in order, first match wins.
	Expect(resolveLanguageFromHeader("", "de-DE,fr-FR;q=0.9,cy;q=0.8", langs, "en")).To(Equal("fr"))
	// Nothing maps → default.
	Expect(resolveLanguageFromHeader("", "de,es", langs, "en")).To(Equal("en"))
	Expect(resolveLanguageFromHeader("", "", langs, "en")).To(Equal("en"))
}

func TestTI18n(t *testing.T) {
	RegisterTestingT(t)

	m := i18nMap{"cy": "Helo"}
	Expect(tI18n("Hello", m, "en", "en")).To(Equal("Hello")) // default language → base
	Expect(tI18n("Hello", m, "cy", "en")).To(Equal("Helo"))  // translated
	Expect(tI18n("Hello", i18nMap{"cy": ""}, "cy", "en")).To(Equal("Hello")) // empty → base
	Expect(tI18n("Hello", nil, "fr", "en")).To(Equal("Hello"))               // missing → base
}

// TestProjectDefinition_ExposesI18n asserts the embed projection surfaces the
// multi-lingual metadata and per-field/option translations so the SDK can render
// in any authored language. Canonical option values are untouched.
func TestProjectDefinition_ExposesI18n(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{
		Title:           "Register",
		Description:     "Tell us about you.",
		DefaultLanguage: "en",
		Languages:       []string{"en", "cy"},
		DisplayMode:     "switch",
		TitleI18n:       i18nMap{"cy": "Cofrestru"},
		DescriptionI18n: i18nMap{"cy": "Dywedwch wrthym."},
		Submit:          &formSubmit{SuccessMessage: "Thanks", SuccessMessageI18n: i18nMap{"cy": "Diolch"}},
		Pages: []formPage{{Components: []formComponent{
			{
				Name: "colour", Label: "Favourite colour", Type: "radio",
				LabelI18n: i18nMap{"cy": "Hoff liw"},
				Options: []formOption{
					{Label: "Red", Value: "red", LabelI18n: i18nMap{"cy": "Coch"}},
				},
			},
		}}},
	}

	proj := projectDefinition(def)
	raw, _ := json.Marshal(proj)

	Expect(proj["default_language"]).To(Equal("en"))
	Expect(proj["display_mode"]).To(Equal("switch"))
	Expect(proj["languages"]).To(Equal([]string{"en", "cy"}))

	var payload struct {
		TitleI18n       map[string]string `json:"title_i18n"`
		DescriptionI18n map[string]string `json:"description_i18n"`
		Submit          struct {
			SuccessMessageI18n map[string]string `json:"success_message_i18n"`
		} `json:"submit"`
		Pages []struct {
			Components []struct {
				LabelI18n map[string]string `json:"label_i18n"`
				Options   []struct {
					Value     string            `json:"value"`
					LabelI18n map[string]string `json:"label_i18n"`
				} `json:"options"`
			} `json:"components"`
		} `json:"pages"`
	}
	Expect(json.Unmarshal(raw, &payload)).To(Succeed())
	Expect(payload.TitleI18n["cy"]).To(Equal("Cofrestru"))
	Expect(payload.DescriptionI18n["cy"]).To(Equal("Dywedwch wrthym."))
	Expect(payload.Submit.SuccessMessageI18n["cy"]).To(Equal("Diolch"))

	comp := payload.Pages[0].Components[0]
	Expect(comp.LabelI18n["cy"]).To(Equal("Hoff liw"))
	Expect(comp.Options[0].Value).To(Equal("red"))     // canonical value untranslated
	Expect(comp.Options[0].LabelI18n["cy"]).To(Equal("Coch"))
}
