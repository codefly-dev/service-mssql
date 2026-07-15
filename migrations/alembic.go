package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/wool"
)

type Alembic struct {
	config Config
	w      *wool.Wool

	containerConnection string // For use inside Docker
	nativeConnection    string // For use on host
}

var alembicImage = &resources.DockerImage{
	Name:   "codeflydev/mssql-alembic",
	Digest: "sha256:d0a5e79de5c88d2bdbf29db3b4b2352a7b612aa0ce29ac13574d77941381f03c",
}

func NewAlembic(ctx context.Context, config Config) (*Alembic, error) {
	w := wool.Get(ctx)
	return &Alembic{config: config, w: w}, nil
}

// convertDSNToSQLAlchemyURL converts a SQL Server DSN connection string to SQLAlchemy URL format
// DSN format: server=host,port;user id=user;password=pass;database=db;encrypt=disable
// SQLAlchemy format: mssql+pyodbc://user:password@host:port/database?driver=ODBC+Driver+17+for+SQL+Server
func convertDSNToSQLAlchemyURL(dsn string) (string, error) {
	// Parse DSN format
	var server, userID, password, database string
	var encrypt string

	parts := strings.Split(dsn, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "server=") {
			server = strings.TrimPrefix(part, "server=")
		} else if strings.HasPrefix(part, "user id=") {
			userID = strings.TrimPrefix(part, "user id=")
		} else if strings.HasPrefix(part, "password=") {
			password = strings.TrimPrefix(part, "password=")
		} else if strings.HasPrefix(part, "database=") {
			database = strings.TrimPrefix(part, "database=")
		} else if strings.HasPrefix(part, "encrypt=") {
			encrypt = strings.TrimPrefix(part, "encrypt=")
		}
	}

	if server == "" || userID == "" || password == "" || database == "" {
		return "", fmt.Errorf("invalid DSN format: missing required fields")
	}

	// Parse server (host,port)
	host := server
	port := "1433"
	if strings.Contains(server, ",") {
		parts := strings.SplitN(server, ",", 2)
		host = parts[0]
		port = parts[1]
	}

	// URL encode password and user (pymssql handles special characters in URLs)
	encodedUser := url.QueryEscape(userID)
	encodedPassword := url.QueryEscape(password)
	encodedDatabase := url.QueryEscape(database)

	// Build SQLAlchemy URL for pyodbc driver (more reliable, better support)
	// Format: mssql+pyodbc://user:password@host:port/database?driver=ODBC+Driver+17+for+SQL+Server
	sqlalchemyURL := fmt.Sprintf("mssql+pyodbc://%s:%s@%s:%s/%s", encodedUser, encodedPassword, host, port, encodedDatabase)

	// Add driver and connection parameters
	// Use FreeTDS driver which works on all architectures
	params := []string{"driver=FreeTDS"}
	// For FreeTDS, we configure encryption via TDS version
	if encrypt == "disable" {
		params = append(params, "TDS_Version=7.4")
	} else {
		params = append(params, "TDS_Version=8.0")
	}

	if len(params) > 0 {
		sqlalchemyURL += "?" + strings.Join(params, "&")
	}

	return sqlalchemyURL, nil
}

func (a *Alembic) Init(ctx context.Context, configurations []*basev0.Configuration) error {
	// Get container connection string
	containerConfig, err := resources.ExtractConfiguration(configurations, resources.NewRuntimeContextContainer())
	if err != nil {
		return a.w.Wrapf(err, "cannot extract container configuration")
	}
	a.containerConnection, err = resources.GetConfigurationValue(ctx, containerConfig, "mssql", "connection")
	if err != nil {
		return a.w.Wrapf(err, "cannot get container connection string")
	}

	// Get native connection string
	nativeConfig, err := resources.ExtractConfiguration(configurations, resources.NewRuntimeContextNative())
	if err != nil {
		return a.w.Wrapf(err, "cannot extract native configuration")
	}
	a.nativeConnection, err = resources.GetConfigurationValue(ctx, nativeConfig, "mssql", "connection")
	if err != nil {
		return a.w.Wrapf(err, "cannot get native connection string")
	}

	a.w.Focus("connection strings",
		wool.Field("container", a.containerConnection),
		wool.Field("native", a.nativeConnection))
	return nil
}

