package poll

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	gitssh "github.com/go-git/go-git/v6/plumbing/transport/ssh"
	"github.com/go-git/go-git/v6/storage/memory"
	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
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
		}).Debug("git-poll trigger has no repository URL — skipping")
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

// lsRemote uses go-git to list remote branch refs without cloning.
func lsRemote(repoURL string, sshKey string) ([]branchRef, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("repository URL is empty")
	}

	remote := git.NewRemote(memory.NewStorage(), &gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})

	listOpts := &git.ListOptions{}

	if sshKey != "" {
		auth, err := gitssh.NewPublicKeys("git", []byte(sshKey), "")
		if err != nil {
			return nil, fmt.Errorf("unable to create SSH auth: %w", err)
		}
		listOpts.Auth = auth
	}

	refs, err := remote.List(listOpts)
	if err != nil {
		return nil, err
	}

	var results []branchRef
	for _, ref := range refs {
		refName := ref.Name().String()
		if !strings.HasPrefix(refName, "refs/heads/") {
			continue
		}

		results = append(results, branchRef{
			Hash:   ref.Hash().String(),
			Branch: strings.TrimPrefix(refName, "refs/heads/"),
		})
	}

	return results, nil
}
