package livewiretest_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/ginadapter"
	"github.com/govengers/livewire/livewiretest"
	"github.com/govengers/livewire/query"
)

// The application the gates run against in these tests and examples: notes (a filterable
// resource), tags (a typed list) and a summary (an object counting both), served on gin in
// front of an engine on in-memory adapters, one tenant per user. Its options break it the
// ways the gates must catch.

// userHeader names the user a request acts as; the test application trusts it.
const userHeader = "X-Test-User"

// tenantOf is the tenant of each user.
var tenantOf = map[string]string{"alice": "north", "bob": "south", "dave": "west"}

type (
	note struct {
		ID      uuid.UUID `json:"id"`
		Title   string    `json:"title"`
		Version int       `json:"version"`
	}
	tag struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	}
	summary struct {
		Notes int `json:"notes"`
		Tags  int `json:"tags"`
	}
	// board is an object whose body has items and a limit, like a list's.
	board struct {
		Items []string `json:"items"`
		Limit int      `json:"limit"`
	}
	// memo is a row of a resource without any key.
	memo struct {
		Title string `json:"title"`
	}
)

// brokenBy names the ways the application is broken; the zero value is a sound application.
type brokenBy struct {
	notesPerTenant    int      // the notes of each tenant (default 1)
	leakingTenants    bool     // the notes list, item and export ignore the tenant
	leakingDistinct   bool     // the distinct read ignores the tenant
	leakingHistory    bool     // a note's history carries every note's version
	leakingItemsFrom  int      // above zero: the item read answers the other tenant's notes from this position on
	leakingPastCap    bool     // the list appends the other tenant's notes after its own
	pageSize          int      // the notes' MaximumLimit (default 100)
	countCap          int      // the notes' CountCap
	exportWithoutKey  bool     // the export writes the title only
	keylessMemos      bool     // a resource memos whose rows carry no key
	boardObject       bool     // an object board whose limit counts the tags but lists notes only
	summaryTables     []string // the summary's Tables instead of notes and tags
	unstableNotes     bool     // every notes list read answers another title
	respelledDistinct bool     // distinct version counts the other tenant's notes as 1.0
	sharedCategories  bool     // a resource categories whose rows every tenant reads on purpose
	formulaCodes      bool     // a resource codes keyed by texts a spreadsheet would run (D69)
}

// notesApplication is the running application and its data.
type notesApplication struct {
	livewiretest.Application
	feed   *feed
	mutex  sync.Mutex
	notes  map[string][]note // by tenant
	tags   map[string][]tag  // by tenant
	reads  atomic.Int64
	broken brokenBy
	stop   func()
}

// startNotes starts the application with notes and one tag per tenant.
func startNotes(broken brokenBy) (*notesApplication, error) {
	application := &notesApplication{feed: &feed{connected: make(chan struct{})}, notes: map[string][]note{}, tags: map[string][]tag{}, broken: broken}
	for _, tenant := range []string{"north", "south"} {
		for position := range max(1, broken.notesPerTenant) {
			application.notes[tenant] = append(application.notes[tenant], note{ID: uuid.New(), Title: fmt.Sprintf("%s %02d", tenant, position+1), Version: 1})
		}
		application.tags[tenant] = []tag{{ID: uuid.New(), Name: "urgent"}}
	}
	registry, err := application.registry()
	if err != nil {
		return nil, err
	}
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: registry, ChangeFeed: application.feed, IdentitySource: headerIdentity{}, Transactor: transactor{},
		SameHostOnly: true, DeliveryBatchWait: time.Millisecond,
	})
	if err != nil {
		return nil, err
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(func(ginContext *gin.Context) {
		if identity := identityOf(ginContext.Request); identity != nil {
			ginContext.Request = ginContext.Request.WithContext(auth.WithIdentity(ginContext.Request.Context(), identity))
		}
	})
	if err := ginadapter.RegisterRoutes(router, &ginadapter.Options{Engine: engine}); err != nil {
		return nil, err
	}
	router.GET("/api/socket", ginadapter.SocketHandler(engine))
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan struct{})
	go func() {
		defer close(running)
		_ = engine.Run(ctx) // returns nil after Close
	}()
	application.Application = livewiretest.Application{Engine: engine, Handler: router, SocketPath: "/api/socket"}
	application.stop = func() {
		closing, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = engine.Close(closing) // the application is done; a slow close only delays the test's end
		cancel()
		<-running
	}
	application.feed.waitConnected()
	return application, nil
}

