package http

import (
	"testing"

	"flomation.app/automate/launch/internal/assets"
)

// The recognition runtime + plate models must be embedded in the binary so the
// public form can load them same-origin (they are served by the NoRoute static
// handler). Guards against the vendored weights being dropped from the tree.
func TestRecognitionAssetsEmbedded(t *testing.T) {
	want := map[string]int{
		"files/recognition/ort/ort.wasm.min.js":             10_000,
		"files/recognition/ort/ort-wasm-simd-threaded.mjs":  5_000,
		"files/recognition/ort/ort-wasm-simd-threaded.wasm": 1_000_000,
		"files/recognition/plate/detector.onnx":             1_000_000,
		"files/recognition/plate/ocr.onnx":                  1_000_000,
	}
	for path, minBytes := range want {
		b, err := assets.Templates.ReadFile(path)
		if err != nil {
			t.Errorf("%s not embedded: %v", path, err)
			continue
		}
		if len(b) < minBytes {
			t.Errorf("%s embedded but too small: %d bytes (want >= %d)", path, len(b), minBytes)
		}
	}
}

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
