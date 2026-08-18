package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/codefly-dev/core/shared"

	"github.com/codefly-dev/core/agents/helpers/code"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/wool"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/codefly-dev/service-mssql/migrations"
)

// readyTimeout bounds how long WaitForReady polls a freshly started SQL Server.
// A cold first boot copies the system databases and refuses clients with EOF for
// close to a minute, so the budget sits above the 90s hardened_test.go already
// allows the pinned image to serve.
const readyTimeout = 120 * time.Second

type Runtime struct {
	services.RuntimeServer
	*Service

	// internal
	runnerEnvironment *dockerrun.DockerEnvironment

	sqlServerPort    uint16
	migrationManager migrations.Manager
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()

	return s.Runtime.LoadService(ctx, req, services.RuntimeLoad{
		Settings:     s.Settings,
		Requirements: requirements,
		ResolveEndpoints: func(ctx context.Context, endpoints []*basev0.Endpoint) error {
			s.Wool.Debug("endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
			endpoint, err := resources.FindTCPEndpoint(ctx, endpoints)
			if err != nil {
				return s.Wool.Wrapf(err, "cannot find TCP endpoint")
			}
			s.TcpEndpoint = endpoint
			return nil
		},
	})
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)
	s.Runtime.WithContext(req.GetRuntimeContext())

	w := s.Wool.In("runtime::init")

	s.NetworkMappings = req.ProposedNetworkMappings

	configuration := req.GetConfiguration()

	net, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.TcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if net == nil {
		return s.Runtime.InitError(w.NewError("network mapping is nil"))
	}

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, s.Runtime.NetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if instance == nil {
		return s.Runtime.InitError(w.NewError("network instance is nil"))
	}

	w.Debug("tcp network instance", wool.Field("instance", instance))

	s.Infof("will run on %s", instance.Host)
	s.sqlServerPort = 1433

	// Create connection string resources for the network instance
	for _, inst := range net.Instances {
		conf, errConn := s.CreateConnectionConfiguration(ctx, configuration, inst, false)
		if errConn != nil {
			return s.Runtime.InitError(errConn)
		}
		w.Debug("adding configuration", wool.Field("config", resources.MakeConfigurationSummary(conf)), wool.Field("instance", inst))
		s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, conf)
	}

	s.Wool.Debug("sending runtime configuration", wool.Field("conf", resources.MakeManyConfigurationSummary(s.Runtime.RuntimeConfigurations)))

	w.Debug("setting up connection string for migrations")
	// Setup a connection string for migration
	hostInstance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, s.Runtime.NetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)

	}

	s.connection, err = s.createConnectionString(ctx, configuration, hostInstance.Address, false)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	w.Debug("connection string", wool.Field("connection", s.connection))

	// Docker
	runnerImage := image
	if s.Settings.ImageOverride != nil {
		runnerImage, err = resources.ParsePinnedImage(*s.Settings.ImageOverride)
		if err != nil {
			return s.Runtime.InitErrorf(err, "invalid image override")
		}
	}

	runner, err := dockerrun.NewDockerHeadlessEnvironment(ctx, runnerImage, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.InitError(err)
	}
	err = s.LoadConfiguration(ctx, configuration)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithOutput(s.Wool)
	runner.WithPortMapping(ctx, uint16(instance.Port), s.sqlServerPort)

	// SQL Server environment variables
	runner.WithEnvironmentVariables(
		ctx,
		resources.Env("ACCEPT_EULA", "Y"),
		resources.Env("MSSQL_SA_PASSWORD", s.sqlServerPassword),
		resources.Env("MSSQL_PID", "Developer"))

	s.runnerEnvironment = runner

	w.Debug("init for runner environment: will start container")
	err = s.runnerEnvironment.Init(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if !s.Settings.NoMigration {
		migrationConfig := migrations.Config{
			DatabaseName: s.Settings.DatabaseName,
			MigrationDir: s.Local("migrations"),
		}

		if s.Settings.MigrationVersionDirOverride != nil {
			versionOverride := s.Local("%s", *s.Settings.MigrationVersionDirOverride)
			empty, err := shared.CheckEmptyDirectory(ctx, versionOverride)
			if err != nil {
				return s.Runtime.InitError(err)
			}
			if empty {
				return s.Runtime.InitError(w.NewError("migration version directory is empty"))
			}
			migrationConfig.MigrationVersionDirOverride = shared.Pointer(versionOverride)
		}

		if s.Settings.AlembicImageOverride != nil && s.Settings.MigrationFormat == "alembic" {
			migrationConfig.ImageOverride = s.Settings.AlembicImageOverride
		}

		manager, err := migrations.NewManager(ctx, s.Settings.MigrationFormat, migrationConfig)
		if err != nil {
			return s.Runtime.InitError(err)
		}
		s.migrationManager = manager
	}

	s.Wool.Debug("init successful")
	return s.Runtime.InitResponse()
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	_ = s.Wool.Inject(ctx)

	s.Wool.Debug("waiting for ready", wool.Field("connection", s.connection))

	// Connect to master database first (target database may not exist yet)
	masterConn := strings.ReplaceAll(s.connection, fmt.Sprintf("database=%s", s.DatabaseName), "database=master")
	if !strings.Contains(masterConn, "connection timeout=") {
		masterConn += ";connection timeout=10"
	}

	// On a cold data volume MSSQL copies the system databases before it accepts
	// clients, fast-failing connections with EOF for the better part of a minute
	// (the pinned image is given up to 90s to serve in hardened_test.go). Wait
	// against a wall-clock deadline rather than a fixed retry count so a slow
	// first boot cannot exhaust the budget while the server is still coming up.
	deadline := time.Now().Add(readyTimeout)
	retryDelay := 3 * time.Second
	for time.Now().Before(deadline) {
		// Check if context is cancelled
		select {
		case <-ctx.Done():
			return s.Wool.Wrapf(ctx.Err(), "context cancelled while waiting for database")
		default:
		}

		db, err := sql.Open("sqlserver", masterConn)
		if err != nil {
			s.Wool.Debug("failed to open database connection", wool.ErrField(err))
			time.Sleep(retryDelay)
			continue
		}

		// Set connection timeout
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = db.PingContext(pingCtx)
		cancel()

		if err != nil {
			s.Wool.Debug("ping failed", wool.ErrField(err))
			db.Close()
			time.Sleep(retryDelay)
			continue
		}

		s.Wool.Debug("ping successful")

		// Try to execute a simple query with timeout
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = db.ExecContext(queryCtx, "SELECT 1")
		cancel()
		db.Close()

		if err != nil {
			s.Wool.Debug("query failed", wool.ErrField(err))
			time.Sleep(retryDelay)
			continue
		}

		s.Wool.Debug("database ready!")
		return nil
	}

	return s.Wool.NewError("database is not ready after %s", readyTimeout)
}