func (a *notesApplication) close() { a.stop() }

func (a *notesApplication) registry() (*livewire.Registry, error) {
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{
		BasePath: "/api",
		Tenant: func(ctx context.Context, _ any) (string, error) {
			return tenantOf[auth.IdentityFrom(ctx).Subject], nil
		},
	})
	if err != nil {
		return nil, err
	}
	summaryTables := a.broken.summaryTables
	if summaryTables == nil {
		summaryTables = []string{"notes", "tags"}
	}
	declarations := []livewire.Declaration{
		livewire.NewResource("notes", "/notes", &livewire.ResourceOptions[note]{
			Tables: []string{"notes"},
			Read:   authorization.IsUser(),
			Fields: query.Fields{
				{Name: "id", Type: query.TypeUUID, SQL: "notes.id", Filter: true, Sort: true},
				{Name: "title", Type: query.TypeText, SQL: "notes.title", Filter: true, Sort: true},
				{Name: "version", Type: query.TypeInteger, SQL: "notes.version", Filter: true},
				{Name: "shelf", Type: query.TypeText, SQL: "notes.shelf", Filter: true}, // in SQL, not in the item
			},
			DefaultSort:  []query.SortTerm{{Field: "title"}},
			MaximumLimit: a.broken.pageSize,
			CountCap:     a.broken.countCap,
			Item:         &livewire.ItemKey{Parameter: "id", Field: "id"},
			Versioned:    true,
			List:         a.listNotes,
			Get:          a.getNote,
			Distinct:     a.distinctNotes,
			History:      a.noteHistory,
			Export:       a.exportNotes,
		}),
		livewire.NewList("tags", "/tags", &livewire.ListOptions[tag, livewire.NoParameters]{
			Tables: []string{"tags"}, Read: authorization.IsUser(),
			Load: func(ctx context.Context, _ *livewire.NoParameters) ([]tag, error) {
				a.mutex.Lock()
				defer a.mutex.Unlock()
				return slices.Clone(a.tags[livewire.TenantFrom(ctx)]), nil
			},
		}),
		livewire.NewObject("summary", "/summary", &livewire.ObjectOptions[summary, livewire.NoParameters]{
			Tables: summaryTables, Read: authorization.IsUser(),
			Load: func(ctx context.Context, _ *livewire.NoParameters) (*summary, error) {
				a.mutex.Lock()
				defer a.mutex.Unlock()
				tenant := livewire.TenantFrom(ctx)
				return &summary{Notes: len(a.notes[tenant]), Tags: len(a.tags[tenant])}, nil
			},
		}),
	}
	if a.broken.boardObject {
		declarations = append(declarations, livewire.NewObject("board", "/board", &livewire.ObjectOptions[board, livewire.NoParameters]{
			Tables: []string{"notes"}, Read: authorization.IsUser(),
			Load: func(ctx context.Context, _ *livewire.NoParameters) (*board, error) {
				a.mutex.Lock()
				defer a.mutex.Unlock()
				return &board{Items: []string{}, Limit: len(a.tags[livewire.TenantFrom(ctx)])}, nil
			},
		}))
	}
	if a.broken.sharedCategories {
		declarations = append(declarations, livewire.NewResource("categories", "/categories", &livewire.ResourceOptions[category]{
			Read:   authorization.IsUser(),
			Fields: query.Fields{{Name: "id", Type: query.TypeString, SQL: "categories.id", Filter: true}},
			Item:   &livewire.ItemKey{Parameter: "id", Field: "id"},
			List: func(context.Context, *query.ListQuery) (*livewire.Page[category], error) {
				return &livewire.Page[category]{Items: sharedCategories(), Total: 2}, nil
			},
			Get: func(_ context.Context, listQuery *query.ListQuery) (*category, error) {
				_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "categories", IDColumn: "categories.id"})
				for _, shared := range sharedCategories() {
					if slices.ContainsFunc(arguments, func(argument any) bool { return fmt.Sprint(argument) == shared.ID }) {
						return &shared, nil
					}
				}
				return nil, livewire.NotFound()
			},
		}))
	}
	if a.broken.formulaCodes {
		declarations = append(declarations, a.codes())
	}
	if a.broken.keylessMemos {
		declarations = append(declarations, livewire.NewResource("memos", "/memos", &livewire.ResourceOptions[memo]{
			Read:   authorization.IsUser(),
			Fields: query.Fields{{Name: "title", Type: query.TypeText, SQL: "memos.title", Filter: true}},
			List: func(context.Context, *query.ListQuery) (*livewire.Page[memo], error) {
				return &livewire.Page[memo]{Items: []memo{{Title: "a secret of either tenant"}}, Total: 1}, nil
			},
		}))
	}
	return registry, registry.Add(declarations...)
}

