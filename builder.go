package main

import (
	"context"
	"embed"
	"fmt"

	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	v0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/communicate"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/shared"
)

type Builder struct {
	services.BuilderServer
	*Service

	answers map[string]*agentv0.Answer
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.LoadService(ctx, req, services.BuilderLoad{
		Settings:         s.Settings,
		Requirements:     requirements,
		FactoryTemplates: factoryFS,
		ResolveEndpoints: func(ctx context.Context, endpoints []*v0.Endpoint) error {
			endpoint, err := resources.FindTCPEndpoint(ctx, endpoints)
			if err != nil {
				return err
			}
			s.TcpEndpoint = endpoint
			s.Wool.Debug("endpoint", wool.Field("tcp", endpoint))
			return nil
		},
	})
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.InitResponse()
}

func (s *Builder) Update(ctx context.Context, req *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()

	return &builderv0.UpdateResponse{}, nil
}

func (s *Builder) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	_ = s.Wool.Inject(ctx)
	return s.Builder.SyncResponse()
}

func (s *Builder) managedRuntimeImage() (*resources.DockerImage, error) {
	if s.Settings != nil && s.Settings.ImageOverride != nil {
		return resources.ParsePinnedImage(*s.Settings.ImageOverride)
	}
	return image, nil
}

func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	managed, err := s.managedRuntimeImage()
	if err != nil {
		return s.Builder.AuditError(err)
	}
	return s.Builder.AuditContainer(ctx, req, managed.FullName())
}

func (s *Builder) SBOM(ctx context.Context, _ *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	managed, err := s.managedRuntimeImage()
	if err != nil {
		return s.Builder.SBOMError(err)
	}
	return s.Builder.SBOMContainer(ctx, managed.FullName())
}

type DockerTemplating struct {
	ConnectionStringKeyHolder string
}

func (s *Builder) WithMigration() bool {
	return !s.Settings.NoMigration
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()

	if !s.WithMigration() {
		s.Wool.Debug("build: no migration")
		return s.Builder.BuildResponse()
	}

	s.Wool.Debug("building migration docker image")

	ctx = s.Wool.Inject(ctx)

	dockerRequest, err := s.Builder.DockerBuildRequest(ctx, req)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "can only do docker build request")
	}

	img := s.DockerImage(dockerRequest)

	if !dockerhelpers.IsValidDockerImageName(img.Name) {
		return s.Builder.BuildError(fmt.Errorf("invalid docker image name: %s", img.Name))
	}

	connectionKey := resources.ServiceSecretConfigurationKey(s.Base.Identity, "mssql", "connection")
	docker := DockerTemplating{ConnectionStringKeyHolder: fmt.Sprintf("{%s}", connectionKey)}

	err = shared.DeleteFile(ctx, s.Local("builder/Dockerfile"))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	err = s.Templates(ctx, docker, services.WithBuilder(builderFS))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
		Root:        s.Location,
		Dockerfile:  "builder/Dockerfile",
		Destination: img,
		Output:      s.Wool,
	})
	if err != nil {
		return s.Builder.BuildError(err)
	}
	_, err = builder.Build(ctx)
	if err != nil {
		return s.Builder.BuildError(err)
	}

	s.Builder.WithDockerImages(img)

	return s.Builder.BuildResponse()
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	parameters := &DeploymentTemplateParameters{
		WithMigration: s.WithMigration(),
		ManagedImage:  image.FullName(),
	}
	var restrictedConfiguration *v0.Configuration
	response, err := s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Inputs: services.DeploymentInputs{
			OwnConfiguration: true,
		},
		Parameters: parameters,
		Prepare: func(ctx context.Context, deployment *services.KustomizeDeploymentContext) error {
			configuration, prepareErr := s.prepareDeployment(ctx, deployment, parameters)
			if prepareErr != nil {
				return prepareErr
			}
			// A restricted render must not receive secret values, so the
			// connection configuration is returned as a value-free reference on
			// the response instead of being exported into the manifests.
			if services.IsRestrictedOutputProfile(deployment.Profile) {
				restrictedConfiguration = configuration
				return nil
			}
			s.Wool.Debug("exporting configuration", wool.Field("conf", resources.MakeConfigurationSummary(configuration)))
			return deployment.ExportConfiguration(ctx, configuration)
		},
	})
	if err != nil ||
		response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS ||
		restrictedConfiguration == nil {
		return response, err
	}
	response.Configuration = restrictedConfiguration
	return response, nil
}

