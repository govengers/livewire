package generate

import (
	"reflect"
	"strings"

	"github.com/govengers/livewire"
)

// The header names the generated clients send and read, for the test that pins them to the
// REST adapter's.
const (
	IdempotencyHeader = idempotencyHeader
	ExportRowsTrailer = exportRowsTrailer
)

// parityItem stands for the item of the core's generic wire types.
type parityItem struct {
	Marker string `json:"marker"`
}

// parityEntry is one wire type of the core, the runtime declaration it must equal and the
// members its doc comment says are never null (encoding/json would allow null).
type parityEntry struct {
	name      string
	goType    reflect.Type
	runtime   string
	neverNull []string
}

var parityEntries = []parityEntry{
	{"ErrorBody", reflect.TypeFor[livewire.ErrorBody](), "runtime.ErrorBody", nil},
	{"ListResponse", reflect.TypeFor[livewire.ListResponse[parityItem]](), "runtime.ListResponse<ParityItem>", []string{"items"}},
	{"ListChange", reflect.TypeFor[livewire.ListChange](), "runtime.ListChange<unknown>", []string{"upserts", "removes"}},
	{"Version", reflect.TypeFor[livewire.Version[parityItem]](), "runtime.Version<ParityItem>", nil},
	{"DistinctValue", reflect.TypeFor[livewire.DistinctValue](), "runtime.DistinctValue", nil},
	{"PresenceItem", reflect.TypeFor[livewire.PresenceItem](), "runtime.PresenceItem", nil},
	{"FilterItem", reflect.TypeFor[livewire.FilterItem](), "runtime.FilterItem", nil},
	{"FilterRequest", reflect.TypeFor[livewire.FilterRequest](), "runtime.FilterRequest", nil},
	{"BatchCall", reflect.TypeFor[livewire.BatchCall](), "runtime.BatchCall", nil},
	{"BatchResult", reflect.TypeFor[livewire.BatchResult](), "runtime.BatchResult", nil},
	{"RequestFrame", reflect.TypeFor[livewire.RequestFrame](), "runtime.RequestFrame", nil},
	{"ResponseFrame", reflect.TypeFor[livewire.ResponseFrame](), "runtime.ResponseFrame", nil},
	{"PushFrame", reflect.TypeFor[livewire.PushFrame](), "runtime.PushFrame", nil},
}

// LibraryParityTypeScript renders the core's wire types from their Go types, mapped as the
// generator maps DTOs, and asserts in TypeScript that each is the runtime's hand-written
// declaration both ways (members, their types, optionality and nullability); importPath is
// where runtime.ts is. Its @ts-expect-error lines prove the check is no tautology.
func LibraryParityTypeScript(importPath string) (string, error) {
	set := newTypeSet(nil)
	set.enums[reflect.TypeFor[livewire.Operation]()] = stringsOf([]livewire.Operation{
		livewire.OperationQuery, livewire.OperationSubscribe, livewire.OperationUnsubscribe, livewire.OperationCall,
		livewire.OperationBatch, livewire.OperationJoin, livewire.OperationLeave, livewire.OperationPing,
	})
	set.enums[reflect.TypeFor[livewire.PushKind]()] = stringsOf([]livewire.PushKind{livewire.PushSnapshot, livewire.PushListChange, livewire.PushReplace, livewire.PushError})
	if err := set.describeLibrary(); err != nil {
		return "", err
	}
	definitions := map[string]*shape{}
	for _, entry := range parityEntries {
		value, err := set.shapeOf(entry.goType, use{})
		if err != nil {
			return "", err
		}
		definition := value.reference.definition
		for position, member := range definition.properties {
			for _, name := range entry.neverNull {
				if member.name == name {
					definition.properties[position].shape = member.shape.withNullable(false)
				}
			}
		}
		definitions[entry.name] = definition
	}
	if err := set.assignNames(); err != nil {
		return "", err
	}
	names := typescriptNames{libraries: "runtime."}
	var builder strings.Builder
	builder.WriteString("import type * as runtime from '" + importPath + "'\n\n")
	builder.WriteString("type Exact<Left, Right> = [Left] extends [Right] ? ([Right] extends [Left] ? true : false) : false\n\n")
	for _, named := range set.ordered {
		if !named.library {
			builder.WriteString("export type " + named.name + " = " + names.expression(named.definition) + "\n")
		}
	}
	for _, entry := range parityEntries {
		builder.WriteString("export type Go" + entry.name + " = " + names.expression(definitions[entry.name]) + "\n")
		builder.WriteString("export const " + strings.ToLower(entry.name[:1]) + entry.name[1:] + ": Exact<Go" + entry.name + ", " + entry.runtime + "> = true\n")
	}
	builder.WriteString(`
// @ts-expect-error a member of another type is no match
export const wrongType: Exact<Omit<GoVersion, 'actor'> & { actor: number }, runtime.Version<ParityItem>> = true
// @ts-expect-error a required member is no optional one
export const wrongOptionality: Exact<Omit<GoErrorBody, 'fields'> & { fields: Record<string, string> }, runtime.ErrorBody> = true
// @ts-expect-error a nullable member is no plain one
export const wrongNullability: Exact<Omit<GoVersion, 'validTo'> & { validTo: string }, runtime.Version<ParityItem>> = true
// @ts-expect-error a missing member counts
export const missingMember: Exact<Omit<GoPushFrame, 'seq'>, runtime.PushFrame> = true
`)
	return builder.String(), nil
}
