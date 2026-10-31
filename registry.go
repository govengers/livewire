package livewire

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// RegistryOptions configures the registry. BasePath is the prefix of every REST route
// ("/api/v1"; empty for none) and must start with a slash and not end with one. Tenant
// resolves the tenant of a request from the decoded parameters (a *Params, nil for a
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
	if resolved.BasePath != "" && (!strings.HasPrefix(resolved.BasePath, "/") || strings.HasSuffix(resolved.BasePath, "/")) {
		return nil, fmt.Errorf("livewire: BasePath %q must start with a slash and not end with one", resolved.BasePath)
	}
	return &Registry{
		options: resolved,
		byName:  map[string]Declaration{},
		byRoute: map[route]Declaration{},
	}, nil
}

// Add registers declarations in order. It returns the first mistake it finds: a declaration
// that failed its own validation, a duplicate name, a duplicate method and path (a
// resource's item and distinct routes count), a mutation whose Resource or Effect target is
// not registered by the end of the call. Nothing is added when any declaration fails, so
// main fails fast on a wiring mistake.
func (r *Registry) Add(declarations ...Declaration) error {
	staged := &Registry{
		options: r.options,
		byName:  maps.Clone(r.byName),
		byRoute: maps.Clone(r.byRoute),
	}
	staged.declarations = slices.Clone(r.declarations)
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
	r.declarations, r.byName, r.byRoute = staged.declarations, staged.byName, staged.byRoute
	return nil
}

func (r *Registry) stage(declaration Declaration) error {
	name := declaration.Name()
	if err := declaration.signature().err; err != nil {
		return fmt.Errorf("livewire: %s %q: %w", declaration.Kind(), name, err)
	}
	if _, exists := r.byName[name]; exists {
		return fmt.Errorf("livewire: %s %q: the name is already registered", declaration.Kind(), name)
	}
	for _, declared := range declaration.routes() {
		if other, exists := r.byRoute[declared]; exists {
			return fmt.Errorf("livewire: %s %q: the route %s is already registered by %q", declaration.Kind(), name, declared, other.Name())
		}
		r.byRoute[declared] = declaration
	}
	r.byName[name] = declaration
	r.declarations = append(r.declarations, declaration)
	return nil
}

func (r *Registry) bind(declaration Declaration) error {
	mutation, ok := declaration.(*Mutation)
	if !ok {
		return nil
	}
	if mutation.resource != "" {
		resource, ok := r.byName[mutation.resource].(*Resource)
		if !ok {
			return fmt.Errorf("livewire: mutation %q: Resource %q is not a registered resource", mutation.name, mutation.resource)
		}
		mutation.bindResource(resource.fields)
	}
	if mutation.effect.Kind != "" {
		switch r.byName[mutation.effect.Target].(type) {
		case *Resource, *List:
		default:
			return fmt.Errorf("livewire: mutation %q: Effect target %q is not a registered list", mutation.name, mutation.effect.Target)
		}
	}
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