// category is reference data every tenant reads.
type category struct {
	ID string `json:"id"`
}

func sharedCategories() []category { return []category{{ID: "bolts"}, {ID: "nuts"}} }

// code is a row keyed by a text a spreadsheet would run, which an export writes quoted (D69).
type code struct {
	Code string `json:"code"`
}

// codes is a tenant's resource of codes keyed by "=<tenant>-<n>", with a CSV export that
// quotes them as postgresadapter.WriteCSV does.
func (a *notesApplication) codes() *livewire.Resource {
	codesOf := func(ctx context.Context) []code {
		tenant := livewire.TenantFrom(ctx)
		return []code{{Code: "=" + tenant + "-1"}, {Code: "=" + tenant + "-2"}}
	}
	return livewire.NewResource("codes", "/codes", &livewire.ResourceOptions[code]{
		Read:   authorization.IsUser(),
		Fields: query.Fields{{Name: "code", Type: query.TypeString, SQL: "codes.code", Filter: true}},
		Item:   &livewire.ItemKey{Parameter: "code", Field: "code"},
		List: func(ctx context.Context, _ *query.ListQuery) (*livewire.Page[code], error) {
			return &livewire.Page[code]{Items: codesOf(ctx), Total: 2}, nil
		},
		Get: func(ctx context.Context, listQuery *query.ListQuery) (*code, error) {
			_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "codes", IDColumn: "codes.id"})
			for _, own := range codesOf(ctx) {
				if slices.ContainsFunc(arguments, func(argument any) bool { return fmt.Sprint(argument) == own.Code }) {
					return &own, nil
				}
			}
			return nil, livewire.NotFound()
		},
		Export: func(ctx context.Context, _ *query.ListQuery, _ query.Fields, writer io.Writer) (int, error) {
			output := csv.NewWriter(writer)
			records := [][]string{{"code"}}
			for _, own := range codesOf(ctx) {
				records = append(records, []string{"'" + own.Code})
			}
			return len(records) - 1, output.WriteAll(records)
		},
	})
}

// notesOf are the notes the request's tenant reads, with every other tenant's after them when
// they leak.
func (a *notesApplication) notesOf(ctx context.Context, leaking bool) []note {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	tenant := livewire.TenantFrom(ctx)
	visible := slices.Clone(a.notes[tenant])
	if leaking {
		for _, other := range []string{"north", "south"} {
			if other != tenant {
				visible = append(visible, a.notes[other]...)
			}
		}
	}
	return visible
}

// foreignNotes are the notes of every other tenant than the request's.
func (a *notesApplication) foreignNotes(ctx context.Context) []note {
	all := a.notesOf(ctx, true)
	return all[len(a.notesOf(ctx, false)):]
}

