package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/govengers/livewire/examples/todo/internal/actor"
	"github.com/govengers/livewire/examples/todo/internal/model"
	"github.com/govengers/livewire/examples/todo/internal/repository"
)

// apiKeyBytes is the randomness of an API key.
const apiKeyBytes = 32

// apiKeyPrefix marks the example's keys, so a leaked one is recognizable.
const apiKeyPrefix = "todo_"

// AccountService runs the workspace memberships and the API keys.
type AccountService struct {
	accounts *repository.AccountRepository
}

// AccountServiceOptions configures an AccountService.
type AccountServiceOptions struct {
	AccountRepository *repository.AccountRepository // required
}

// NewAccountService returns an AccountService.
func NewAccountService(options *AccountServiceOptions) (*AccountService, error) {
	if options == nil || options.AccountRepository == nil {
		return nil, errors.New("service: AccountServiceOptions.AccountRepository is required")
	}
	return &AccountService{accounts: options.AccountRepository}, nil
}

// Join puts a user into a workspace.
func (s *AccountService) Join(ctx context.Context, userID uuid.UUID, workspace string) error {
	return s.accounts.Join(ctx, userID, workspace)
}

// Workspace is the user's workspace, or model.ErrNoMember.
func (s *AccountService) Workspace(ctx context.Context, userID uuid.UUID) (string, error) {
	return s.accounts.Workspace(ctx, userID)
}

// CreateAPIKey creates a key for the member in ctx; the answer carries the key, which is
// stored as its hash and shown this once.
func (s *AccountService) CreateAPIKey(ctx context.Context) (*model.APIKey, error) {
	member, err := actor.Member(ctx)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, apiKeyBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("an API key: %w", err)
	}
	key := apiKeyPrefix + base64.RawURLEncoding.EncodeToString(secret)
	created, err := s.accounts.CreateAPIKey(ctx, member, hashOf(key))
	if err != nil {
		return nil, err
	}
	created.Key = key
	return created, nil
}

// MemberOfAPIKey is the member a key belongs to, or model.ErrNotFound.
func (s *AccountService) MemberOfAPIKey(ctx context.Context, key string) (*model.Member, error) {
	return s.accounts.MemberByKeyHash(ctx, hashOf(key))
}

// hashOf is what the key is stored and looked up as: a key carries 256 random bits, so a
// plain SHA-256 is enough and lets the lookup use the unique index.
func hashOf(key string) []byte {
	hash := sha256.Sum256([]byte(key))
	return hash[:]
}
