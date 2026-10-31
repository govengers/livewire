package livewire

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"

	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire/query"
)

// ListOptions declares a typed-parameters list. Read and Load are required; Guarded names
// the fields of Item whose Read check strips them (D39); a live list (Tables set) needs an
// item with a JSON field "id".
type ListOptions[Item, Params any] struct {
	Tables  []string
	Read    authorization.Check
	Guarded []GuardedField
	Load    func(ctx context.Context, parameters *Params) ([]Item, error)
}

// List is a read that answers a ListResponse of every item its loader returns for the
// typed parameters; it has no filter, sort or page of its own.
type List struct {
	Signature
	itemType reflect.Type
	guarded  query.Fields
	load     func(ctx context.Context, parameters any) (any, error)
}

// NewList declares a typed-parameters list of Item at path, whose `{name}` placeholders are
// the `path:"name"` fields of Params. A mistake is reported by Registry.Add.
func NewList[Item, Params any](name, path string, options *ListOptions[Item, Params]) *List {
	if options == nil {
		options = &ListOptions[Item, Params]{}
	}
	list := &List{
		Signature: newSignature(name, http.MethodGet, path, options.Tables, options.Read),
		itemType:  reflect.TypeFor[Item](),
		guarded:   guardedFields(options.Guarded),
	}
	if options.Load != nil {
		list.load = func(ctx context.Context, parameters any) (any, error) {
			items, err := options.Load(ctx, parameters.(*Params))
			if items == nil {
				items = []Item{}
			}
			return items, err
		}
	}
	list.compileParameters(reflect.TypeFor[Params]())
	if options.Load == nil {
		list.fail("Load is required")
	}
	list.failWith(validateGuards(list.itemType, options.Guarded))
	if len(list.tables) > 0 {
		list.failWith(requireJSONField(list.itemType, keyFieldJSON, liveListNeedsID))
	}
	return list
}

// Kind is KindList.
func (l *List) Kind() Kind { return KindList }

// ItemType is the Go type of one item.
func (l *List) ItemType() reflect.Type { return l.itemType }

// Fields are the guarded fields (name and Read check only) for Redact; the result is the
// caller's copy.
func (l *List) Fields() query.Fields { return slices.Clone(l.guarded) }

func (l *List) routes() []route { return []route{{http.MethodGet, l.path}} }

// Load runs the loader with the decoded parameters (a *Params); the items are a []Item,
// never nil.
func (l *List) Load(ctx context.Context, parameters any) (any, error) {
	return l.load(ctx, parameters)
}

// ObjectOptions declares a typed-parameters object. Read and Load are required; Guarded
// names the fields of Item whose Read check strips them (D39).
type ObjectOptions[Item, Params any] struct {
	Tables  []string
	Read    authorization.Check
	Guarded []GuardedField
	Load    func(ctx context.Context, parameters *Params) (*Item, error)
}

// Object is a read that answers one item for the typed parameters.
type Object struct {
	Signature
	itemType reflect.Type
	guarded  query.Fields
	load     func(ctx context.Context, parameters any) (any, error)
}

// NewObject declares a typed-parameters object of Item at path, whose `{name}` placeholders
// are the `path:"name"` fields of Params. A mistake is reported by Registry.Add.
func NewObject[Item, Params any](name, path string, options *ObjectOptions[Item, Params]) *Object {
	if options == nil {
		options = &ObjectOptions[Item, Params]{}
	}
	object := &Object{
		Signature: newSignature(name, http.MethodGet, path, options.Tables, options.Read),
		itemType:  reflect.TypeFor[Item](),
		guarded:   guardedFields(options.Guarded),
	}
	if options.Load != nil {
		object.load = func(ctx context.Context, parameters any) (any, error) {
			item, err := options.Load(ctx, parameters.(*Params))
			if err != nil {
				return nil, err
			}
			if item == nil {
				return nil, fmt.Errorf("livewire: the loader of %q returned no item and no error", name)
			}
			return item, nil
		}
	}
	object.compileParameters(reflect.TypeFor[Params]())
	if options.Load == nil {
		object.fail("Load is required")
	}
	object.failWith(validateGuards(object.itemType, options.Guarded))
	return object
}

// Kind is KindObject.
func (o *Object) Kind() Kind { return KindObject }

// ItemType is the Go type of the item.
func (o *Object) ItemType() reflect.Type { return o.itemType }

// Fields are the guarded fields (name and Read check only) for Redact; the result is the
// caller's copy.
func (o *Object) Fields() query.Fields { return slices.Clone(o.guarded) }

func (o *Object) routes() []route { return []route{{http.MethodGet, o.path}} }

// Load runs the loader with the decoded parameters (a *Params); the item is a *Item. A
// loader that finds nothing returns NotFound.
func (o *Object) Load(ctx context.Context, parameters any) (any, error) {
	return o.load(ctx, parameters)
}

func validateGuards(itemType reflect.Type, guarded []GuardedField) error {
	seen := map[string]bool{}
	for _, guard := range guarded {
		switch {
		case guard.Name == "" || guard.Read == nil:
			return fmt.Errorf("a guarded field needs Name and Read")
		case seen[guard.Name]:
			return fmt.Errorf("the field %q is guarded twice", guard.Name)
		}
		seen[guard.Name] = true
		if err := requireJSONField(itemType, guard.Name, "Guarded names"); err != nil {
			return err
		}
	}
	return nil
}
