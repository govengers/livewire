package generate

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/generate/internal/fixture"
	"github.com/govengers/livewire/generate/internal/fixture/billing"
)

func TestGoExpressionsNameEveryTypeArgument(t *testing.T) {
	file := newGoFile("github.com/govengers/livewire/generate/internal/application", []string{"Client"})
	testCases := []struct {
		goType   reflect.Type
		expected string
	}{
		{reflect.TypeFor[fixture.Page[map[string]*fixture.Node]](), "fixture.Page[map[string]*fixture.Node]"},
		{reflect.TypeFor[fixture.Page[[]billing.Part]](), "fixture.Page[[]billing.Part]"},
		{reflect.TypeFor[fixture.Page[[2]int]](), "fixture.Page[[2]int]"},
		{reflect.TypeFor[fixture.Page[fixture.Page[string]]](), "fixture.Page[fixture.Page[string]]"},
		{reflect.TypeFor[fixture.Page[any]](), "fixture.Page[any]"},
		{reflect.TypeFor[map[string][]*billing.Record](), "map[string][]*billing.Record"},
		{reflect.TypeFor[[3]any](), "[3]any"},
	}
	for _, testCase := range testCases {
		expression, err := file.expression(testCase.goType)
		require.NoError(t, err, testCase.goType.String())
		require.Equal(t, testCase.expected, expression)
	}
	require.Equal(t, "import (\n\t\"github.com/govengers/livewire/generate/internal/fixture\"\n\t\"github.com/govengers/livewire/generate/internal/fixture/billing\"\n)\n", file.importBlock())

	for _, refused := range []reflect.Type{reflect.TypeFor[func()](), reflect.TypeFor[interface{ Close() }](), reflect.TypeFor[fixture.Page[struct{}]]()} {
		_, err := file.expression(refused)
		require.ErrorContains(t, err, "cannot name the type", refused.String())
	}
}

func TestGoImportAliasesNeverCollide(t *testing.T) {
	file := newGoFile("example.com/application/client", []string{"Client", "fixture"})
	require.Equal(t, "fixture2", file.use("github.com/govengers/livewire/generate/internal/fixture", "fixture"), "a generated identifier keeps its name")
	require.Equal(t, "fixture3", file.use("example.com/other/fixture", "fixture"))
	require.Equal(t, "yaml", file.use("gopkg.in/yaml.v3", ""))
	require.Equal(t, "dto9lives", file.use("example.com/9-lives", ""))
	require.Equal(t, "type2", file.use("example.com/type", "type"), "a keyword is no alias")
	require.Equal(t, "fixture2", file.use("github.com/govengers/livewire/generate/internal/fixture", "fixture"), "a path keeps its alias")
	require.Equal(t, "import (\n"+
		"\tdto9lives \"example.com/9-lives\"\n"+
		"\tfixture3 \"example.com/other/fixture\"\n"+
		"\ttype2 \"example.com/type\"\n"+
		"\tfixture2 \"github.com/govengers/livewire/generate/internal/fixture\"\n"+
		"\tyaml \"gopkg.in/yaml.v3\"\n"+
		")\n", file.importBlock())
	require.Empty(t, newGoFile("x", nil).importBlock())
}

func TestGoNamesFollowGoStyle(t *testing.T) {
	for name, expected := range map[string]string{"filterId": "FilterID", "id": "ID", "createdAt": "CreatedAt", "content-type": "ContentType", "apiUrl": "APIURL", "ownerIds": "OwnerIDs"} {
		require.Equal(t, expected, goIdentifier(name), name)
	}
	for name, expected := range map[string]string{"createPart": "create-part", "parts": "parts", "filterId": "filter-id", "uploadHTTPFile": "upload-httpfile", "x_y": "x-y"} {
		require.Equal(t, expected, kebabCase(name), name)
	}
	_, err := formatGo("broken.go", "package")
	require.ErrorContains(t, err, "broken.go does not format")
}
