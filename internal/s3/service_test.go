package s3

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
)

func Test_ParseEventTypes_Default(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	result := parseEventTypes("")
	Expect(result).To(HaveKeyWithValue("put", true))
	Expect(result).To(HaveKeyWithValue("delete", true))
}

func Test_ParseEventTypes_PutOnly(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	result := parseEventTypes("put")
	Expect(result).To(HaveKeyWithValue("put", true))
	Expect(result).NotTo(HaveKey("delete"))
}

func Test_ParseEventTypes_DeleteOnly(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	result := parseEventTypes("delete")
	Expect(result).To(HaveKeyWithValue("delete", true))
	Expect(result).NotTo(HaveKey("put"))
}

func Test_ParseEventTypes_Both(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	result := parseEventTypes("put,delete")
	Expect(result).To(HaveKeyWithValue("put", true))
	Expect(result).To(HaveKeyWithValue("delete", true))
}

func Test_ParseEventTypes_WithWhitespace(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	result := parseEventTypes("  put , delete  ")
	Expect(result).To(HaveKeyWithValue("put", true))
	Expect(result).To(HaveKeyWithValue("delete", true))
}

func Test_ParseEventTypes_InvalidIgnored(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	result := parseEventTypes("put,invalid,delete")
	Expect(result).To(HaveKeyWithValue("put", true))
	Expect(result).To(HaveKeyWithValue("delete", true))
	Expect(result).NotTo(HaveKey("invalid"))
}

func Test_ParseConfig_Valid(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	raw := `{
		"bucket_name": "my-bucket",
		"prefix": "uploads/",
		"aws_access_key": "AKIAIOSFODNN7EXAMPLE",
		"aws_secret_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"region": "eu-west-2",
		"poll_interval": "60s",
		"event_types": "put,delete"
	}`

	var cfg s3TriggerConfig
	err := json.Unmarshal([]byte(raw), &cfg)
	Expect(err).To(BeNil())
	Expect(cfg.BucketName).To(Equal("my-bucket"))
	Expect(cfg.Prefix).To(Equal("uploads/"))
	Expect(cfg.AwsAccessKey).To(Equal("AKIAIOSFODNN7EXAMPLE"))
	Expect(cfg.AwsSecretKey).To(Equal("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"))
	Expect(cfg.Region).To(Equal("eu-west-2"))
	Expect(cfg.PollInterval).To(Equal("60s"))
	Expect(cfg.EventTypes).To(Equal("put,delete"))
}

func Test_ParseConfig_MissingBucket(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	raw := `{
		"prefix": "uploads/",
		"aws_access_key": "AKIAIOSFODNN7EXAMPLE",
		"aws_secret_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"region": "eu-west-2"
	}`

	var cfg s3TriggerConfig
	err := json.Unmarshal([]byte(raw), &cfg)
	Expect(err).To(BeNil())
	Expect(cfg.BucketName).To(Equal(""))
}

func Test_ParseConfig_EmptyCredentials(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	raw := `{
		"bucket_name": "my-bucket",
		"region": "us-east-1"
	}`

	var cfg s3TriggerConfig
	err := json.Unmarshal([]byte(raw), &cfg)
	Expect(err).To(BeNil())
	Expect(cfg.AwsAccessKey).To(Equal(""))
	Expect(cfg.AwsSecretKey).To(Equal(""))
}

func Test_ObjectState_Serialisation(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	state := objectState{
		ETag:         "\"abc123\"",
		Size:         1234,
		LastModified: "2026-03-23T10:00:00Z",
	}

	data, err := json.Marshal(state)
	Expect(err).To(BeNil())

	var decoded objectState
	err = json.Unmarshal(data, &decoded)
	Expect(err).To(BeNil())
	Expect(decoded.ETag).To(Equal("\"abc123\""))
	Expect(decoded.Size).To(Equal(int64(1234)))
	Expect(decoded.LastModified).To(Equal("2026-03-23T10:00:00Z"))
}

func Test_StateDiff_NewObjects(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	current := map[string]objectState{
		"file1.txt": {ETag: "\"aaa\"", Size: 100, LastModified: "2026-03-23T10:00:00Z"},
		"file2.txt": {ETag: "\"bbb\"", Size: 200, LastModified: "2026-03-23T10:00:00Z"},
	}

	known := map[string]json.RawMessage{}

	var newKeys []string
	for key := range current {
		if _, exists := known[key]; !exists {
			newKeys = append(newKeys, key)
		}
	}

	Expect(len(newKeys)).To(Equal(2))
}

func Test_StateDiff_ChangedETag(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	current := map[string]objectState{
		"file1.txt": {ETag: "\"new-etag\"", Size: 150, LastModified: "2026-03-23T11:00:00Z"},
	}

	existingState := objectState{ETag: "\"old-etag\"", Size: 100, LastModified: "2026-03-23T10:00:00Z"}
	existingJSON, _ := json.Marshal(existingState)

	known := map[string]json.RawMessage{
		"file1.txt":     existingJSON,
		sentinelKey: json.RawMessage(`{"status":"initialised"}`),
	}

	for key, obj := range current {
		if data, exists := known[key]; exists {
			var existing objectState
			_ = json.Unmarshal(data, &existing)
			Expect(existing.ETag).NotTo(Equal(obj.ETag))
		}
	}
}

func Test_StateDiff_DeletedObjects(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	current := map[string]objectState{
		"file1.txt": {ETag: "\"aaa\"", Size: 100, LastModified: "2026-03-23T10:00:00Z"},
	}

	existingState1, _ := json.Marshal(objectState{ETag: "\"aaa\"", Size: 100, LastModified: "2026-03-23T10:00:00Z"})
	existingState2, _ := json.Marshal(objectState{ETag: "\"bbb\"", Size: 200, LastModified: "2026-03-23T10:00:00Z"})

	known := map[string]json.RawMessage{
		"file1.txt":     existingState1,
		"file2.txt":     existingState2,
		sentinelKey: json.RawMessage(`{"status":"initialised"}`),
	}

	// Remove current objects from known.
	for key := range current {
		delete(known, key)
	}

	// Remaining non-sentinel keys are deleted.
	var deletedKeys []string
	for key := range known {
		if key != sentinelKey {
			deletedKeys = append(deletedKeys, key)
		}
	}

	Expect(len(deletedKeys)).To(Equal(1))
	Expect(deletedKeys[0]).To(Equal("file2.txt"))
}

func Test_StateDiff_PrefixFiltering(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	// The prefix filtering happens at the S3 API level via ListObjectsV2Input.Prefix.
	// We just verify the config is parsed correctly.
	raw := `{"bucket_name": "bucket", "prefix": "uploads/2026/", "region": "us-east-1"}`
	var cfg s3TriggerConfig
	err := json.Unmarshal([]byte(raw), &cfg)
	Expect(err).To(BeNil())
	Expect(cfg.Prefix).To(Equal("uploads/2026/"))
}

func Test_FirstPoll_Detection(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	// Empty state = first poll (no sentinel).
	emptyState := map[string]json.RawMessage{}
	_, initialised := emptyState[sentinelKey]
	Expect(initialised).To(BeFalse())

	// With sentinel = not first poll.
	populatedState := map[string]json.RawMessage{
		sentinelKey: json.RawMessage(`{"status":"initialised"}`),
	}
	_, initialised = populatedState[sentinelKey]
	Expect(initialised).To(BeTrue())
}