func (s *Runtime) createDatabaseIfNotExists(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	// Connect to master database to create the target database
	// Parse connection string and replace database name with master
	masterConn := strings.ReplaceAll(s.connection, fmt.Sprintf("database=%s", s.DatabaseName), "database=master")

	db, err := sql.Open("sqlserver", masterConn)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot open connection to master database")
	}
	defer db.Close()

	// Check if database exists
	var exists int
	err = db.QueryRow(`
		SELECT CASE WHEN EXISTS (
			SELECT * FROM sys.databases WHERE name = @p1
		) THEN 1 ELSE 0 END
	`, sql.Named("p1", s.DatabaseName)).Scan(&exists)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot check if database exists")
	}

	if exists == 0 {
		s.Wool.Focus("creating database", wool.Field("database", s.DatabaseName))
		// Create database with proper escaping
		createSQL := fmt.Sprintf("CREATE DATABASE [%s]", strings.ReplaceAll(s.DatabaseName, "]", "]]"))
		_, err = db.Exec(createSQL)
		if err != nil {
			return s.Wool.Wrapf(err, "cannot create database")
		}
		s.Wool.Focus("database created")
	} else {
		s.Wool.Debug("database already exists", wool.Field("database", s.DatabaseName))
	}

	return nil
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("starting")

	s.Wool.Debug("waiting for ready")

	err := s.WaitForReady(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	// Create database if it doesn't exist
	err = s.createDatabaseIfNotExists(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	if !s.Settings.NoMigration && s.migrationManager != nil {
		err = s.migrationManager.Init(ctx, s.Runtime.RuntimeConfigurations)
		if err != nil {
			return s.Runtime.StartError(err)
		}
		s.Wool.Focus("applying migrations")
		err = s.migrationManager.Apply(ctx)
		if err != nil {
			return s.Runtime.StartError(err)
		}
		s.Wool.Focus("migrations applied")
	}

	if s.Settings.HotReload {
		conf := services.NewWatchConfiguration(requirements)
		err := s.SetupWatcher(ctx, conf, s.EventHandler)
		if err != nil {
			s.Wool.Warn("error in watcher", wool.ErrField(err))
		}
	}
	s.Wool.Debug("start done")
	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("nothing to stop: keep environment alive")

	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying")

	// Use the existing runner environment if available
	if s.runnerEnvironment != nil {
		err := s.runnerEnvironment.Shutdown(ctx)
		if err != nil {
			return s.Runtime.DestroyError(err)
		}
		s.runnerEnvironment = nil
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	if strings.Contains(event.Path, "migrations") && s.migrationManager != nil {
		err := s.migrationManager.Update(context.Background(), event.Path)
		if err != nil {
			s.Wool.Warn("cannot apply migration", wool.ErrField(err))
		}
	}
	return nil
}
