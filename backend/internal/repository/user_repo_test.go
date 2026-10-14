package repository

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/testutil"
)

// setup connects to the TEST database, applies migrations, and empties the table.
func setup(t *testing.T) (*UserRepository, *pgxpool.Pool) {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	// Safety net: this test TRUNCATES tables.
	if !strings.Contains(url, "_test") {
		t.Fatal("TEST_DATABASE_URL must point to a database whose name contains '_test'")
	}

	if err := database.RunMigrations(url); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	pool, err := database.NewPool(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	testutil.LockDatabase(t, url)

	if _, err := pool.Exec(context.Background(), "TRUNCATE users CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return NewUserRepository(pool), pool
}

func TestCreateAndGet(t *testing.T) {
	repo, _ := setup(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, "  Alice@Example.COM ", "hash-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil || created.CreatedAt.IsZero() {
		t.Error("expected ID and CreatedAt to be filled by the database")
	}
	if created.Email != "alice@example.com" {
		t.Errorf("email stored as %q, want normalized form", created.Email)
	}

	byEmail, err := repo.GetByEmail(ctx, "ALICE@example.com")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if byEmail.ID != created.ID || byEmail.PasswordHash != "hash-1" {
		t.Errorf("GetByEmail returned a different user: %+v", byEmail)
	}

	byID, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if byID.Email != "alice@example.com" {
		t.Errorf("GetByID email = %q", byID.Email)
	}
}

func TestCreate_DuplicateEmail(t *testing.T) {
	repo, _ := setup(t)
	ctx := context.Background()

	if _, err := repo.Create(ctx, "bob@example.com", "h"); err != nil {
		t.Fatal(err)
	}
	// Same email, different case and spacing: still a duplicate.
	_, err := repo.Create(ctx, " BOB@example.com ", "h")
	if !errors.Is(err, models.ErrEmailTaken) {
		t.Errorf("err = %v, want ErrEmailTaken", err)
	}
}

func TestGet_NotFound(t *testing.T) {
	repo, _ := setup(t)
	ctx := context.Background()

	if _, err := repo.GetByEmail(ctx, "nobody@example.com"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("GetByEmail err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByID(ctx, uuid.New()); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("GetByID err = %v, want ErrNotFound", err)
	}
}

// Proves migration 000002: even if app code forgot to normalize,
// the database refuses a case-variant duplicate. (Fails if you skipped Step 3.)
func TestDB_RejectsCaseVariantEmails(t *testing.T) {
	_, pool := setup(t)
	ctx := context.Background()

	const q = `INSERT INTO users (email, password_hash) VALUES ($1, 'h')`
	if _, err := pool.Exec(ctx, q, "Case@Example.com"); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, q, "case@example.com")
	if err == nil || !isUniqueViolation(err) {
		t.Errorf("err = %v, want a unique violation", err)
	}
}

// The race condition from the step description, made real:
// 10 simultaneous signups with the same email, exactly one may win.
func TestCreate_ConcurrentSameEmail(t *testing.T) {
	repo, _ := setup(t)

	const n = 10
	results := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.Create(context.Background(), "race@example.com", "h")
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var succeeded, taken int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, models.ErrEmailTaken):
			taken++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 || taken != n-1 {
		t.Errorf("succeeded=%d taken=%d, want 1 and %d", succeeded, taken, n-1)
	}
}