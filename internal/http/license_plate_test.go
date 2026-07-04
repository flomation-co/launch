package http

import "testing"

// findUploadComponent must accept license_plate (its captured frame uploads
// through the same blob proxy as camera/esignature/file_upload) and honour
// its per-field mime/size constraints.
func TestFindUploadComponent_LicensePlate(t *testing.T) {
	maxSize := int64(5 * 1024 * 1024)
	def := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "notes", Type: "text"},
				{Name: "plate", Type: "license_plate", AcceptMime: "image/*", MaxSizeBytes: maxSize},
			},
		}},
	}

	if c := findUploadComponent(def, "plate"); c == nil {
		t.Fatal("expected license_plate to be an upload component")
	} else if c.AcceptMime != "image/*" || c.MaxSizeBytes != maxSize {
		t.Errorf("upload constraints not preserved: %+v", c)
	}

	// A non-upload field must not resolve as an upload component.
	if c := findUploadComponent(def, "notes"); c != nil {
		t.Errorf("text field must not be an upload component, got %+v", c)
	}
}
