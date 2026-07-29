package http

import (
	"testing"

	. "github.com/onsi/gomega"
)

// A field-level "Computed by a flow" (value_source) on an OPTION field populates
// its OPTION LIST from the flow output — the field-level equivalent of the
// form-level options_source. These lock in bakeComputedOptions + the strip fix.

func TestBakeComputedOptions_PopulatesFromFlow(t *testing.T) {
	RegisterTestingT(t)
	def := formDefinition{
		Pages: []formPage{{Components: []formComponent{{
			Name:        "version",
			Type:        "dropdown",
			ValueSource: "versions-flow-id",
			ValueOutput: "versions",
		}}}},
	}
	// The per-field flow returns an array of strings under its "versions" output.
	resolve := func(flowID string) map[string]interface{} {
		Expect(flowID).To(Equal("versions-flow-id"))
		return map[string]interface{}{"versions": []interface{}{"0.0.1", "0.0.2", "0.0.3"}}
	}
	baked := bakeComputedOptions(def, resolve)
	opts := baked.Pages[0].Components[0].Options
	Expect(opts).To(HaveLen(3))
	Expect(opts[0].Value).To(Equal("0.0.1"))
	Expect(opts[0].Label).To(Equal("0.0.1"))
	Expect(opts[2].Value).To(Equal("0.0.3"))
}

func TestStripComputed_KeepsOptionSelection(t *testing.T) {
	RegisterTestingT(t)
	// A value_source option field's answer (the chosen option) must survive the
	// computed strip — only its OPTIONS come from the flow, not its value.
	def := formDefinition{
		Pages: []formPage{{Components: []formComponent{{
			Name: "version", Type: "dropdown", ValueSource: "versions-flow-id", ValueOutput: "versions",
		}}}},
	}
	out := stripComputedSubmissions(map[string]interface{}{"version": "0.0.2"}, def)
	Expect(out["version"]).To(Equal("0.0.2"))
}
