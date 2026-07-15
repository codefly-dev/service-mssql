package main

import (
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/templates"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
)

// Agent version
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
	builders.NewDependency("migrations", "migrations").WithPathSelect(shared.NewSelect("*.sql")),
)

type Settings struct {
	DatabaseName                string  `yaml:"database-name"`
	HotReload                   bool    `yaml:"hot-reload"`
	WithoutSSL                  bool    `yaml:"without-ssl"`                    // Default to SSL
	NoMigration                 bool    `yaml:"no-migration"`                   // Developer only
	MigrationFormat             string  `yaml:"migration-format"`               // golang-migrate or dbmate
	MigrationVersionDirOverride *string `yaml:"migration-version-dir-override"` // migrations directory
	ImageOverride               *string `yaml:"image-override"`                 // image to use for the runtime
	AlembicImageOverride        *string `yaml:"alembic-image-override"`         // image to use for alembic migrations
}

// Constants for settings
const (
	HotReload       = "hot-reload"
	DatabaseName    = "database-name"
	MigrationFormat = "migration-format"
)

var image = &resources.DockerImage{
	Name:   "mcr.microsoft.com/mssql/server",
	Tag:    "2022-CU25-ubuntu-22.04",
	Digest: "sha256:e07b9699a2b749969f19d86563ceeea22bd3a69f7f1db85a8d1ac4bdaf0c6f56",
}

type DeploymentTemplateParameters struct {
	WithMigration bool
	ManagedImage  string
}

type Service struct {
	*services.Base

	// Settings
	*Settings

	sqlServerUser     string
	sqlServerPassword string
	connection        string

	TcpEndpoint *basev0.Endpoint
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return services.Advertisement{
		Backends: runnersbase.BackendSupport{
			Docker: true,
		},
		Config: []*agentv0.ConfigurationValueDetail{
			{
				Name: "mssql", Description: "microsoft sql server credentials",
				Fields: []*agentv0.ConfigurationValueInformation{
					{
						Name: "connection", Description: "connection string",
					},
				}},
		},
		ReadMe: readme,
	}.Build(), nil
}

func NewService() *Service {
	return &Service{
		Base: services.NewServiceBase(context.Background(), agent.Of(resources.ServiceAgent)),
		Settings: &Settings{
			MigrationFormat: "gomigrate", // Default to golang-migrate for backward compatibility
		},
	}
}

func (s *Service) LoadConfiguration(ctx context.Context, conf *basev0.Configuration) error {
	var err error
	s.sqlServerUser, err = resources.GetConfigurationValue(ctx, conf, "mssql", "MSSQL_USER")
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get user")
	}
	s.sqlServerPassword, err = resources.GetConfigurationValue(ctx, conf, "mssql", "MSSQL_PASSWORD")
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get password")
	}
	return nil
}

func (s *Service) createConnectionString(ctx context.Context, conf *basev0.Configuration, address string, withSSL bool) (string, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.LoadConfiguration(ctx, conf)
	if err != nil {
		return "", s.Wool.Wrapf(err, "cannot get user and password")
	}

	// SQL Server connection string format: server=host,port;user id=user;password=pass;database=db;encrypt=disable
	host, port, _ := strings.Cut(address, ":")
	if port == "" {
		port = "1433"
	}

	// Use DSN format for go-mssqldb driver
	// The driver handles special characters in passwords, but we need to URL encode if using URL format
	// For DSN format, special characters like ; and = need to be escaped or the value quoted
	// However, go-mssqldb handles this internally, so we can use the password as-is
	conn := fmt.Sprintf("server=%s,%s;user id=%s;password=%s;database=%s", host, port, s.sqlServerUser, s.sqlServerPassword, s.DatabaseName)
	if !withSSL || strings.Contains(address, "localhost") || strings.Contains(address, "host.docker.internal") {
		conn += ";encrypt=disable"
	} else {
		conn += ";encrypt=true"
	}
	return conn, nil
}

func (s *Service) CreateConnectionConfiguration(ctx context.Context, conf *basev0.Configuration, instance *basev0.NetworkInstance, withSSL bool) (*basev0.Configuration, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	connection, err := s.createConnectionString(ctx, conf, instance.Address, withSSL)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create connection string")
	}

	outputConf := &basev0.Configuration{
		Origin:         s.Base.Unique(),
		RuntimeContext: resources.RuntimeContextFromInstance(instance),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "mssql",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: connection, Secret: true},
				},
			},
		},
	}
	return outputConf, nil
}

func main() {
	agents.Serve(agents.PluginRegistration{
		Agent:   NewService(),
		Runtime: NewRuntime(),
		Builder: NewBuilder(),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