func (a *notesApplication) listNotes(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[note], error) {
	visible := a.notesOf(ctx, a.broken.leakingTenants || a.broken.leakingPastCap)
	if a.broken.unstableNotes {
		for index := range visible {
			visible[index].Title += " #" + strconv.FormatInt(a.reads.Add(1), 10)
		}
	}
	total := len(visible)
	if a.broken.countCap > 0 {
		total = min(total, a.broken.countCap+1) // D14: the total is at most cap+1
	}
	start := min(listQuery.Offset, len(visible))
	return &livewire.Page[note]{Items: visible[start:min(len(visible), start+listQuery.Limit)], Total: total}, nil
}

// keyedNote is the note a loader's query names by its id, among the candidates.
func keyedNote(listQuery *query.ListQuery, candidates []note) (*note, bool) {
	_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "notes", IDColumn: "notes.id"})
	for _, candidate := range candidates {
		if slices.ContainsFunc(arguments, func(argument any) bool { return fmt.Sprint(argument) == candidate.ID.String() }) {
			return &candidate, true
		}
	}
	return nil, false
}

func (a *notesApplication) getNote(ctx context.Context, listQuery *query.ListQuery) (*note, error) {
	candidates := a.notesOf(ctx, a.broken.leakingTenants)
	if from := a.broken.leakingItemsFrom; from > 0 {
		candidates = append(candidates, a.foreignNotes(ctx)[min(from, len(a.foreignNotes(ctx))):]...)
	}
	if found, ok := keyedNote(listQuery, candidates); ok {
		return found, nil
	}
	return nil, livewire.NotFound()
}

// noteHistory answers one version per note: the keyed one's, or every note's when it leaks.
func (a *notesApplication) noteHistory(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[note]], error) {
	var versions []livewire.Version[note]
	for _, candidate := range a.notesOf(ctx, true) {
		_, keyed := keyedNote(listQuery, []note{candidate})
		if keyed || a.broken.leakingHistory {
			versions = append(versions, livewire.Version[note]{ID: len(versions) + 1, Operation: livewire.ChangeInsert, ValidFrom: time.Unix(1, 0).UTC(), Data: &candidate})
		}
	}
	return &livewire.Page[livewire.Version[note]]{Items: versions, Total: len(versions)}, nil
}

func (a *notesApplication) distinctNotes(ctx context.Context, field string, _ *query.ListQuery) ([]livewire.DistinctValue, error) {
	counts := map[any]int{}
	for _, visible := range a.notesOf(ctx, a.broken.leakingDistinct) {
		switch field {
		case "id":
			counts[visible.ID.String()]++
		case "version":
			counts[visible.Version]++
		case "shelf": // filterable in SQL, not in the item
			counts["shelf of "+livewire.TenantFrom(ctx)]++
		default:
			counts[visible.Title]++
		}
	}
	if a.broken.respelledDistinct && field == "version" {
		counts[json.Number("1.0")] = len(a.foreignNotes(ctx)) // the other tenant's ones, spelled apart
	}
	values := make([]livewire.DistinctValue, 0, len(counts))
	for value, count := range counts {
		values = append(values, livewire.DistinctValue{Value: value, Count: count})
	}
	slices.SortFunc(values, func(left, right livewire.DistinctValue) int {
		return strings.Compare(fmt.Sprint(left.Value), fmt.Sprint(right.Value))
	})
	return values, nil
}

func (a *notesApplication) exportNotes(ctx context.Context, _ *query.ListQuery, _ query.Fields, writer io.Writer) (int, error) {
	output := csv.NewWriter(writer)
	visible := a.notesOf(ctx, a.broken.leakingTenants)
	header := []string{"id", "title"}
	if a.broken.exportWithoutKey {
		header = header[1:]
	}
	if err := output.Write(header); err != nil {
		return 0, err
	}
	for _, exported := range visible {
		record := []string{exported.ID.String(), exported.Title}
		if err := output.Write(record[len(record)-len(header):]); err != nil {
			return 0, err
		}
	}
	output.Flush()
	return len(visible), output.Error()
}

// firstNote is the id of the tenant's first note.
func (a *notesApplication) firstNote(tenant string) string {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.notes[tenant][0].ID.String()
}