func (a *Alembic) getRunner(ctx context.Context) (*dockerrun.DockerEnvironment, error) {
	name := fmt.Sprintf("alembic-%d", time.Now().UnixMilli())

	// Debug directory contents
	a.w.Debug("checking migrations directory",
		wool.Field("dir", a.config.MigrationDir))
	entries, err := os.ReadDir(a.config.MigrationDir)
	if err != nil {
		a.w.Warn("cannot read migrations directory", wool.ErrField(err))
	} else {
		var files []string
		for _, entry := range entries {
			files = append(files, entry.Name())
		}
		a.w.Debug("migrations directory contents", wool.Field("files", files))
	}

	// Use SQL Server-specific image for Microsoft SQL Server support
	// Users can override with AlembicImageOverride setting if needed
	// The publisher currently exposes only a floating `latest` tag. Pin the
	// resolved manifest digest so migration behavior cannot change silently.
	image := alembicImage
	if a.config.ImageOverride != nil {
		var err error
		image, err = resources.ParsePinnedImage(*a.config.ImageOverride)
		if err != nil {
			return nil, a.w.Wrapf(err, "cannot parse alembic image override")
		}
	}
	runner, err := dockerrun.NewDockerEnvironment(ctx, image, a.config.MigrationDir, name)
	if err != nil {
		return nil, a.w.Wrapf(err, "cannot create docker environment")
	}

	// Mount migrations directory which should contain alembic.ini and versions/
	runner.WithMount(a.config.MigrationDir, "/workspace")
	if a.config.MigrationVersionDirOverride != nil {
		runner.WithMount(*a.config.MigrationVersionDirOverride, "/workspace/versions")
	}
	runner.WithWorkDir("/workspace")
	runner.WithPause()

	// Convert DSN format to SQLAlchemy URL format for Alembic
	sqlalchemyURL, err := convertDSNToSQLAlchemyURL(a.containerConnection)
	if err != nil {
		return nil, a.w.Wrapf(err, "cannot convert connection string to SQLAlchemy URL")
	}

	// Set environment variables
	runner.WithEnvironmentVariables(ctx,
		resources.Env("DATABASE_URL", sqlalchemyURL),
	)

	return runner, nil
}

