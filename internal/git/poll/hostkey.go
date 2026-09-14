package poll

import (
	"bytes"
	"fmt"
	"net"
	"strings"

	gossh "golang.org/x/crypto/ssh"
)

// Host key verification for the git-poll trigger.
//
// This mirrors actions/git/hostkey.go in the executor rather than importing it:
// Launch and the executor are separate modules, and the alternative — a shared
// module for sixty lines of key parsing — buys less than it costs. The two must
// stay in step, so a change to the accepted formats belongs in both.
//
// Without it, go-git verifies against the LAUNCH SERVICE's ~/.ssh/known_hosts,
// which is empty in a container. A poll against any self-hosted server then
// fails every interval with "knownhosts: key is unknown", which reads as a
// broken trigger rather than a missing key — and unlike the actions, nobody is
// watching an execution to see it.

// ParseHostKeys reads the forms a person can obtain: a known_hosts line (what
// `ssh-keyscan host` prints), a bare "keytype base64" pair, or a SHA256:...
// fingerprint. Blank lines and # comments are ignored so keyscan output pastes
// verbatim.
func ParseHostKeys(raw string) (fingerprints []string, keys []gossh.PublicKey, err error) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "SHA256:") {
			fingerprints = append(fingerprints, line)
			continue
		}

		// A bare "keytype base64" has no host column, which ParseKnownHosts
		// requires; a wildcard makes it parse. The host column is not used for
		// matching anyway — see suppliedHostKeyCallback.
		candidate := line
		if fields := strings.Fields(line); len(fields) >= 2 && isHostKeyType(fields[0]) {
			candidate = "* " + line
		}

		_, _, key, _, _, parseErr := gossh.ParseKnownHosts([]byte(candidate))
		if parseErr != nil {
			return nil, nil, fmt.Errorf("could not read %q as a host key — expected `ssh-keyscan <host>` output or a SHA256:... fingerprint", truncate(line, 60))
		}
		keys = append(keys, key)
	}

	if len(fingerprints) == 0 && len(keys) == 0 {
		return nil, nil, fmt.Errorf("no host key was found — paste the output of `ssh-keyscan <host>`, or a SHA256:... fingerprint")
	}
	return fingerprints, keys, nil
}

// hostKeyCallback builds the callback for a poll.
//
// A nil callback means "leave go-git's default alone", which is the known_hosts
// path, so the secure default survives when the trigger configures neither.
func hostKeyCallback(hostKey string, skip bool) (gossh.HostKeyCallback, error) {
	if skip {
		// #nosec G106 -- Deliberate, opt-in, and never the default. This is
		// reached only when a trigger author has ticked "Skip host key
		// verification", whose label states the risk. The alternative to
		// offering it is that a self-hosted server nobody can keyscan cannot be
		// polled at all, which pushes people towards worse workarounds.
		return gossh.InsecureIgnoreHostKey(), nil
	}
	hostKey = strings.TrimSpace(hostKey)
	if hostKey == "" || strings.HasPrefix(hostKey, "${") {
		return nil, nil
	}

	fingerprints, keys, err := ParseHostKeys(hostKey)
	if err != nil {
		return nil, err
	}
	return suppliedHostKeyCallback(fingerprints, keys), nil
}

func suppliedHostKeyCallback(fingerprints []string, keys []gossh.PublicKey) gossh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		presented := gossh.FingerprintSHA256(key)
		for _, f := range fingerprints {
			if f == presented {
				return nil
			}
		}

		marshalled := key.Marshal()
		for _, k := range keys {
			if bytes.Equal(k.Marshal(), marshalled) {
				return nil
			}
		}

		return fmt.Errorf(
			"host key mismatch for %s: the server presented %s (%s), which is not the Host Key configured on this trigger. "+
				"Either the server's key has genuinely changed — confirm the new one with whoever runs it — or this connection is being intercepted",
			hostname, presented, key.Type())
	}
}

// describeHostKeyError makes a poll failure say what to do about it. The
// unmodified error is "knownhosts: key is unknown", which names neither the
// problem nor the fix, and this one lands in a log rather than in front of
// someone who can ask.
func describeHostKeyError(err error, repositoryURL string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	host := hostFromRepositoryURL(repositoryURL)
	if host == "" {
		host = "<host>"
	}

	switch {
	case strings.Contains(message, "knownhosts: key is unknown"),
		strings.Contains(message, "knownhosts: key not found"):
		return fmt.Errorf(
			"the Git server's host key is not trusted by the Launch service, so the poll was refused before authentication. "+
				"Run `ssh-keyscan %s` and paste the output into the trigger's Host Key field, or tick Skip host key verification. (%w)",
			host, err)

	case strings.Contains(message, "knownhosts: key mismatch"):
		return fmt.Errorf(
			"the Git server at %s presented a host key that does not match the one already trusted. "+
				"Either the server's key has genuinely changed, or the connection is being intercepted — confirm the current key with whoever runs the server before changing anything. (%w)",
			host, err)
	}
	return err
}

// hostFromRepositoryURL pulls the hostname out of a git remote so the error can
// name the exact ssh-keyscan command. Handles scp-style
// (git@host:group/repo.git) and URL-style (ssh://git@host:2222/group/repo.git).
func hostFromRepositoryURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	if idx := strings.Index(raw, "://"); idx >= 0 {
		rest := raw[idx+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		if slash := strings.IndexAny(rest, "/"); slash >= 0 {
			rest = rest[:slash]
		}
		// A port is not part of what ssh-keyscan wants, and including it would
		// make the suggested command wrong.
		if host, _, err := net.SplitHostPort(rest); err == nil {
			return host
		}
		return rest
	}

	if at := strings.LastIndex(raw, "@"); at >= 0 {
		raw = raw[at+1:]
	}
	if colon := strings.Index(raw, ":"); colon >= 0 {
		raw = raw[:colon]
	}
	if strings.ContainsAny(raw, "/ ") {
		return ""
	}
	return raw
}

func isHostKeyType(field string) bool {
	switch {
	case field == "ssh-rsa", field == "ssh-dss", field == "ssh-ed25519":
		return true
	case strings.HasPrefix(field, "ecdsa-sha2-"):
		return true
	case strings.HasPrefix(field, "sk-"): // FIDO-backed keys
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
