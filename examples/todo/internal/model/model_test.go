package model_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/examples/todo/internal/model"
)

func TestATodoChangeAppliesWhatItCarries(t *testing.T) {
	due := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	open := model.Todo{Title: "milk", Priority: model.PriorityNormal, DueAt: &due}
	testCases := map[string]struct {
		change    model.TodoChange
		expected  model.Todo
		completes bool
	}{
		"nothing":          {model.TodoChange{}, open, false},
		"a title":          {model.TodoChange{Title: new("bread")}, model.Todo{Title: "bread", Priority: model.PriorityNormal, DueAt: &due}, false},
		"done":             {model.TodoChange{Done: new(true)}, model.Todo{Title: "milk", Priority: model.PriorityNormal, Done: true, DueAt: &due}, true},
		"still open":       {model.TodoChange{Done: new(false)}, open, false},
		"the due time off": {model.TodoChange{ClearDueAt: true}, model.Todo{Title: "milk", Priority: model.PriorityNormal}, false},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			changed := testCase.change.Apply(open)
			require.True(t, changed.Equal(testCase.expected), "%+v", changed)
			require.Equal(t, testCase.completes, testCase.change.Completes(open))
		})
	}
	later := due.Add(time.Hour)
	require.False(t, open.Equal(model.Todo{Title: "milk", Priority: model.PriorityNormal, DueAt: &later}))
	require.True(t, open.Equal(model.Todo{Title: "milk", Priority: model.PriorityNormal, DueAt: new(due.In(time.FixedZone("plus one", 3600)))}), "the same instant")
}
