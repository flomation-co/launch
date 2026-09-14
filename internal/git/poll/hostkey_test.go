package poll

import (
	"fmt"
	"net"
	"testing"

	. "github.com/onsi/gomega"
	gossh "golang.org/x/crypto/ssh"
)

const (
	testKeyType = "ssh-ed25519"
	testKeyB64  = "AAAAC3NzaC1lZDI1NTE5AAAAIKz8T8Y0DLHMwRPPQJ5fm2GRPwGgJmDDxJvR8kZ8KqZx"
	otherKeyB64 = "AAAAC3NzaC1lZDI1NTE5AAAAIH8pVJ9wJ0m5cN1CkGbLVq5xGqiLHrz3mYb3VQZvLxJt"
)

func mustKey(t *testing.T, b64 string) gossh.PublicKey {
	t.Helper()
	_, _, key, _, _, err := gossh.ParseKnownHosts([]byte("host " + testKeyType + " " + b64))
	if err != nil {
		t.Fatalf("test key is not parseable: %v", err)
	}
	return key
}

// ssh-keyscan output has to paste in verbatim — comments, blank lines and
// several key types included.
func TestParseHostKeys_AcceptsKeyscanOutput(t *testing.T) {
	RegisterTestingT(t)

	raw := "# gitlab.example.com:22 SSH-2.0-OpenSSH_8.9\n" +
		"gitlab.example.com " + testKeyType + " " + testKeyB64 + "\n\n" +
		"gitlab.example.com " + testKeyType + " " + otherKeyB64 + "\n"

	fingerprints, keys, err := ParseHostKeys(raw)
	Expect(err).To(BeNil())
	Expect(fingerprints).To(BeEmpty())
	Expect(keys).To(HaveLen(2))
}

func TestParseHostKeys_AcceptsFingerprintAndBareKey(t *testing.T) {
	RegisterTestingT(t)

	expected := gossh.FingerprintSHA256(mustKey(t, testKeyB64))
	fingerprints, _, err := ParseHostKeys(expected)
	Expect(err).To(BeNil())
	Expect(fingerprints).To(Equal([]string{expected}))

	_, keys, err := ParseHostKeys(testKeyType + " " + testKeyB64)
	Expect(err).To(BeNil())
	Expect(keys).To(HaveLen(1))
}

// The secure default has to survive: nil is what leaves go-git checking
// known_hosts, so anything non-nil here would silently replace verification.
func TestHostKeyCallback_DefaultsToKnownHosts(t *testing.T) {
	RegisterTestingT(t)

	callback, err := hostKeyCallback("", false)
	Expect(err).To(BeNil())
	Expect(callback).To(BeNil())

	// An unresolved reference is not a host key.
	callback, err = hostKeyCallback("${secrets.Missing}", false)
	Expect(err).To(BeNil())
	Expect(callback).To(BeNil())
}

func TestHostKeyCallback_SuppliedAndSkipped(t *testing.T) {
	RegisterTestingT(t)

	callback, err := hostKeyCallback("gitlab.example.com "+testKeyType+" "+testKeyB64, false)
	Expect(err).To(BeNil())
	Expect(callback("gitlab.example.com:22", &net.TCPAddr{}, mustKey(t, testKeyB64))).To(BeNil())
	Expect(callback("gitlab.example.com:22", &net.TCPAddr{}, mustKey(t, otherKeyB64))).ToNot(BeNil())

	// Skip wins over a supplied key: the trigger author has said plainly what
	// they want, and a conflict should not stop the poll.
	callback, err = hostKeyCallback("gitlab.example.com "+testKeyType+" "+testKeyB64, true)
	Expect(err).To(BeNil())
	Expect(callback("anything", &net.TCPAddr{}, mustKey(t, otherKeyB64))).To(BeNil())
}

// This error repeats every poll interval into a log, so it has to carry the fix.
func TestDescribeHostKeyError(t *testing.T) {
	RegisterTestingT(t)

	err := describeHostKeyError(fmt.Errorf("ssh: handshake failed: knownhosts: key is unknown"), "git@gitlab.example.com:group/repo.git")
	Expect(err.Error()).To(ContainSubstring("ssh-keyscan gitlab.example.com"))
	Expect(err.Error()).To(ContainSubstring("knownhosts: key is unknown"))

	// A mismatch must not suggest keyscanning the new key, which would walk
	// someone straight through a real interception.
	err = describeHostKeyError(fmt.Errorf("ssh: handshake failed: knownhosts: key mismatch"), "git@gitlab.example.com:group/repo.git")
	Expect(err.Error()).To(ContainSubstring("intercepted"))
	Expect(err.Error()).ToNot(ContainSubstring("ssh-keyscan"))

	// Everything else passes through untouched.
	original := fmt.Errorf("repository not found")
	Expect(describeHostKeyError(original, "git@host:repo.git")).To(Equal(original))
}

func TestHostFromRepositoryURL(t *testing.T) {
	RegisterTestingT(t)

	Expect(hostFromRepositoryURL("git@gitlab.example.com:group/repo.git")).To(Equal("gitlab.example.com"))
	Expect(hostFromRepositoryURL("ssh://git@gitlab.example.com:2222/group/repo.git")).To(Equal("gitlab.example.com"))
	Expect(hostFromRepositoryURL("")).To(Equal(""))
}
