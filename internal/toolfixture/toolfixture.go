// Package toolfixture is the registry the tests of the toolset and its adapters run (D82,
// D83): notes, a resource with a guarded column, a create whose body passes the column's
// write check, an admin-only delete and an upload, served by an engine on in-memory stores
// that records every audit entry. It is test support for this repository's modules only.
package toolfixture

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"

	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/query"
)

// Note is the resource's item; Secret is readable and writable by admins only.
type Note struct {
	ID     uuid.UUID `json:"id"`
	Title  string    `json:"title"`
	Secret string    `json:"secret,omitempty"`
}

// CreateNote is the create's body.
type CreateNote struct {
	Title  string `json:"title" validate:"required"`
	Secret string `json:"secret,omitempty"`
}

// NoteParameters name one note.
type NoteParameters struct {
	ID uuid.UUID `path:"id"`
}

// Attachment is the upload's answer.
type Attachment struct {
	Size    int    `json:"size"`
	Content string `json:"content"`
}

// FailingContent is the attachment whose upload fails with an internal error.
const FailingContent = "fail"

// Fixture is the engine, its catalogue and what the stores recorded.
type Fixture struct {
	Engine    *livewire.Engine
	Catalogue *generate.Catalogue
	mutex     sync.Mutex
	notes     []Note
	audit     []livewire.AuditEntry
	keys      map[string]bool
}

// New builds the fixture with one note, "first", whose secret is "hidden".
func New() (*Fixture, error) {
	fixture := &Fixture{notes: []Note{{ID: uuid.MustParse("019a0000-0000-7000-8000-000000000001"), Title: "first", Secret: "hidden"}}, keys: map[string]bool{}}
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api"})
	if err != nil {
		return nil, err
	}
	err = registry.Add(
		livewire.NewResource("notes", "/notes", &livewire.ResourceOptions[Note]{
			Description: "The notes.",
			Read:        authorization.IsUser(),
			Fields: query.Fields{
				{Name: "id", Type: query.TypeUUID, SQL: "notes.id", Filter: true, Sort: true},
				{Name: "title", Type: query.TypeText, SQL: "notes.title", Filter: true, Sort: true, Search: true},
				{Name: "secret", Type: query.TypeText, SQL: "notes.secret", Filter: true, Read: authorization.IsAdmin()},
			},
			DefaultSort: []query.SortTerm{{Field: "title"}},
			List:        fixture.list,
		}),
		livewire.NewMutation("createNote", http.MethodPost, "/notes", &livewire.MutationOptions[livewire.NoParameters, CreateNote, Note]{
			Description: "Creates a note.", Check: authorization.IsUser(), Status: http.StatusCreated, Resource: "notes", Handle: fixture.create,
		}),
		livewire.NewMutation("deleteNote", http.MethodDelete, "/notes/{id}", &livewire.MutationOptions[NoteParameters, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsAdmin(), Handle: fixture.remove,
		}),
		livewire.NewUpload("attachNote", "/notes/{id}/attachments", &livewire.UploadOptions[NoteParameters, Attachment]{
			Check: authorization.IsUser(), ContentTypes: []string{"text/plain"}, MaximumBytes: 64, Handle: fixture.attach,
		}),
	)
	if err != nil {
		return nil, err
	}
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: registry, IdentitySource: refuseSockets{}, Transactor: transactor{}, SameHostOnly: true,
		IdempotencyStore: (*idempotency)(fixture), AuditStore: (*audit)(fixture),
	})
	if err != nil {
		return nil, err
	}
	catalogue, err := generate.NewCatalogue(registry, nil)
	if err != nil {
		return nil, err
	}
	fixture.Engine, fixture.Catalogue = engine, catalogue
	return fixture, nil
}

// User is ctx with alice, a user without roles.
func User(ctx context.Context) context.Context {
	return auth.WithIdentity(ctx, &auth.Identity{Provider: "password", Subject: "alice", Username: "alice", Active: true})
}

// Admin is ctx with root, an admin.
func Admin(ctx context.Context) context.Context {
	return auth.WithIdentity(ctx, &auth.Identity{Provider: "password", Subject: "root", Username: "root", Active: true, Roles: []string{auth.RoleAdmin}})
}

// Audit returns the audit entries written so far.
func (f *Fixture) Audit() []livewire.AuditEntry {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return slices.Clone(f.audit)
}

// Notes returns the notes.
func (f *Fixture) Notes() []Note {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return slices.Clone(f.notes)
}

func (f *Fixture) list(context.Context, *query.ListQuery) (*livewire.Page[Note], error) {
	notes := f.Notes()
	return &livewire.Page[Note]{Items: notes, Total: len(notes)}, nil
}

func (f *Fixture) create(_ context.Context, _ *livewire.NoParameters, body *CreateNote) (*Note, error) {
	note := Note{ID: uuid.Must(uuid.NewV7()), Title: body.Title, Secret: body.Secret}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.notes = append(f.notes, note)
	return &note, nil
}

func (f *Fixture) remove(_ context.Context, parameters *NoteParameters, _ *livewire.NoBody) (*livewire.NoContent, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	position := slices.IndexFunc(f.notes, func(note Note) bool { return note.ID == parameters.ID })
	if position < 0 {
		return nil, livewire.NotFound()
	}
	f.notes = slices.Delete(f.notes, position, position+1)
	return &livewire.NoContent{}, nil
}

func (f *Fixture) attach(_ context.Context, _ *NoteParameters, file *livewire.File) (*Attachment, error) {
	content, err := io.ReadAll(file.Body)
	if err != nil {
		return nil, err
	}
	if string(content) == FailingContent {
		return nil, errors.New("toolfixture: the database password is hunter2") // a 500 whose text must never reach an agent
	}
	return &Attachment{Size: len(content), Content: string(content)}, nil
}

// refuseSockets is the IdentitySource of an engine that serves no socket.
type refuseSockets struct{}

func (refuseSockets) Identify(context.Context, *http.Request) (*auth.Identity, error) {
	return nil, auth.ErrUnauthenticated
}

type transactor struct{}

func (transactor) Begin(context.Context, livewire.TransactionDetails) (livewire.Transaction, error) {
	return transaction{}, nil
}

type transaction struct{}

func (transaction) Commit(ctx context.Context) error { return ctx.Err() }
func (transaction) Rollback(context.Context) error   { return nil }
func (transaction) Handle() any                      { return nil }

// idempotency keeps every key; a repeated one replays as in progress, which the tests never
// need to tell from a replay.
type idempotency Fixture

func (i *idempotency) Reserve(_ context.Context, owner livewire.Owner, key, _ string) (*livewire.IdempotencyRecord, bool, error) {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	if i.keys[owner.Key+"|"+key] {
		return nil, false, livewire.InProgress()
	}
	i.keys[owner.Key+"|"+key] = true
	return nil, false, nil
}

func (i *idempotency) Record(context.Context, *livewire.IdempotencyRecord) error { return nil }

type audit Fixture

func (a *audit) Write(_ context.Context, entry *livewire.AuditEntry) error {
	if entry == nil {
		return errors.New("toolfixture: no audit entry")
	}
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.audit = append(a.audit, *entry)
	return nil
}
