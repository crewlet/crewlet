package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// A TASK WAITS ON A PULL REQUEST ONLY UNDER THE COMPANY'S OWNERS: one there is
// kept under its tracker name, and one elsewhere on the host is refused naming
// why, with nothing written.
func TestWaitingOnAPullRequestIsHeldToTheCompanysOwners(t *testing.T) {
	t.Parallel()
	changeRef := func(raw string) (string, string) {
		switch {
		case strings.Contains(raw, "github.com/acme/"):
			return strings.ToLower(raw), ""
		case strings.Contains(raw, "github.com/"):
			return "", "a pull request under other, which the company's GitHub does not cover: " +
				"a task waits only on one under acme."
		}
		return "", ""
	}
	for _, c := range []struct {
		name, ref, refusal, wrote string
	}{
		{"under the company's owner", "https://github.com/acme/web/pull/12", "",
			"https://github.com/acme/web/pull/12"},
		{"under another owner", "https://github.com/other/web/pull/3", "does not cover", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Dependencies: trk.depends,
				ChangeRef: changeRef})
			got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1",
				"waiting_on": map[string]any{"add": []any{c.ref}}})
			if c.refusal != "" {
				if !got.Failed || !strings.Contains(got.Output, c.refusal) || len(trk.depended) != 0 {
					t.Fatalf("answered %q and wrote %d, want refused saying %q", got.Output,
						len(trk.depended), c.refusal)
				}
				return
			}
			if got.Failed || len(trk.depended) != 1 || len(trk.depended[0].WaitingOnAdd) != 1 ||
				trk.depended[0].WaitingOnAdd[0] != c.wrote {
				t.Fatalf("answered %q and wrote %+v, want a wait on %s", got.Output, trk.depended, c.wrote)
			}
		})
	}
}
