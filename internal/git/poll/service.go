package poll

import (
	"flomation.app/automate/launch/internal/config"
	"time"
)

const (
	DefaultPollInterval = time.Second
)

type Service struct {
	config *config.Config
}

func NewService(config *config.Config) *Service {
	s := Service{
		config: config,
	}

	go s.watch()

	return &s
}

func (s *Service) watch() {
	for {

		time.Sleep(DefaultPollInterval)
	}
}
