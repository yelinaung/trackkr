package db

import (
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func RunMigrations(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("running migrations: %w", err)
	}

	return nil
}

// MigrationState reports the migration version recorded by PostgreSQL.
// Applied is false before the first migration has run.
type MigrationState struct {
	Version uint
	Dirty   bool
	Applied bool
}

func GetMigrationStatus(dsn string) (MigrationState, error) {
	m, err := newMigrator(dsn)
	if err != nil {
		return MigrationState{}, err
	}
	defer func() { _, _ = m.Close() }()

	version, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return MigrationState{}, nil
	}
	if err != nil {
		return MigrationState{}, fmt.Errorf("reading migration version: %w", err)
	}
	return MigrationState{Version: version, Dirty: dirty, Applied: true}, nil
}

func ForceMigrationVersion(dsn string, version int) error {
	if err := validateMigrationVersion(version); err != nil {
		return err
	}

	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Force(version); err != nil {
		return fmt.Errorf("forcing migration version: %w", err)
	}
	return nil
}

func validateMigrationVersion(version int) error {
	if version < -1 {
		return fmt.Errorf("migration version %d must be at least -1", version)
	}
	return nil
}

func newMigrator(dsn string) (*migrate.Migrate, error) {
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("creating migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	if err != nil {
		return nil, fmt.Errorf("creating migrator: %w", err)
	}
	return m, nil
}
