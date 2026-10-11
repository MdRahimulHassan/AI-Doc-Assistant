package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

type UserRepository struct {
	pool *pgxpool.Pool
}

func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
	return &UserRepository{pool: pool}
}

// Create inserts a user. It does NOT check for an existing email first:
// two concurrent requests would both pass that check. Instead it inserts
// and lets the UNIQUE constraint decide.
func (r *UserRepository) Create(ctx context.Context, email, passwordHash string) (*models.User, error) {
	const q = `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		RETURNING id, email, password_hash, created_at`

	// Normalizing here as well makes it impossible for a caller to bypass it.
	// It is idempotent, so doing it in the service too is harmless.
	u, err := scanUser(r.pool.QueryRow(ctx, q, models.NormalizeEmail(email), passwordHash))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, models.ErrEmailTaken
		}
		return nil, fmt.Errorf("insert user: %w", err)
	}
	return u, nil
}

func (r *UserRepository) GetByEmail(ctx context.Context, email string) (*models.User, error) {
	const q = `
		SELECT id, email, password_hash, created_at
		FROM users
		WHERE email = $1`

	u, err := scanUser(r.pool.QueryRow(ctx, q, models.NormalizeEmail(email)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, models.ErrNotFound
		}
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

func (r *UserRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.User, error) {
	const q = `
		SELECT id, email, password_hash, created_at
		FROM users
		WHERE id = $1`

	u, err := scanUser(r.pool.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, models.ErrNotFound
		}
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	return u, nil
}

// scanUser keeps the column order in one place for all three queries.
func scanUser(row pgx.Row) (*models.User, error) {
	var u models.User
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}