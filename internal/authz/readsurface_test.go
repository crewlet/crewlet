package authz_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
)

// readSurfaceDirs are the packages whose answers reach the dashboard's socket:
// the query registry, whose every question is a socket frame as well as a REST
// route, and the socket itself, which decides a watch.
var readSurfaceDirs = []string{"internal/api/queries", "internal/api/stream"}

// NO VERB THE READ SURFACE DECIDES BY ASKS A PROOF OR A PERSON.
//
// A question and a watch are answered over the dashboard's socket, whose
// vocabulary has no `step_up_required` and no `token_refused` — a socket cannot
// carry a step-up, and the dashboard replays one over REST alone — so a refusal
// for either reaching a frame would read as a missing permission nobody could
// grant. internal/api/queries refuses a question REGISTERED as decided by such
// a verb, and no registration sees the verbs an answer decides by inside
// itself (a chat question's room verb, a sandbox tail's trail, a personal
// question's record), nor the one a watch asks. So this reads them off the code
// — every Action constant a non-test file in those packages names ([treeScan])
// — and holds each to asking no proof on any arm ([proofAsked]) and needing no
// person present. A verb spelled as a conversion rather than named by its
// constant would be invisible to it, so a conversion there fails too.
//
// Mutation: have the chat questions decide by authz.ActionSecretReveal and this
// goes red naming internal/api/queries.
func TestNoVerbTheReadSurfaceDecidesAsksAProofOrAPerson(t *testing.T) {
	t.Parallel()
	scan := treeScan(t)
	read := 0
	for _, dir := range readSurfaceDirs {
		if len(scan.verbsNamedIn(dir)) == 0 {
			t.Errorf("read no verb named in %s, which decides by several — the "+
				"walk no longer sees the package", dir)
		}
		for _, at := range scan.conversions[dir] {
			t.Errorf("%s spells a verb as authz.Action(...) rather than naming its "+
				"constant, which no walk of the read surface can see — name the "+
				"constant", at)
		}
		for _, a := range scan.verbsNamedIn(dir) {
			read++
			if r, asks := proofAsked(a); asks {
				t.Errorf("%s, named in %s, asks a %s proof, and a refusal for it "+
					"would reach the socket as a permission nobody can grant", a, dir, r)
			}
			if p, _ := authz.PresenceOf(a); p != authz.PresenceOptional {
				t.Errorf("%s, named in %s, needs a person present (%s), and a "+
					"token's refusal for it would reach the socket as a missing "+
					"permission", a, dir, p)
			}
		}
	}
	if read < 6 {
		t.Fatalf("read %d verbs named by the read surface; a walk that stopped "+
			"reading the code certifies nothing", read)
	}
}
