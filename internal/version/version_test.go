package version

import (
	"testing"

	. "github.com/onsi/gomega"
)

func Test_GetHash(t *testing.T) {
	RegisterTestingT(t)

	Hash = "abcdef1234567890"

	hash := GetHash()
	Expect(hash).To(Equal(hash[:8]))
}