// addNote and addTag are the committed writes of the fixture: the row, then its change.
func (a *notesApplication) addNote(tenant, title string) livewiretest.Write {
	return livewiretest.Write{Table: "notes", Commit: func(context.Context) error {
		added := note{ID: uuid.New(), Title: title, Version: 1}
		a.mutex.Lock()
		a.notes[tenant] = append(a.notes[tenant], added)
		a.mutex.Unlock()
		a.feed.publish(livewire.Change{Table: "notes", Operation: livewire.ChangeInsert, ID: added.ID.String()})
		return nil
	}}
}

func (a *notesApplication) addTag(tenant, name string) livewiretest.Write {
	return livewiretest.Write{Table: "tags", Commit: func(context.Context) error {
		added := tag{ID: uuid.New(), Name: name}
		a.mutex.Lock()
		a.tags[tenant] = append(a.tags[tenant], added)
		a.mutex.Unlock()
		a.feed.publish(livewire.Change{Table: "tags", Operation: livewire.ChangeInsert, ID: added.ID.String()})
		return nil
	}}
}

// as authenticates a request as the user.
func as(user string) func(request *http.Request) {
	return func(request *http.Request) { request.Header.Set(userHeader, user) }
}

func identityOf(request *http.Request) *auth.Identity {
	user := request.Header.Get(userHeader)
	if _, known := tenantOf[user]; !known {
		return nil
	}
	return &auth.Identity{Provider: "test", Subject: user, Active: true}
}

// headerIdentity re-validates a socket by the user of its upgrade request.
type headerIdentity struct{}

func (headerIdentity) Identify(_ context.Context, request *http.Request) (*auth.Identity, error) {
	if identity := identityOf(request); identity != nil {
		return identity, nil
	}
	return nil, auth.ErrUnauthenticated
}

// transactor begins transactions that hold nothing: the data lives in memory.
type transactor struct{}

func (transactor) Begin(context.Context, livewire.TransactionDetails) (livewire.Transaction, error) {
	return transaction{}, nil
}

type transaction struct{}

func (transaction) Commit(context.Context) error   { return nil }
func (transaction) Rollback(context.Context) error { return nil }
func (transaction) Handle() any                    { return struct{}{} }

// feed is the change feed the writes publish on.
type feed struct {
	mutex     sync.Mutex
	onChange  func(livewire.Change)
	connected chan struct{}
	once      sync.Once
}

func (f *feed) Run(ctx context.Context, onChange func(livewire.Change), onResync func()) error {
	f.mutex.Lock()
	f.onChange = onChange
	f.mutex.Unlock()
	onResync()
	f.once.Do(func() { close(f.connected) })
	<-ctx.Done()
	return ctx.Err()
}

func (f *feed) waitConnected() { <-f.connected }

func (f *feed) publish(change livewire.Change) {
	f.mutex.Lock()
	onChange := f.onChange
	f.mutex.Unlock()
	onChange(change)
}

// recordingT stands in for the application test's testing.TB: it records the gate's failures
// instead of failing, so a test can require that a gate fails a broken application.
type recordingT struct {
	testing.TB // never called: the gates call only the methods below
	mutex      sync.Mutex
	failures   []string
}

func (r *recordingT) Helper() {}

func (r *recordingT) Errorf(format string, arguments ...any) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, arguments...))
}

func (r *recordingT) Fatalf(format string, arguments ...any) {
	r.Errorf(format, arguments...)
	runtime.Goexit()
}

// gateFailures runs a gate on a goroutine of its own, as the testing package runs a test, so a
// Fatalf ends that goroutine only, and answers what the gate failed with.
func gateFailures(gate func(t testing.TB)) []string {
	recorder := &recordingT{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		gate(recorder)
	}()
	<-done
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return recorder.failures
}

// verdict is what an example prints: passed, or the failures.
func verdict(failures []string) string {
	if len(failures) == 0 {
		return "passed"
	}
	return strings.Join(failures, "\n")
}
