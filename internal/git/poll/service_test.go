package poll

import (
	"testing"

	. "github.com/onsi/gomega"
)

func Test_ParseRefs_ValidOutput(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	output := `abc123def456abc123def456abc123def456abc123	refs/heads/main
def456abc123def456abc123def456abc123def456	refs/heads/feature/new-thing
789012345678901234567890123456789012345678	refs/heads/develop`

	refs := parseRefs(output)

	Expect(len(refs)).To(Equal(3))
	Expect(refs[0].Branch).To(Equal("main"))
	Expect(refs[0].Hash).To(Equal("abc123def456abc123def456abc123def456abc123"))
	Expect(refs[1].Branch).To(Equal("feature/new-thing"))
	Expect(refs[1].Hash).To(Equal("def456abc123def456abc123def456abc123def456"))
	Expect(refs[2].Branch).To(Equal("develop"))
}

func Test_ParseRefs_EmptyOutput(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	refs := parseRefs("")
	Expect(refs).To(BeNil())
}

func Test_ParseRefs_MalformedLines(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	output := `abc123def456abc123def456abc123def456abc123	refs/heads/main
not-a-valid-line
def456abc123def456abc123def456abc123def456	refs/heads/develop`

	refs := parseRefs(output)

	Expect(len(refs)).To(Equal(2))
	Expect(refs[0].Branch).To(Equal("main"))
	Expect(refs[1].Branch).To(Equal("develop"))
}
