package poll

import (
	"testing"

	. "github.com/onsi/gomega"
)

func Test_LsRemote_EmptyURL(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	_, err := lsRemote("", "")
	Expect(err).NotTo(BeNil())
}
