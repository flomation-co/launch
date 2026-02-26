package google

import (
	"context"
	"fmt"
	"os"

	"flomation.app/automate/launch/internal/config"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	g "golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v2"
	"google.golang.org/api/option"
)

type TokenResponse struct {
	State string
	Code  string
}

type Service struct {
	config               *config.Config
	client               *drive.Service
	pendingAuthorization chan TokenResponse
}

func NewService(config *config.Config) (*Service, error) {
	s := Service{
		config:               config,
		pendingAuthorization: make(chan TokenResponse),
	}

	if config.Google.CredentialsFile != nil {
		b, err := os.ReadFile(*config.Google.CredentialsFile)
		if err != nil {
			return nil, err
		}

		cfg, err := g.ConfigFromJSON(b)
		if err != nil {
			return nil, err
		}
		cfg.Scopes = []string{
			"https://www.googleapis.com/auth/drive",
		}

		url := cfg.AuthCodeURL(uuid.NewString(), oauth2.AccessTypeOffline, oauth2.ApprovalForce)
		fmt.Printf("Use the following URL to authorise the application: %v\n", url)

		go func() {
			for {
				code := <-s.pendingAuthorization

				log.WithFields(log.Fields{
					"code": code,
				}).Info("received authorisation code")

				token, err := cfg.Exchange(context.Background(), code.Code)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("unable to exchange token")
					return
				}

				log.WithFields(log.Fields{
					"refresh_token": token.RefreshToken,
					"expiry":        token.Expiry,
				}).Info("refresh token")

				tokenSource := cfg.TokenSource(context.Background(), &oauth2.Token{
					RefreshToken: token.RefreshToken,
				})

				refreshedToken, err := tokenSource.Token()
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("unable to use refresh token")
				}

				log.WithFields(log.Fields{
					"refreshed_token": refreshedToken.AccessToken,
				}).Info("refresh")

				client := cfg.Client(context.Background(), refreshedToken)

				driveService, err := drive.NewService(context.Background(), option.WithHTTPClient(client))
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("unable to create new google drive service")
					return
				}

				s.client = driveService

				files, err := s.client.Files.List().Do()
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("unable to get google drive files")
					return
				}

				for _, f := range files.Items {
					log.WithFields(log.Fields{
						"title": f.Title,
					}).Info("file")
				}
			}
		}()
	} else {
		driveService, err := drive.NewService(context.Background())
		if err != nil {
			return nil, err
		}

		s.client = driveService
	}

	return &s, nil
}

func (s *Service) ReceiveAuthCode(response TokenResponse) {
	s.pendingAuthorization <- response
}
