package host

import (
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE LINE GIT PRINTS FOR A PUSH'S CHANGE LINKS ITS PAGE: a push that opened a
// change, or revised the live one its branch heads, is told the change's name
// and the address of its page on the deployment's base — the dashboard's
// shell and the change's route in it, the repository's name one path segment
// whatever it holds — and a host told no base names the change alone, never
// a link that could not be opened.
//
// Mutations: print the name alone on a base — every linked row, red; spell
// the route `change/` — every linked row, red; leave the name unescaped — the
// row whose name holds a slash, red; link on no base — the bare rows, red.
func TestTheLineGitPrintsLinksThePushsChange(t *testing.T) {
	t.Parallel()
	const base = "https://crewlet.example.com"
	landed := pushResult{outcome: types.AuditApplied, position: statelog.Position{Stream: "code", Seq: 9}}
	asked := func(number int64, opened bool) changeAsk {
		return changeAsk{asked: true, answer: &code.ChangeAnswer{ChangeID: "c", Number: number, Opened: opened}}
	}
	for _, tc := range []struct {
		name, repo, base string
		ask              changeAsk
		want             string
	}{
		{name: "opened", repo: "web", base: base, ask: asked(42, true),
			want: "change web!42 opened: https://crewlet.example.com/dashboard#/code/repos/web/changes/42"},
		{name: "updated", repo: "web", base: base, ask: asked(42, false),
			want: "change web!42 updated: https://crewlet.example.com/dashboard#/code/repos/web/changes/42"},
		{name: "a scratch repository", repo: "ceo.notes", base: base, ask: asked(7, true),
			want: "change ceo.notes!7 opened: https://crewlet.example.com/dashboard#/code/repos/ceo.notes/changes/7"},
		{name: "a name the route must hold as one segment", repo: "web/x", base: base, ask: asked(3, false),
			want: "change web/x!3 updated: https://crewlet.example.com/dashboard#/code/repos/web%2Fx/changes/3"},
		{name: "opened on no base", repo: "web", ask: asked(42, true), want: "change web!42 opened"},
		{name: "updated on no base", repo: "web", ask: asked(42, false), want: "change web!42 updated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := changeLine(tc.ask, landed, tc.repo, tc.base); got != tc.want {
				t.Fatalf("git is told %q of the push's change, want %q", got, tc.want)
			}
		})
	}
}
