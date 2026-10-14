// Package testutil holds helpers shared by integration tests.
package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// An arbitrary number meaning "I am using the shared test database".
const lockKey int64 = 7_270_001

// LockDatabase makes the calling test the only one using the shared test
// database. `go test ./...` runs packages in parallel, and every database test
// starts by emptying tables, so without this they would wipe each other's data.
//
// The lock belongs to a dedicated connection and is released when the test
// ends. If the test process crashes, closing the connection releases it too.
func LockDatabase(t *testing.T, databaseURL string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect for the test database lock: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		conn.Close(context.Background())
		t.Fatalf("acquire the test database lock: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
}