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

func Test_ValidateRepoURL_ValidURLs(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	validURLs := []string{
		"https://github.com/org/repo.git",
		"git@github.com:org/repo.git",
		"ssh://git@gitlab.example.com/org/repo.git",
		"https://gitlab.tooling.flomation.app/flomation/automate/executor.git",
		"/home/user/repos/my-project",
	}

	for _, url := range validURLs {
		Expect(validateRepoURL(url)).To(BeNil(), "expected valid: %s", url)
	}
}

func Test_ValidateRepoURL_InvalidURLs(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	invalidURLs := []string{
		"",
		"https://example.com/repo; rm -rf /",
		"https://example.com/repo | cat /etc/passwd",
		"repo`whoami`",
		"https://example.com/repo\nmalicious",
	}

	for _, url := range invalidURLs {
		Expect(validateRepoURL(url)).NotTo(BeNil(), "expected invalid: %s", url)
	}
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
