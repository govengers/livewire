// Package postgresadapter is the PostgreSQL side of livewire on pgx/v5: the LISTEN change
// feed, the trigger and table SQL for the application's migrations, the mutation
// transactor, and the audit, filter, idempotency and presence stores.
//
// A database failure is returned wrapped with %w, so errors.As finds pgx's *pgconn.PgError
// (its SQLSTATE in Code) and errors.Is finds pgx.ErrNoRows and context errors; the wrapping
// messages are not part of the API.
package postgresadapter