func (s *Builder) prepareDeployment(
	ctx context.Context,
	deployment *services.KustomizeDeploymentContext,
	parameters *DeploymentTemplateParameters,
) (*v0.Configuration, error) {
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, deployment.Request.GetNetworkMappings(), s.TcpEndpoint, resources.NewPublicNetworkAccess())
	if err != nil {
		return nil, err
	}
	if services.IsRestrictedOutputProfile(deployment.Profile) {
		passwordEnv := resources.ServiceSecretConfigurationKeyFromUnique(s.Unique(), "mssql", "MSSQL_PASSWORD")
		passwordReference := deployment.Kubernetes.GetSecretReferences()[passwordEnv]
		if passwordReference == nil {
			return nil, fmt.Errorf("mssql requires a typed Kubernetes Secret reference for %s", passwordEnv)
		}
		if passwordReference.GetOptional() {
			return nil, fmt.Errorf("mssql Secret reference must not be optional")
		}
		parameters.PasswordReference = passwordReference
		return s.restrictedConnectionConfiguration(instance), nil
	}
	return s.CreateConnectionConfiguration(ctx, deployment.Request.GetConfiguration(), instance, !s.Settings.WithoutSSL)
}

func (s *Builder) Options() []*agentv0.Question {
	return []*agentv0.Question{
		communicate.NewConfirm(&agentv0.Message{
			Name:        HotReload,
			Message:     "Migration hot-reload (Recommended)?",
			Description: "codefly can restart your database when migration changes detected 🔎",
		}, true),
		communicate.NewStringInput(&agentv0.Message{
			Name:        DatabaseName,
			Message:     "Name of the database?",
			Description: "Ensure encapsulation of your data",
		}, s.Base.Identity.Module),
		communicate.NewChoice(&agentv0.Message{
			Name:        MigrationFormat,
			Message:     "Choose migration format",
			Description: "Select the database migration tool you prefer",
		},
			&agentv0.Message{
				Name:        "gomigrate",
				Message:     "Golang Migrate",
				Description: "Regular SQL Migrations",
			},
			&agentv0.Message{
				Name:        "alembic",
				Message:     "Alembic",
				Description: "Alembic is a database migration tool that is used to manage the database schema.",
			}),
	}
}

func (s *Builder) Communicate(stream builderv0.Builder_CommunicateServer) error {
	asker := communicate.NewQuestionAsker(stream)
	answers, err := asker.RunSequence(s.Options())
	if err != nil {
		return err
	}
	s.answers = answers
	return nil
}

type create struct {
	DatabaseName    string
	TableName       string
	MigrationFormat string
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	if s.Builder.CreationMode.Communicate {
		var err error
		s.Settings.DatabaseName, err = communicate.InputString(s.answers, DatabaseName)
		if err != nil {
			return s.Builder.CreateError(err)
		}

		choice, err := communicate.Choice(s.answers, MigrationFormat)
		if err != nil {
			return s.Builder.CreateError(err)
		}
		s.Settings.MigrationFormat = choice.Option
	} else {
		options := s.Options()
		var err error

		s.Settings.HotReload, err = communicate.GetDefaultConfirm(options, HotReload)
		if err != nil {
			return s.Builder.CreateError(err)
		}
		if s.Settings.DatabaseName == "" {

			s.Settings.DatabaseName, err = communicate.GetDefaultStringInput(options, DatabaseName)
			if err != nil {
				return s.Builder.CreateError(err)
			}
		}
		if s.Settings.MigrationFormat == "" {
			s.Settings.MigrationFormat = "gomigrate"
		}
	}

	c := create{
		DatabaseName:    s.Settings.DatabaseName,
		TableName:       s.Builder.Service.Name,
		MigrationFormat: s.Settings.MigrationFormat,
	}

	var migrationTemplate *services.TemplateWrapper
	switch s.Settings.MigrationFormat {
	case "gomigrate":
		migrationTemplate = services.WithTemplate(gomigrateFS, "migrations/gomigrate", "migrations").WithDestination("%s", s.Local("migrations"))
	case "alembic":
		migrationTemplate = services.WithTemplate(alembicFS, "migrations/alembic", "migrations").WithDestination("%s", s.Local("migrations"))
	default:
		return s.Builder.CreateError(fmt.Errorf("invalid migration format: %s", s.Settings.MigrationFormat))
	}

	err := s.Templates(ctx, c, services.WithFactory(factoryFS), migrationTemplate)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	err = s.CreateEndpoints(ctx)
	if err != nil {
		return s.Builder.CreateErrorf(err, "cannot create endpoints")
	}

	s.Wool.Debug("created endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(s.Endpoints)))

	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	tcp, err := resources.LoadTCPAPI(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load tcp api")
	}
	endpoint := s.Base.BaseEndpoint(standards.TCP)
	endpoint.Visibility = resources.VisibilityExternal
	s.TcpEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToTCPAPI(tcp))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create tcp endpoint")
	}
	s.Endpoints = []*v0.Endpoint{s.TcpEndpoint}
	return nil
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS

//go:embed templates/migrations/gomigrate
var gomigrateFS embed.FS

//go:embed templates/migrations/alembic
var alembicFS embed.FS
