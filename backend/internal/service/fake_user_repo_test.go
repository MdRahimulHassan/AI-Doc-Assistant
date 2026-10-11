package service

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

// fakeUserRepo is an in-memory UserRepository. No database needed.
type fakeUserRepo struct {
	mu      sync.Mutex
	byEmail map[string]*models.User
	byID    map[uuid.UUID]*models.User
	err     error // if set, every call fails with it (simulates a DB outage)
}

func newFakeUserRepo() *fakeUserRepo {
	return &fakeUserRepo{
		byEmail: map[string]*models.User{},
		byID:    map[uuid.UUID]*models.User{},
	}
}

func (f *fakeUserRepo) Create(_ context.Context, email, hash string) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	email = models.NormalizeEmail(email)
	if _, exists := f.byEmail[email]; exists {
		return nil, models.ErrEmailTaken
	}
	u := &models.User{ID: uuid.New(), Email: email, PasswordHash: hash, CreatedAt: time.Now()}
	f.byEmail[email] = u
	f.byID[u.ID] = u
	cp := *u // return a COPY, like a real database does, so the service
	return &cp, nil // clearing the hash doesn't change what's "stored"
}

func (f *fakeUserRepo) GetByEmail(_ context.Context, email string) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.byEmail[models.NormalizeEmail(email)]
	if !ok {
		return nil, models.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (f *fakeUserRepo) GetByID(_ context.Context, id uuid.UUID) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.byID[id]
	if !ok {
		return nil, models.ErrNotFound
	}
	cp := *u
	return &cp, nil
}