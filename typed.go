package livewire

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"

	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire/query"
)

// ListOptions declares a typed-parameters list. Read and Load are required; Guarded names
// the fields of Item whose Read check strips them (D39); a live list (Tables set) needs an
// item with a JSON field "id". Partition names a subscription's partition (D25); nil means
// the tenant.
type ListOptions[Item, Parameters any] struct {
	Tables    []string
	Read      authorization.Check
	Partition PartitionResolver
	Guarded   []GuardedField
	Load      func(ctx context.Context, parameters *Parameters) ([]Item, error)
}

// List is a read that answers a ListResponse of every item its loader returns for the
// typed parameters; it has no filter, sort or page of its own.
type List struct {
	signature
	itemType reflect.Type
	guarded  query.Fields
	load     func(ctx context.Context, parameters any) (any, error)
}

// NewList declares a typed-parameters list of Item at path, whose `{name}` placeholders are
// the `path:"name"` fields of Parameters. The options are copied. A mistake is reported by
// Registry.Add (D43).
func NewList[Item, Parameters any](name, path string, options *ListOptions[Item, Parameters]) *List {
	if options == nil {
		options = &ListOptions[Item, Parameters]{}
	}
	list := &List{
		signature: newSignature(name, http.MethodGet, path, options.Tables, options.Read),
		itemType:  reflect.TypeFor[Item](),
		guarded:   guardedFields(options.Guarded),
	}
	list.partition = options.Partition
	if load := options.Load; load != nil {
		list.load = func(ctx context.Context, parameters any) (any, error) {
			items, err := load(ctx, parameters.(*Parameters))
			if items == nil {
				items = []Item{}
			}
			return items, err
		}
	}
	list.compileParameters(reflect.TypeFor[Parameters]())
	list.failWith(list.validate(options.Guarded))
	return list
}

// Kind is KindList.
func (l *List) Kind() Kind { return KindList }

// ItemType is the Go type of one item.
func (l *List) ItemType() reflect.Type { return l.itemType }

// Fields are the guarded fields (name and Read check only) for RedactItems; the result is
// the caller's copy.
func (l *List) Fields() query.Fields { return slices.Clone(l.guarded) }

// Load runs the loader with the decoded parameters (a *Parameters); the items are a []Item,
// never nil.
func (l *List) Load(ctx context.Context, parameters any) (any, error) {
	return l.load(ctx, parameters)
}

func (l *List) declaration() Declaration { return l }

func (l *List) routes() []route { return []route{{http.MethodGet, l.path}} }

func (l *List) validate(guarded []GuardedField) error {
	if l.load == nil {
		return errors.New("Load is required")
	}
	if err := validateGuards(l.itemType, guarded); err != nil {
		return err
	}
	if len(l.tables) > 0 {
		return requireJSONField(l.itemType, keyFieldJSON, liveListNeedsID)
	}
	return nil
}

// ObjectOptions declares a typed-parameters object. Read and Load are required; Guarded
// names the fields of Item whose Read check strips them (D39). Partition names a
// subscription's partition (D25); nil means the tenant.
type ObjectOptions[Item, Parameters any] struct {
	Tables    []string
	Read      authorization.Check
	Partition PartitionResolver
	Guarded   []GuardedField
	Load      func(ctx context.Context, parameters *Parameters) (*Item, error)
}

// Object is a read that answers one item for the typed parameters.
type Object struct {
	signature
	itemType reflect.Type
	guarded  query.Fields
	load     func(ctx context.Context, parameters any) (any, error)
}

// NewObject declares a typed-parameters object of Item at path, whose `{name}` placeholders
// are the `path:"name"` fields of Parameters. The options are copied. A mistake is reported by
// Registry.Add (D43).
func NewObject[Item, Parameters any](name, path string, options *ObjectOptions[Item, Parameters]) *Object {
	if options == nil {
		options = &ObjectOptions[Item, Parameters]{}
	}
	object := &Object{
		signature: newSignature(name, http.MethodGet, path, options.Tables, options.Read),
		itemType:  reflect.TypeFor[Item](),
		guarded:   guardedFields(options.Guarded),
	}
	object.partition = options.Partition
	if load := options.Load; load != nil {
		object.load = func(ctx context.Context, parameters any) (any, error) {
			item, err := load(ctx, parameters.(*Parameters))
			if err != nil {
				return nil, err
			}
			if item == nil {
				return nil, fmt.Errorf("livewire: the loader of %q returned no item and no error", name)
			}
			return item, nil
		}
	}
	object.compileParameters(reflect.TypeFor[Parameters]())
	if object.load == nil {
		object.fail("Load is required")
	}
	object.failWith(validateGuards(object.itemType, options.Guarded))
	return object
}

// Kind is KindObject.
func (o *Object) Kind() Kind { return KindObject }

// ItemType is the Go type of the item.
func (o *Object) ItemType() reflect.Type { return o.itemType }

// Fields are the guarded fields (name and Read check only) for RedactItem; the result is
// the caller's copy.
func (o *Object) Fields() query.Fields { return slices.Clone(o.guarded) }

// Load runs the loader with the decoded parameters (a *Parameters); the item is a *Item. A
// loader that finds nothing returns NotFound.
func (o *Object) Load(ctx context.Context, parameters any) (any, error) {
	return o.load(ctx, parameters)
}

func (o *Object) declaration() Declaration { return o }

func (o *Object) routes() []route { return []route{{http.MethodGet, o.path}} }

func validateGuards(itemType reflect.Type, guarded []GuardedField) error {
	seen := map[string]bool{}
	for _, guard := range guarded {
		switch {
		case guard.Name == "" || guard.Read == nil:
			return errors.New("a guarded field needs Name and Read")
		case seen[guard.Name]:
			return fmt.Errorf("the field %q is guarded twice", guard.Name)
		}
		seen[guard.Name] = true
		if err := requireJSONField(itemType, guard.Name, "Guarded names it"); err != nil {
			return err
		}
	}
	return nil
}
