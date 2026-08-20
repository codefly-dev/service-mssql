package migrations

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/codefly-dev/core/wool"
)

// TestWaitForApplicationTablesHonorsCancellation guards the post-migration table
// poll against ignoring context cancellation. With the previous time.Sleep-based
// loop a cancelled context was ignored and the poll slept out its entire retry
// window; the poll must instead return promptly with the context error.
func TestWaitForApplicationTablesHonorsCancellation(t *testing.T) {
	a := &Alembic{w: wool.Get(context.Background())}

	// Point at an endpoint that never accepts a connection: the poll must give
	// up because the context is cancelled, not because it managed to connect.
	db, err := sql.Open("sqlserver", "sqlserver://sa:test@127.0.0.1:1?database=master")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the poll starts

	// With a 30s delay across 12 attempts the pre-fix loop would block for
	// minutes; honouring cancellation must return within a couple of seconds.
	start := time.Now()
	_, _, waitErr := a.waitForApplicationTables(ctx, db, 12, 30*time.Second)
	elapsed := time.Since(start)

	if waitErr == nil {
		t.Fatal("expected a context error, got nil")
	}
	if !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", waitErr)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("poll ignored cancellation: took %s", elapsed)
	}
}
