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
// poll against ignoring context cancellation. It must return promptly with the
// context error instead of sleeping out its retry window (the original
// time.Sleep loop) or, when the cancellation surfaces inside a probe on the
// final attempt, swallowing it as a retryable error and reporting the misleading
// "no tables found" outcome.
func TestWaitForApplicationTablesHonorsCancellation(t *testing.T) {
	a := &Alembic{w: wool.Get(context.Background())}

	// Point at an endpoint that never accepts a connection: the poll must give
	// up because the context is cancelled, not because it managed to connect.
	db, err := sql.Open("sqlserver", "sqlserver://sa:test@127.0.0.1:1?database=master")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// maxRetries=1 exercises the final-attempt probe path (no subsequent
	// ctx.Done() wait to catch cancellation); maxRetries=12 the multi-attempt
	// path. Both must surface context.Canceled quickly. With a 30s delay the
	// pre-fix loop would block for minutes.
	for _, maxRetries := range []int{1, 12} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already cancelled before the poll starts

		start := time.Now()
		_, _, waitErr := a.waitForApplicationTables(ctx, db, maxRetries, 30*time.Second)
		elapsed := time.Since(start)

		if !errors.Is(waitErr, context.Canceled) {
			t.Fatalf("maxRetries=%d: expected context.Canceled, got %v", maxRetries, waitErr)
		}
		if elapsed > 10*time.Second {
			t.Fatalf("maxRetries=%d: poll ignored cancellation: took %s", maxRetries, elapsed)
		}
	}
}
