package persistence

import (
	"embed"
	"errors"
	"fmt"

	"flomation.app/automate/launch/internal/config"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migration
var migrations embed.FS

func CheckAndUpdate(config *config.Config) error {
	d, err := iofs.New(migrations, "migration")
	if err != nil {
		return err
	}

	m, err := migrate.NewWithSourceInstance("iofs", d, fmt.Sprintf("postgres://%v:%v@%v:%d/%v", config.Database.Username, config.Database.Password, config.Database.Hostname, config.Database.Port, config.Database.Database))
	if err != nil {
		return err
	}

	err = m.Up()
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}

	return err
}
