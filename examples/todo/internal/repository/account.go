package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/livewire/postgresadapter"

	"github.com/govengers/livewire/examples/todo/internal/model"
	"github.com/govengers/livewire/examples/todo/internal/repository/internal/sqlc"
)

// AccountRepository stores the workspace memberships and the API keys.
type AccountRepository struct {
	pool *pgxpool.Pool
}

// AccountRepositoryOptions are the dependencies of an AccountRepository.
type AccountRepositoryOptions struct {
	Pool *pgxpool.Pool // required
}

// NewAccountRepository fails without the pool.
func NewAccountRepository(options *AccountRepositoryOptions) (*AccountRepository, error) {
	if options == nil || options.Pool == nil {
		return nil, errors.New("repository: AccountRepositoryOptions.Pool is required")
	}
	return &AccountRepository{pool: options.Pool}, nil
}

func (r *AccountRepository) database(ctx context.Context) postgresadapter.DBTX {
	if transaction := postgresadapter.TransactionFrom(ctx); transaction != nil {
		return transaction
	}
	return r.pool
}

// Join puts the user into the workspace, idempotently (INT-007).
func (r *AccountRepository) Join(ctx context.Context, userID uuid.UUID, workspace string) error {
	if err := sqlc.New(r.database(ctx)).UpsertMembership(ctx, sqlc.UpsertMembershipParams{UserID: userID, Workspace: workspace}); err != nil {
		return fmt.Errorf("put user %s into a workspace: %w", userID, err)
	}
	return nil
}

// JoinUnlessMember puts the user into the workspace unless it is a member already.
func (r *AccountRepository) JoinUnlessMember(ctx context.Context, userID uuid.UUID, workspace string) error {
	err := sqlc.New(r.database(ctx)).InsertMembershipUnlessPresent(ctx, sqlc.InsertMembershipUnlessPresentParams{UserID: userID, Workspace: workspace})
	if err != nil {
		return fmt.Errorf("put user %s into a workspace: %w", userID, err)
	}
	return nil
}

// Leave removes the user's membership and with it the user's API keys, or answers
// model.ErrNoMember.
func (r *AccountRepository) Leave(ctx context.Context, userID uuid.UUID) error {
	removed, err := sqlc.New(r.database(ctx)).DeleteMembership(ctx, userID)
	switch {
	case err != nil:
		return fmt.Errorf("remove user %s from its workspace: %w", userID, err)
	case removed == 0:
		return model.ErrNoMember
	default:
		return nil
	}
}

// Workspace is the user's workspace, or model.ErrNoMember.
func (r *AccountRepository) Workspace(ctx context.Context, userID uuid.UUID) (string, error) {
	workspace, err := sqlc.New(r.database(ctx)).MembershipWorkspace(ctx, userID)
	if errors.Is(mapError(err), model.ErrNotFound) {
		return "", model.ErrNoMember
	}
	if err != nil {
		return "", fmt.Errorf("the workspace of user %s: %w", userID, err)
	}
	return workspace, nil
}

// CreateAPIKey stores the hash of a new key of the member.
func (r *AccountRepository) CreateAPIKey(ctx context.Context, member model.Member, keyHash []byte) (*model.APIKey, error) {
	row, err := sqlc.New(r.database(ctx)).InsertAPIKey(ctx, sqlc.InsertAPIKeyParams{KeyHash: keyHash, UserID: member.UserID, Username: member.Username})
	if err != nil {
		return nil, fmt.Errorf("create an API key of user %s: %w", member.UserID, err)
	}
	return &model.APIKey{ID: row.ID, CreatedAt: row.CreatedAt}, nil
}

// MemberByKeyHash is the member a key hash belongs to, or model.ErrNotFound.
func (r *AccountRepository) MemberByKeyHash(ctx context.Context, keyHash []byte) (*model.Member, error) {
	row, err := sqlc.New(r.database(ctx)).APIKeyByHash(ctx, keyHash)
	if err != nil {
		return nil, fmt.Errorf("resolve an API key: %w", mapError(err))
	}
	return &model.Member{UserID: row.UserID, Username: row.Username, Workspace: row.Workspace}, nil
}
