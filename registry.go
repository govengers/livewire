package livewire

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/govengers/auth/authorization"
)

// RegistryOptions configures the registry. BasePath is the prefix of every REST route
// ("/api/v1"; empty for none) and must start with a slash and not end with one. Tenant
// resolves the tenant of a request from the decoded parameters (a *Parameters, nil for a
// resource read) and the identity in ctx (D42); nil means one tenant. Observer receives the
// outcomes of the engine and the adapters; nil means none.
type RegistryOptions struct {
	BasePath string
	Tenant   func(ctx context.Context, parameters any) (string, error)
	Observer Observer
}

// Registry is the single source of truth of an API: every declaration by name, in
// declaration order. Build it in main with Add; it is read-only afterwards and safe for
// concurrent reads.
type Registry struct {
	options      RegistryOptions
	declarations []Declaration
	byName       map[string]Declaration
	byRoute      map[route]Declaration
}

// NewRegistry validates and copies options. A nil options value is the defaults.
func NewRegistry(options *RegistryOptions) (*Registry, error) {
	resolved := RegistryOptions{}
	if options != nil {
		resolved = *options
	}
	if !isBasePath(resolved.BasePath) {
		return nil, fmt.Errorf("livewire: BasePath %q must start with a slash and not end with one", resolved.BasePath)
	}
	return &Registry{
		options: resolved,
		byName:  map[string]Declaration{},
		byRoute: map[route]Declaration{},
	}, nil
}

// Add registers declarations in order. It returns the first mistake it finds: a nil or
// wrapped declaration, one that failed its own validation or belongs to another registry, the
// reserved names "batch" and "filters" (the targets of a batch's and the stored filter
// routes' outcomes, D33, D59), a duplicate name, a
// duplicate method and route pattern (placeholder names aside; a resource's item and
// distinct routes count), a mutation whose Resource or Effect target is not registered by
// the end of the call, a route of the library's own (BatchPath, FiltersPath). Nothing is added and no declaration is changed when any
// declaration fails, so main fails fast on a wiring mistake.
func (r *Registry) Add(declarations ...Declaration) error {
	staged := &staging{
		registry:     r,
		declarations: slices.Clone(r.declarations),
		byName:       maps.Clone(r.byName),
		byRoute:      maps.Clone(r.byRoute),
		writeChecks:  map[*Mutation]map[string]authorization.Check{},
	}
	for _, declaration := range declarations {
		if err := staged.stage(declaration); err != nil {
			return err
		}
	}
	for _, declaration := range declarations {
		if err := staged.bind(declaration); err != nil {
			return err
		}
	}
	staged.publish()
	return nil
}

// Lookup returns the declaration with the given name.
func (r *Registry) Lookup(name string) (Declaration, bool) {
	declaration, ok := r.byName[name]
	return declaration, ok
}

// Declarations returns every declaration in declaration order; the result is the caller's
// copy.
func (r *Registry) Declarations() []Declaration {
	return slices.Clone(r.declarations)
}

// BasePath is the prefix of every REST route, "" for none.
func (r *Registry) BasePath() string { return r.options.BasePath }

// Tenant resolves the tenant of a request (D42): "" and no error with a single tenant.
func (r *Registry) Tenant(ctx context.Context, parameters any) (string, error) {
	if r.options.Tenant == nil {
		return "", nil
	}
	tenant, err := r.options.Tenant(ctx, parameters)
	if err != nil {
		return "", err
	}
	if tenant == "" {
		return "", errors.New("livewire: the Tenant function returned no tenant and no error")
	}
	return tenant, nil
}

// Observe reports an outcome to the Observer; it does nothing without one.
func (r *Registry) Observe(ctx context.Context, outcome Outcome) {
	if r.options.Observer != nil {
		r.options.Observer(ctx, outcome)
	}
}

func isBasePath(basePath string) bool {
	return basePath == "" || (strings.HasPrefix(basePath, "/") && !strings.HasSuffix(basePath, "/"))
}

// staging is one Add in progress: the registry's indexes plus the new declarations, and the
// bindings computed for them, published together once every check passed.
type staging struct {
	registry     *Registry
	declarations []Declaration
	byName       map[string]Declaration
	byRoute      map[route]Declaration
	writeChecks  map[*Mutation]map[string]authorization.Check
}

