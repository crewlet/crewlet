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
// The socket has no `step_up_required` or `token_refused` (it cannot carry a
// step-up), so such a refusal in a frame would read as a missing permission
// nobody could grant. internal/api/queries refuses a question REGISTERED as
// decided by such a verb, but not the verbs an answer decides by inside
// itself, nor a watch's. So this reads every Action constant a non-test file
// there names ([treeScan]) and holds each to no proof on any arm
// ([proofAsked]) and no person present. A verb spelled as a conversion would
// be invisible, so a conversion there fails too.
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
