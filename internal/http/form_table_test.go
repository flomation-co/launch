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

func TestSanitiseTableSubmissions_Multiple_WhitelistDedupeReconstruct(t *testing.T) {
	RegisterTestingT(t)
	def := tableDef("multiple", "ref")

	out := sanitiseTableSubmissions(map[string]interface{}{
		"claim": []interface{}{
			map[string]interface{}{"ref": "REF-2", "status": "HACKED"},
			map[string]interface{}{"ref": "REF-999"}, // unknown → dropped
			map[string]interface{}{"ref": "REF-1"},
			map[string]interface{}{"ref": "REF-2"}, // duplicate → dropped
			"not-an-object",                        // wrong shape → dropped
		},
	}, def)
	arr, ok := out["claim"].([]interface{})
	Expect(ok).To(BeTrue())
	Expect(arr).To(HaveLen(2))
	first := arr[0].(map[string]interface{})
	second := arr[1].(map[string]interface{})
	Expect(first["ref"]).To(Equal("REF-2"))
	Expect(first["status"]).To(Equal("Closed")) // reconstructed, not "HACKED"
	Expect(second["ref"]).To(Equal("REF-1"))
}

func TestSanitiseTableSubmissions_Multiple_EmptyStrips(t *testing.T) {
	RegisterTestingT(t)
	def := tableDef("multiple", "ref")
	out := sanitiseTableSubmissions(map[string]interface{}{"claim": []interface{}{}}, def)
	_, present := out["claim"]
	Expect(present).To(BeFalse())
}

func TestBakeDynamicOptions_BakesComputedTableRows(t *testing.T) {
	RegisterTestingT(t)
	def := formDefinition{
		Pages: []formPage{{Components: []formComponent{{
			Name:          "claim",
			Type:          "table",
			SelectionMode: "single",
			ValueColumn:   "ref",
			RowsSource:    "claims",
			TableColumns:  []tableColumn{{Key: "ref"}, {Key: "status"}},
		}}}},
	}
	// Data-source output: an array of objects.
	outputs := map[string]interface{}{
		"claims": []interface{}{
			map[string]interface{}{"ref": "REF-1", "status": "Open"},
			map[string]interface{}{"ref": "REF-2", "status": "Closed"},
		},
	}
	baked := bakeDynamicOptions(def, outputs)
	rows := baked.Pages[0].Components[0].TableRows
	Expect(rows).To(HaveLen(2))
	Expect(rows[0]["status"]).To(Equal("Open"))

	// A selection is then validated against the BAKED rows.
	san := sanitiseTableSubmissions(map[string]interface{}{
		"claim": map[string]interface{}{"ref": "REF-2", "status": "tampered"},
	}, baked)
	row := san["claim"].(map[string]interface{})
	Expect(row["status"]).To(Equal("Closed"))
}

func TestRowsFromOutput_ArrayOfArrays_BindsByColumn(t *testing.T) {
	RegisterTestingT(t)
	cols := []tableColumn{{Key: "id"}, {Key: "name"}}
	rows := rowsFromOutput([]interface{}{
		[]interface{}{"1", "Alice"},
		[]interface{}{"2", "Bob"},
	}, cols)
	Expect(rows).To(HaveLen(2))
	Expect(rows[0]["id"]).To(Equal("1"))
	Expect(rows[1]["name"]).To(Equal("Bob"))
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

func TestStripHidden_TableSelectionDrivesVisibility(t *testing.T) {
	RegisterTestingT(t)
	// A follow-up field is visible only when the table's selected row has
	// value_column "ref" == "REF-1".
	def := formDefinition{
		Pages: []formPage{{Components: []formComponent{
			{
				Name: "claim", Type: "table", SelectionMode: "single", ValueColumn: "ref",
				TableColumns: []tableColumn{{Key: "ref"}, {Key: "status"}},
				TableRows: []map[string]interface{}{
					{"ref": "REF-1", "status": "Open"}, {"ref": "REF-2", "status": "Closed"},
				},
			},
			{Name: "followup", Type: "text", VisibleIf: &visibilityRule{
				Match: "all", Rules: []visibilityClause{{Field: "claim", Op: "equals", Value: "REF-1"}},
			}},
		}}},
	}

	// Selected REF-1 → followup visible → its answer survives.
	out := stripHiddenSubmissions(map[string]interface{}{
		"claim":    map[string]interface{}{"ref": "REF-1", "status": "Open"},
		"followup": "kept",
	}, def)
	Expect(out["followup"]).To(Equal("kept"))

	// Selected REF-2 → followup hidden → its answer stripped.
	out = stripHiddenSubmissions(map[string]interface{}{
		"claim":    map[string]interface{}{"ref": "REF-2", "status": "Closed"},
		"followup": "smuggled",
	}, def)
	_, present := out["followup"]
	Expect(present).To(BeFalse())
}

func TestStripHidden_MultiSelectTableContains(t *testing.T) {
	RegisterTestingT(t)
	def := formDefinition{
		Pages: []formPage{{Components: []formComponent{
			{
				Name: "claims", Type: "table", SelectionMode: "multiple", ValueColumn: "ref",
				TableColumns: []tableColumn{{Key: "ref"}},
				TableRows:    []map[string]interface{}{{"ref": "A"}, {"ref": "B"}, {"ref": "C"}},
			},
			{Name: "extra", Type: "text", VisibleIf: &visibilityRule{
				Match: "all", Rules: []visibilityClause{{Field: "claims", Op: "contains", Value: "B"}},
			}},
		}}},
	}
	// Multi-select including B → visible.
	out := stripHiddenSubmissions(map[string]interface{}{
		"claims": []interface{}{map[string]interface{}{"ref": "A"}, map[string]interface{}{"ref": "B"}},
		"extra":  "kept",
	}, def)
	Expect(out["extra"]).To(Equal("kept"))
	// Without B → hidden.
	out = stripHiddenSubmissions(map[string]interface{}{
		"claims": []interface{}{map[string]interface{}{"ref": "A"}},
		"extra":  "gone",
	}, def)
	_, present := out["extra"]
	Expect(present).To(BeFalse())
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