func (s *staging) stage(declaration Declaration) error {
	if isNilDeclaration(declaration) {
		return errors.New("livewire: a nil declaration was added")
	}
	if unwrapped, ok := unwrapDeclaration(declaration); !ok || unwrapped != declaration {
		return fmt.Errorf("livewire: %T wraps a declaration; add the declaration itself", declaration)
	}
	name, kind := declaration.Name(), declaration.Kind()
	shared := declaration.shared()
	switch {
	case shared.err != nil:
		return fmt.Errorf("livewire: %s %q: %w", kind, name, shared.err)
	case shared.registry != nil && shared.registry != s.registry:
		return fmt.Errorf("livewire: %s %q is registered with another registry", kind, name)
	case name == batchMutation || name == filtersTarget:
		return fmt.Errorf("livewire: %s %q: the name is reserved: the outcomes of a batch and of the stored filter routes carry it as their target (D33, D59)", kind, name)
	}
	if _, exists := s.byName[name]; exists {
		return fmt.Errorf("livewire: %s %q: the name is already registered", kind, name)
	}
	for _, declaredRoute := range declaration.routes() {
		pattern := declaredRoute.pattern()
		if reservedRoutes[pattern] {
			return fmt.Errorf("livewire: %s %q: the route %s is the library's own (BatchPath, FiltersPath)", kind, name, declaredRoute)
		}
		if other, exists := s.byRoute[pattern]; exists {
			return fmt.Errorf("livewire: %s %q: the route %s is already registered by %q", kind, name, declaredRoute, other.Name())
		}
		s.byRoute[pattern] = declaration
	}
	s.byName[name] = declaration
	s.declarations = append(s.declarations, declaration)
	return nil
}

// unwrapDeclaration asks the declaration for itself; a wrapper around a nil declaration
// panics in the promoted call, which is reported as not a declaration.
func unwrapDeclaration(declaration Declaration) (unwrapped Declaration, ok bool) {
	defer func() {
		if recover() != nil {
			unwrapped, ok = nil, false
		}
	}()
	return declaration.declaration(), true
}

func (s *staging) bind(declaration Declaration) error {
	mutation, ok := declaration.(*Mutation)
	if !ok {
		return nil
	}
	if mutation.resource != "" {
		resource, ok := s.byName[mutation.resource].(*Resource)
		if !ok {
			return fmt.Errorf("livewire: mutation %q: Resource %q is not a registered resource", mutation.name, mutation.resource)
		}
		s.writeChecks[mutation] = mutation.writeChecksFor(resource.fields)
	}
	return s.bindEffect(mutation)
}

// bindEffect checks the optimistic effect (D36) against the registry: the target is a live
// list, Upserts has a response to upsert, Removes has the `id` path parameter to remove by.
func (s *staging) bindEffect(mutation *Mutation) error {
	effect := mutation.effect
	if effect.Kind == "" {
		return nil
	}
	target, ok := s.byName[effect.Target]
	switch target.(type) {
	case *Resource, *List:
	default:
		return fmt.Errorf("livewire: mutation %q: Effect target %q is not a registered list", mutation.name, effect.Target)
	}
	liveTarget := ok && target.shared().live()
	switch {
	case !liveTarget:
		return fmt.Errorf("livewire: mutation %q: Effect target %q is not live (no Tables)", mutation.name, effect.Target)
	case effect.Kind == EffectUpserts && !mutation.HasContent():
		return fmt.Errorf("livewire: mutation %q: Upserts needs a response to upsert, not NoContent", mutation.name)
	case effect.Kind == EffectRemoves && !mutation.hasPathParameter(keyFieldJSON):
		return fmt.Errorf("livewire: mutation %q: Removes needs the path parameter %q to remove by", mutation.name, keyFieldJSON)
	}
	if effect.Kind == EffectUpserts {
		if err := requireJSONField(mutation.responseType, keyFieldJSON, "Upserts keys the optimistic item by it"); err != nil {
			return fmt.Errorf("livewire: mutation %q: %w", mutation.name, err)
		}
	}
	return nil
}

func (s *staging) publish() {
	for mutation, checks := range s.writeChecks {
		mutation.writeChecks = checks
	}
	for _, declaration := range s.declarations {
		declaration.shared().registry = s.registry
	}
	s.registry.declarations, s.registry.byName, s.registry.byRoute = s.declarations, s.byName, s.byRoute
}

func isNilDeclaration(declaration Declaration) bool {
	if declaration == nil {
		return true
	}
	value := reflect.ValueOf(declaration)
	return value.Kind() == reflect.Pointer && value.IsNil()
}
