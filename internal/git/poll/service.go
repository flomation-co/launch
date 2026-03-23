package poll

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"

	log "github.com/sirupsen/logrus"
)

const (
	DefaultPollInterval = 60 * time.Second
	MinPollInterval     = 10 * time.Second
)

// triggerConfig holds the parsed configuration from a git-poll trigger's Data field.
type triggerConfig struct {
	RepositoryURL string `json:"repository_url"`
	SSHKey        string `json:"ssh_key"`
	BranchRegex   string `json:"branch_regex"`
	PollInterval  string `json:"poll_interval"`
}

// branchRef represents a branch name and its HEAD commit hash from ls-remote.
type branchRef struct {
	Hash   string
	Branch string
}

type Service struct {
	config  *config.Config
	db      *persistence.Service
	trigger *trigger.Service

	// state tracks the last known commit hash per trigger per branch.
	// Key: triggerID -> branchName -> commitHash
	state map[string]map[string]string
	mu    sync.RWMutex
}

func NewService(config *config.Config, db *persistence.Service, trigger *trigger.Service) *Service {
	s := Service{
		config:  config,
		db:      db,
		trigger: trigger,
		state:   make(map[string]map[string]string),
	}

	go s.watch()

	return &s
}

func (s *Service) watch() {
	// Initial delay to let the service start up fully.
	time.Sleep(5 * time.Second)

	for {
		s.poll()
		time.Sleep(DefaultPollInterval)
	}
}

func (s *Service) poll() {
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeGitPoll)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to get git-poll triggers")
		return
	}

	for _, tr := range triggers {
		s.checkTrigger(tr)
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger) {
	var cfg triggerConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
		}).Error("unable to parse git-poll trigger config")
		return
	}

	if cfg.RepositoryURL == "" {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
		}).Warn("git-poll trigger has no repository URL")
		return
	}

	// Resolve variable references in config values
	repoURL := s.trigger.ResolveString(tr.ID, cfg.RepositoryURL)
	sshKey := s.trigger.ResolveString(tr.ID, cfg.SSHKey)

	refs, err := lsRemote(repoURL, sshKey)
	if err != nil {
		log.WithFields(log.Fields{
			"error":      err,
			"trigger_id": tr.ID,
			"repo":       repoURL,
		}).Error("unable to ls-remote repository")
		return
	}

	var branchFilter *regexp.Regexp
	if cfg.BranchRegex != "" {
		branchFilter, err = regexp.Compile(cfg.BranchRegex)
		if err != nil {
			log.WithFields(log.Fields{
				"error":      err,
				"trigger_id": tr.ID,
				"regex":      cfg.BranchRegex,
			}).Error("invalid branch regex")
			return
		}
	}

	s.mu.Lock()
	if s.state[tr.ID] == nil {
		s.state[tr.ID] = make(map[string]string)
	}
	known := s.state[tr.ID]
	s.mu.Unlock()

	for _, ref := range refs {
		if branchFilter != nil && !branchFilter.MatchString(ref.Branch) {
			continue
		}

		s.mu.RLock()
		previousHash, seen := known[ref.Branch]
		s.mu.RUnlock()

		if !seen {
			// First time seeing this branch — record state but don't trigger,
			// to avoid firing on every existing branch at startup.
			s.mu.Lock()
			s.state[tr.ID][ref.Branch] = ref.Hash
			s.mu.Unlock()
			continue
		}

		if ref.Hash != previousHash {
			log.WithFields(log.Fields{
				"trigger_id": tr.ID,
				"branch":     ref.Branch,
				"old_hash":   previousHash,
				"new_hash":   ref.Hash,
			}).Info("git change detected, firing trigger")

			s.mu.Lock()
			s.state[tr.ID][ref.Branch] = ref.Hash
			s.mu.Unlock()

			data := map[string]interface{}{
				"branch":         ref.Branch,
				"commit_hash":    ref.Hash,
				"commit_message": "",
				"repository_url": repoURL,
			}

			if err := s.trigger.Trigger(tr, data); err != nil {
				log.WithFields(log.Fields{
					"error":      err,
					"trigger_id": tr.ID,
					"branch":     ref.Branch,
				}).Error("unable to fire git-poll trigger")
			}
		}
	}
}

// repoURLPattern matches common Git repository URL formats:
// SSH:   git@host:org/repo.git, ssh://user@host/path
// HTTPS: https://host/org/repo.git
// File:  /path/to/repo (absolute paths only)
var repoURLPattern = regexp.MustCompile(`^(?:(?:https?|ssh|git)://[^\s]+|[a-zA-Z0-9._-]+@[a-zA-Z0-9._-]+:[^\s]+|/[^\s]+)$`)

// validateRepoURL checks that the repository URL is a valid git remote and
// does not contain shell metacharacters or suspicious patterns.
func validateRepoURL(url string) error {
	if url == "" {
		return errors.New("repository URL is empty")
	}

	if strings.ContainsAny(url, ";|&$`\\'\"\n\r") {
		return fmt.Errorf("repository URL contains disallowed characters: %q", url)
	}

	if !repoURLPattern.MatchString(url) {
		return fmt.Errorf("repository URL does not match expected format: %q", url)
	}

	return nil
}

// lsRemote runs `git ls-remote --heads` against the repository and returns
// the branch refs. This avoids cloning the entire repository.
func lsRemote(repoURL string, sshKey string) ([]branchRef, error) {
	if err := validateRepoURL(repoURL); err != nil {
		return nil, err
	}

	// #nosec G204 — repoURL is validated above and passed as a single argument,
	// not interpreted by a shell.
	cmd := exec.Command("git", "ls-remote", "--heads", repoURL)

	if sshKey != "" {
		// Write the SSH key to a temp file (ssh -i requires a file path)
		tmpFile, err := os.CreateTemp("", "flomation-ssh-key-*")
		if err != nil {
			return nil, fmt.Errorf("unable to create temp SSH key file: %w", err)
		}
		defer os.Remove(tmpFile.Name())

		if _, err := tmpFile.WriteString(sshKey); err != nil {
			tmpFile.Close()
			return nil, fmt.Errorf("unable to write SSH key to temp file: %w", err)
		}
		tmpFile.Close()

		if err := os.Chmod(tmpFile.Name(), 0600); err != nil {
			return nil, fmt.Errorf("unable to set SSH key file permissions: %w", err)
		}

		cmd.Env = append(cmd.Environ(),
			"GIT_SSH_COMMAND=ssh -i "+tmpFile.Name()+" -o StrictHostKeyChecking=no",
		)
	}

	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return parseRefs(string(output)), nil
}

// parseRefs parses the output of `git ls-remote --heads` into branchRef values.
// Each line has the format: <hash>\t<refname>
func parseRefs(output string) []branchRef {
	var refs []branchRef

	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}

		hash := parts[0]
		refName := parts[1]

		// Strip refs/heads/ prefix to get the branch name.
		branch := strings.TrimPrefix(refName, "refs/heads/")

		refs = append(refs, branchRef{
			Hash:   hash,
			Branch: branch,
		})
	}

	return refs
}
