package http

import (
	"testing"

	. "github.com/onsi/gomega"
)

// tableDef builds a form definition with a single table field for the tests.
func tableDef(mode, valueCol string) formDefinition {
	return formDefinition{
		Pages: []formPage{{
			Components: []formComponent{{
				Name:          "claim",
				Type:          "table",
				SelectionMode: mode,
				ValueColumn:   valueCol,
				TableColumns: []tableColumn{
					{Key: "ref", Label: "Reference", Clickable: true},
					{Key: "status", Label: "Status"},
					{Key: "amount", Label: "Amount", Type: "currency"},
				},
				TableRows: []map[string]interface{}{
					{"ref": "REF-1", "status": "Open", "amount": 240.0},
					{"ref": "REF-2", "status": "Closed", "amount": 12.5},
				},
			}},
		}},
	}
}

func TestSanitiseTableSubmissions_Single_WhitelistAndReconstruct(t *testing.T) {
	RegisterTestingT(t)
	def := tableDef("single", "ref")

	// A valid selection whose non-key columns have been TAMPERED is replaced
	// with the server's authoritative row.
	out := sanitiseTableSubmissions(map[string]interface{}{
		"claim": map[string]interface{}{"ref": "REF-1", "status": "HACKED", "amount": 9999.0},
	}, def)
	row, ok := out["claim"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(row["status"]).To(Equal("Open")) // reconstructed, not "HACKED"
	Expect(row["amount"]).To(Equal(240.0))  // reconstructed, not 9999
}

func TestSanitiseTableSubmissions_Single_UnknownKeyDropped(t *testing.T) {
	RegisterTestingT(t)
	def := tableDef("single", "ref")

	// A selection whose value_column key is not one of the authoritative rows
	// is dropped entirely (a crafted POST can't smuggle a fabricated row).
	out := sanitiseTableSubmissions(map[string]interface{}{
		"claim": map[string]interface{}{"ref": "REF-999", "status": "Open"},
	}, def)
	_, present := out["claim"]
	Expect(present).To(BeFalse())
}

func TestSanitiseTableSubmissions_NumericKeyMatches(t *testing.T) {
	RegisterTestingT(t)
	def := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{{
				Name:          "row",
				Type:          "table",
				SelectionMode: "single",
				ValueColumn:   "id",
				TableColumns:  []tableColumn{{Key: "id"}, {Key: "name"}},
				TableRows: []map[string]interface{}{
					{"id": 4471.0, "name": "Alice"},
				},
			}},
		}},
	}
	// JSON numbers decode to float64 on both sides; an integer id must match.
	out := sanitiseTableSubmissions(map[string]interface{}{
		"row": map[string]interface{}{"id": 4471.0, "name": "tampered"},
	}, def)
	row, ok := out["row"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(row["name"]).To(Equal("Alice"))
}

func TestSanitiseTableSubmissions_NoneStripsValue(t *testing.T) {
	RegisterTestingT(t)
	def := tableDef("none", "ref")

	// A display-only table collects no answer — any client value is stripped.
	out := sanitiseTableSubmissions(map[string]interface{}{
		"claim": map[string]interface{}{"ref": "REF-1"},
	}, def)
	_, present := out["claim"]
	Expect(present).To(BeFalse())
}

func TestSanitiseTableSubmissions_MissingValueColumnStrips(t *testing.T) {
	RegisterTestingT(t)
	def := tableDef("single", "") // no value column configured

	out := sanitiseTableSubmissions(map[string]interface{}{
		"claim": map[string]interface{}{"ref": "REF-1"},
	}, def)
	_, present := out["claim"]
	Expect(present).To(BeFalse())
}

func TestSanitiseTableSubmissions_WrongShapeDropped(t *testing.T) {
	RegisterTestingT(t)
	def := tableDef("single", "ref")

	// A non-object submission (e.g. a bare string) is dropped, not stored.
	out := sanitiseTableSubmissions(map[string]interface{}{
		"claim": "REF-1",
	}, def)
	_, present := out["claim"]
	Expect(present).To(BeFalse())
}

func TestSanitiseTableSubmissions_NoTablesPassthrough(t *testing.T) {
	RegisterTestingT(t)
	def := formDefinition{Pages: []formPage{{Components: []formComponent{{Name: "x", Type: "text"}}}}}
	in := map[string]interface{}{"x": "hello"}
	out := sanitiseTableSubmissions(in, def)
	Expect(out["x"]).To(Equal("hello"))
}

func TestStripReadOnly_SkipsTable(t *testing.T) {
	RegisterTestingT(t)
	def := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{{
				Name: "claim", Type: "table", ReadOnly: true, SelectionMode: "single", ValueColumn: "ref",
			}},
		}},
	}
	// A read_only table must NOT be flattened to a baked default_value — the
	// structured skip keeps the object shape intact.
	out := stripReadOnlySubmissions(map[string]interface{}{
		"claim": map[string]interface{}{"ref": "REF-1"},
	}, def)
	row, ok := out["claim"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(row["ref"]).To(Equal("REF-1"))
}
