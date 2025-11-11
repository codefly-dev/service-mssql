package migrations

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlserver"
	_ "github.com/microsoft/go-mssqldb"
)

type GolangMigrate struct {
	config Config
	w      *wool.Wool

	connection string
}

func NewGolangMigrate(ctx context.Context, config Config) (*GolangMigrate, error) {
	w := wool.Get(ctx).In("golang_migrate")
	return &GolangMigrate{config: config, w: w}, nil
}

func (g *GolangMigrate) getMigrationPath(ctx context.Context) (string, error) {
	u := url.URL{
		Scheme: "file",
		Path:   g.config.MigrationDir,
	}
	return u.String(), nil
}

func (g *GolangMigrate) Init(ctx context.Context, configurations []*basev0.Configuration) error {
	migrationConfig, err := resources.ExtractConfiguration(configurations, resources.NewRuntimeContextNative())
	if err != nil {
		return g.w.Wrapf(err, "cannot extract configuration")
	}
	g.w.Focus("migration config", wool.Field("migration config", migrationConfig))
	connString, err := resources.GetConfigurationValue(ctx, migrationConfig, "mssql", "connection")
	if err != nil {
		return g.w.Wrapf(err, "cannot get connection string")
	}
	g.connection = connString
	g.w.Focus("connection string", wool.Field("connection", g.connection))
	return nil
}

func (g *GolangMigrate) Apply(ctx context.Context) error {
	migrationPath, err := g.getMigrationPath(ctx)
	if err != nil {
		return g.w.Wrapf(err, "cannot get migration path")
	}

	db, err := sql.Open("sqlserver", g.connection)
	if err != nil {
		return g.w.Wrapf(err, "cannot open database")
	}

	driver, err := sqlserver.WithInstance(db, &sqlserver.Config{DatabaseName: g.config.DatabaseName})
	if err != nil {
		return g.w.Wrapf(err, "cannot create driver")
	}

	m, err := migrate.NewWithDatabaseInstance(migrationPath, g.config.DatabaseName, driver)
	if err != nil {
		return g.w.Wrapf(err, "cannot create migration")
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return g.w.Wrapf(err, "cannot apply migration")
	}
	return nil
}

func (g *GolangMigrate) Update(ctx context.Context, migrationFile string) error {
	base := filepath.Base(migrationFile)
	g.w.Info("applying migration: " + base)

	migrationNumber, err := strconv.Atoi(strings.Split(base, "_")[0])
	if err != nil {
		return g.w.Wrapf(err, "cannot parse migration number")
	}

	db, err := sql.Open("sqlserver", g.connection)
	if err != nil {
		return g.w.Wrapf(err, "cannot open database")
	}

	driver, err := sqlserver.WithInstance(db, &sqlserver.Config{DatabaseName: g.config.DatabaseName})
	if err != nil {
		return g.w.Wrapf(err, "cannot create driver")
	}

	migrationPath, err := g.getMigrationPath(ctx)
	if err != nil {
		return g.w.Wrapf(err, "cannot get migration path")
	}

	m, err := migrate.NewWithDatabaseInstance(migrationPath, g.config.DatabaseName, driver)
	if err != nil {
		return g.w.Wrapf(err, "cannot create migration")
	}

	if err := m.Force(migrationNumber); err != nil {
		return g.w.Wrapf(err, "cannot force migration")
	}

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return g.w.Wrapf(err, "cannot apply migration")
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return g.w.Wrapf(err, "cannot apply migration")
	}

	return nil
}
