package assistant_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
)

// denyingCompany is the deep-work company with tools.deny set to deny.
func denyingCompany(t *testing.T, deny ...string) assistant.Company {
	t.Helper()
	c, err := evals.DenyingCompany(deny, "grace", "ada")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The model is never offered a family the company denies, deep work included,
// and the rest stay offered.
func TestADeniedToolFamilyIsNeverOffered(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		deny     []string
		gone     []string
		kept     []string
		readsImg bool
	}{
		{nil, nil, []string{"get_task", "create_task", catalog.StartDeepWorkTool}, true},
		{[]string{"writes"}, []string{"create_task"}, []string{"get_task", catalog.StartDeepWorkTool}, true},
		{[]string{"deep_work"}, []string{catalog.StartDeepWorkTool}, []string{"get_task", "create_task"}, false},
	} {
		t.Run(fmt.Sprint(tc.deny), func(t *testing.T) {
			t.Parallel()
			f := assistanttest.NewRig(t, evals.RigOptions{
				Company: denyingCompany(t, tc.deny...), Deep: &evals.DeepScript{},
				Handler: task, Acts: []string{"create_work_item"},
				Tools:  []catalog.Tool{getTask(t), assistanttest.Tool(t, "create_task")},
				Rounds: []evals.Round{evals.Say("Here.")}})
			accepted, err := f.Ask("k-deny", "What is open?", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.Follow(accepted.TurnID, 0)
			names := offered(t, f)
			for _, name := range tc.gone {
				if slices.Contains(names, name) {
					t.Errorf("%s was offered with %v denied: %v", name, tc.deny, names)
				}
			}
			for _, name := range tc.kept {
				if !slices.Contains(names, name) {
					t.Errorf("%s was not offered with %v denied: %v", name, tc.deny, names)
				}
			}
			if got := f.Svc.ReadsImages(); got != tc.readsImg {
				t.Errorf("ReadsImages = %v with %v denied", got, tc.deny)
			}
		})
	}
}