func (a *Alembic) Apply(ctx context.Context) error {
	// Create a detached context with no timeout/deadline for migration operations
	// This will prevent context cancellation from interfering with DB operations
	migrationCtx := context.Background()

	runner, err := a.getRunner(ctx)
	if err != nil {
		return err
	}

	defer func() {
		err = runner.Shutdown(ctx)
		if err != nil {
			a.w.Warn("cannot shutdown runner", wool.ErrField(err))
		}
	}()

	err = runner.Init(ctx)
	if err != nil {
		return a.w.Wrapf(err, "cannot init runner")
	}

	// Wait a moment for SQL Server to be fully ready (especially after database creation)
	time.Sleep(2 * time.Second)

	// First check current state
	// Note: If database is empty, alembic current may fail - that's OK, we'll proceed with upgrade
	a.w.Focus("checking current migration state")
	currentProc, err := runner.NewProcess("alembic", "-c", "/workspace/alembic.ini", "current")
	if err != nil {
		return a.w.Wrapf(err, "cannot create current process")
	}
	currentProc.WithOutput(a.w)
	err = currentProc.Run(migrationCtx) // Use the detached context
	if err != nil {
		// If current fails, it might be because database is empty - log but continue
		a.w.Debug("alembic current failed (may be empty database)", wool.ErrField(err))
		// Don't return error, proceed with upgrade which will handle empty database
	}

	// Run alembic upgrade
	a.w.Focus("starting migrations to latest version")
	proc, err := runner.NewProcess("alembic", "-c", "/workspace/alembic.ini", "upgrade", "head")
	if err != nil {
		return a.w.Wrapf(err, "cannot create process")
	}
	proc.WithOutput(a.w)

	a.w.Focus("running upgrade process")
	err = proc.Run(migrationCtx) // Use the detached context
	if err != nil {
		// Only perform transaction cleanup if the migration failed
		a.w.Debug("migration failed, attempting transaction cleanup")

		// SQL Server transactions are typically handled automatically by the driver
		a.w.Debug("SQL Server transaction cleanup handled by driver")

		return a.w.Wrapf(err, "alembic upgrade failed")
	}
	a.w.Focus("upgrade process completed")

	// Check final state
	a.w.Focus("checking final migration state")
	finalProc, err := runner.NewProcess("alembic", "-c", "/workspace/alembic.ini", "current")
	if err != nil {
		return a.w.Wrapf(err, "cannot create final check process")
	}
	finalProc.WithOutput(a.w)
	err = finalProc.Run(migrationCtx) // Use detached context
	if err != nil {
		return a.w.Wrapf(err, "cannot check final version")
	}

	a.w.Focus("checking tables in database")

	// Check tables using native connection with retries for up to 1 minute
	maxRetries := 12 // Try 12 times with 10-second intervals = 120 seconds total
	retryDelay := time.Second * 10
	var tables []string
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			a.w.Debug("retrying table check", wool.Field("attempt", i+1), wool.Field("max_retries", maxRetries))
			time.Sleep(retryDelay)
		}

		a.w.Debug("checking database for tables", wool.Field("attempt", i+1),
			wool.Field("elapsed_time", time.Duration(i)*retryDelay),
			wool.Field("timeout", time.Duration(maxRetries)*retryDelay))
		db, err := sql.Open("sqlserver", a.nativeConnection)
		if err != nil {
			a.w.Debug("failed to open database connection", wool.Field("attempt", i+1), wool.ErrField(err))
			lastErr = err
			continue
		}
		defer db.Close()

		// Test the connection
		err = db.Ping()
		if err != nil {
			a.w.Debug("database ping failed", wool.Field("attempt", i+1), wool.ErrField(err))
			lastErr = err
			continue
		}

		// List all tables including version tables
		query := `
			SELECT TABLE_NAME
			FROM INFORMATION_SCHEMA.TABLES
			WHERE TABLE_TYPE = 'BASE TABLE'
			AND TABLE_SCHEMA = 'dbo'
			AND TABLE_NAME NOT LIKE 'sys%'
			AND TABLE_NAME != 'alembic_version'
		`
		rows, err := db.Query(query)
		if err != nil {
			lastErr = err
			continue
		}
		defer rows.Close()

		tables = nil
		for rows.Next() {
			var table string
			if err := rows.Scan(&table); err != nil {
				lastErr = err
				continue
			}
			tables = append(tables, table)
		}
		if rows.Err() != nil {
			lastErr = rows.Err()
		}

		if len(tables) > 0 {
			break
		}
	}

	// Log tables but don't fail if empty
	a.w.Focus("tables in database", wool.Field("tables", tables))
	if len(tables) == 0 {
		// Open a fresh connection to check for alembic_version
		finalDb, finalErr := sql.Open("sqlserver", a.nativeConnection)
		if finalErr != nil {
			a.w.Debug("failed to open final database connection", wool.ErrField(finalErr))
		} else {
			defer finalDb.Close()

			// Check for alembic_version table to see if migrations ran but didn't create tables
			var hasAlembicVersion bool
			vErr := finalDb.QueryRow(`
				SELECT CASE WHEN EXISTS (
					SELECT * FROM INFORMATION_SCHEMA.TABLES
					WHERE TABLE_SCHEMA = 'dbo'
					AND TABLE_NAME = 'alembic_version'
				) THEN 1 ELSE 0 END
			`).Scan(&hasAlembicVersion)

			if vErr == nil && hasAlembicVersion {
				// Check version records to provide better error information
				var versions []string
				vRows, vRowErr := finalDb.Query("SELECT version_num FROM alembic_version")
				if vRowErr == nil {
					defer vRows.Close()
					for vRows.Next() {
						var v string
						if vRows.Scan(&v) == nil {
							versions = append(versions, v)
						}
					}
				}

				a.w.Warn("migration completed but no application tables found",
					wool.Field("versions", versions),
					wool.Field("max_wait_time", time.Duration(maxRetries)*retryDelay))

				return a.w.Wrap(fmt.Errorf("migration completed (alembic_version exists with versions: %v) but no application tables were created after waiting %s - this may indicate the migration files don't create any tables or there was a transaction issue", versions, time.Duration(maxRetries)*retryDelay))
			}
		}

		if lastErr != nil {
			return a.w.Wrapf(lastErr, "failed to check tables after %d attempts (waited %s)", maxRetries, time.Duration(maxRetries)*retryDelay)
		}
		return a.w.Wrap(fmt.Errorf("no tables found in database after waiting %s (%d attempts): migrations failed or are taking too long to commit", time.Duration(maxRetries)*retryDelay, maxRetries))
	}
	return nil
}

func (a *Alembic) Update(ctx context.Context, migrationFile string) error {
	runner, err := a.getRunner(ctx)
	if err != nil {
		return err
	}

	err = runner.Init(ctx)
	if err != nil {
		return a.w.Wrapf(err, "cannot init docker environment")
	}
	defer runner.Shutdown(ctx)

	// Force reapply by running down and up
	proc, err := runner.NewProcess("alembic", "downgrade", "-1")
	if err != nil {
		return a.w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return a.w.Wrapf(err, "alembic downgrade failed")
	}

	proc, err = runner.NewProcess("alembic", "upgrade", "+1")
	if err != nil {
		return a.w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return a.w.Wrapf(err, "alembic upgrade failed")
	}
	return nil
}
