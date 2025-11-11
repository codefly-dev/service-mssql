package migrations

import (
	"context"
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
)

// Manager defines the interface for different migration systems
type Manager interface {
	// Init initializes the migration manager
	Init(ctx context.Context, configurations []*basev0.Configuration) error

	// Apply runs all pending migrations
	Apply(ctx context.Context) error
	// Update forces a specific migration to be reapplied
	Update(ctx context.Context, migrationFile string) error
}

// Config holds common configuration for migration managers
type Config struct {
	DatabaseName string
	MigrationDir string

	// Optional override for the migration version directory
	MigrationVersionDirOverride *string

	// Optional override for the alembic image
	ImageOverride *string
}

// NewManager creates a migration manager based on the specified format
func NewManager(ctx context.Context, format string, config Config) (Manager, error) {
	switch format {
	case "gomigrate":
		return NewGolangMigrate(ctx, config)
	case "alembic":
		return NewAlembic(ctx, config)
	default:
		return nil, fmt.Errorf("unsupported migration format")
	}
}
