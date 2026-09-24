package iamapi_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// AN ADMINISTRATOR PINNING A PROVIDER SUBJECT: the second of the only two ways
// a subject is ever linked to somebody.

const testIssuer = "https://idp.example.com"

// linkBlinder is the blinder the rig's surface pins subjects with, so a case
// can compute the blind it expects.
func linkBlinder(t *testing.T) *iamdomain.Blinder {
	t.Helper()
	blinder, err := iamdomain.NewBlinder([]byte(strings.Repeat("k",
		iamdomain.MinBlindKeyBytes)))
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	return blinder
}

func blindOf(t *testing.T, sub string) string {
	t.Helper()
	blind, err := linkBlinder(t).Subject(testIssuer, sub)
	if err != nil {
		t.Fatalf("blind: %v", err)
	}
	return blind
}

// withProvider is a rig on a deployment that signs people in through
// testIssuer.
func withProvider(t *testing.T) func(*iamapi.Options) {
	return func(o *iamapi.Options) {
		o.Issuer = testIssuer
		o.Blinds = linkBlinder(t)
	}
}

// AN ADMINISTRATOR PINS A SUBJECT, MOVES IT, AND TAKES IT OFF — naming the link
// being replaced every time.
//
// The subject an administrator types is blinded under THIS deployment's
// issuer, which is the one every provider sign-in resolves under; a pin of
// the same subject the person already holds publishes nothing; a move names
// the link the person holds now, which is what lets the estate refuse a
// relink that happens in passing; and the empty string unlinks.
func TestAnAdministratorPinsMovesAndRemovesALink(t *testing.T) {
	t.Parallel()
	r := newRig(t, withProvider(t))
	patch := func(sub string) answered {
		t.Helper()
		return r.as(administrator(), http.MethodPatch, "/iam/people/"+bob.String(),
			map[string]any{"oidc_subject": sub, "reason": "their provider account"})
	}

	if got := patch("sub-first"); got.status != http.StatusOK {
		t.Fatalf("a first link answered %d (%v)", got.status, got.body)
	}
	if len(r.writer.links) != 1 {
		t.Fatalf("links %+v, want one", r.writer.links)
	}
	first := r.writer.links[0]
	if first.PersonID != bob.String() || first.Replacing != "" ||
		first.Link != (iamdomain.Link{Issuer: testIssuer, Blind: blindOf(t, "sub-first")}) {
		t.Errorf("the first link was %+v, want bob pinned to sub-first's blind "+
			"under the deployment's issuer, replacing nothing", first)
	}

	// THE PERSON NOW HOLDS IT, as the directory reports.
	row := r.directory.people[bob.String()]
	row.Link = first.Link
	r.directory.people[bob.String()] = row

	if got := patch("sub-first"); got.status != http.StatusOK || len(r.writer.links) != 1 {
		t.Errorf("re-pinning the subject they hold answered %d and asked for "+
			"%d links, want 200 and nothing published", got.status, len(r.writer.links))
	}
	if got := patch("sub-second"); got.status != http.StatusOK {
		t.Fatalf("a move answered %d (%v)", got.status, got.body)
	}
	if moved := r.writer.links[1]; moved.Replacing != first.Link.Blind ||
		moved.Link.Blind != blindOf(t, "sub-second") {
		t.Errorf("the move was %+v, want sub-second replacing the link bob "+
			"holds now", moved)
	}
	if got := patch(""); got.status != http.StatusOK {
		t.Fatalf("an unlink answered %d (%v)", got.status, got.body)
	}
	if len(r.writer.unlinked) != 1 || r.writer.unlinked[0].from != first.Link.Blind {
		t.Errorf("unlinked %+v, want the link bob holds", r.writer.unlinked)
	}

	// AND A PERSON LINKED TO NOTHING IS UNLINKED BY NOTHING.
	nothing := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"oidc_subject": ""})
	if nothing.status != http.StatusOK || len(r.writer.unlinked) != 1 {
		t.Errorf("unlinking somebody linked to nothing answered %d with %d "+
			"unlinks, want 200 and none", nothing.status, len(r.writer.unlinked))
	}
}

// AN EDIT THE SURFACE CAN REFUSE IS REFUSED BEFORE ITS FIRST RECORD.
//
// A PATCH is a sequence, and a value refused halfway leaves every record
// before it landed. So a subject on a deployment with no provider, one
// carrying whitespace (a subject is matched byte for byte, so a pasted space
// pins an account the provider never asserts) and one longer than OpenID
// Connect allows are refused before the seat that rides in the same body
// moves.
func TestALinkTheSurfaceCanRefuseIsRefusedFirst(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		options []func(*iamapi.Options)
		subject string
	}{
		"no provider":        {nil, "sub-1"},
		"a pasted space":     {[]func(*iamapi.Options){withProvider(t)}, "sub-1 "},
		"longer than OpenID": {[]func(*iamapi.Options){withProvider(t)}, strings.Repeat("s", 256)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, tc.options...)
			got := r.as(administrator(), http.MethodPatch,
				"/iam/people/"+bob.String(),
				map[string]any{"seat": "sre", "oidc_subject": tc.subject})
			if got.status != http.StatusBadRequest {
				t.Fatalf("answered %d (%v), want 400", got.status, got.body)
			}
			if len(r.writer.calls) != 0 {
				t.Errorf("published %v before refusing the body", r.writer.calls)
			}
		})
	}
}

// A SUBJECT SOMEBODY ELSE HOLDS, AND A RELINK IN PASSING, ARE 409
// `subject_conflict` — the code the sign-in surface answers the same fact with.
//
// The administrator may manage people, so the conflict NAMES the holder they
// have to go and unlink; it never carries the blind, which is a keyed hash
// nobody can act on.
func TestALinkConflictIsNamed(t *testing.T) {
	t.Parallel()
	blind := blindOf(t, "held")
	for name, refusal := range map[string]error{
		"held by somebody else": &iamdomain.ErrClaimed{Kind: iamdomain.KindLink,
			Token: blind, Holder: alice.String()},
		"a relink in passing": fmt.Errorf("%w: person %s", iamdomain.ErrLinked,
			bob.String()),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, withProvider(t))
			r.writer.err = refusal
			got := r.as(administrator(), http.MethodPatch,
				"/iam/people/"+bob.String(), map[string]any{"oidc_subject": "held"})
			if got.status != http.StatusConflict || got.body["error"] != "subject_conflict" {
				t.Fatalf("answered %d %v, want 409 subject_conflict", got.status,
					got.body)
			}
			detail, _ := got.body["detail"].(string)
			if strings.Contains(detail, blind) {
				t.Errorf("the conflict carries the blind: %q", detail)
			}
			if _, claimed := refusal.(*iamdomain.ErrClaimed); claimed &&
				got.body["holder"] != alice.String() {
				t.Errorf("the conflict names holder %v, want %s", got.body["holder"],
					alice)
			}
		})
	}
}

// A PROVIDER ACCOUNT SOMEBODY ELSE HOLDS IS REFUSED BEFORE ANYTHING MOVES.
//
// The link is claimed after the seat, so a subject somebody else is pinned to,
// met only at the link's own record, was refused with the seat already moved.
// This node's rows can say who holds it, so the edit is refused first.
func TestALinkSomebodyHoldsIsRefusedBeforeTheSeatMoves(t *testing.T) {
	t.Parallel()
	r := newRig(t, withProvider(t))
	row := r.directory.people[alice.String()]
	row.Link = iamdomain.Link{Issuer: testIssuer, Blind: blindOf(t, "sub-held")}
	r.directory.people[alice.String()] = row
	got := r.as(administrator(), http.MethodPatch, "/iam/people/"+bob.String(),
		map[string]any{"seat": "sre", "oidc_subject": "sub-held"})
	if got.status != http.StatusConflict || got.body["holder"] != alice.String() {
		t.Fatalf("answered %d %v, want 409 naming the holder", got.status,
			got.body)
	}
	if len(r.writer.calls) != 0 {
		t.Errorf("a link somebody holds published %v before it was refused",
			r.writer.calls)
	}
}

// THE DIRECTORY SAYS WHO IS LINKED, AND TO WHICH PROVIDER — NEVER THE SUBJECT.
func TestThePersonViewNamesTheProviderAndNotTheSubject(t *testing.T) {
	t.Parallel()
	r := newRig(t, withProvider(t))
	row := r.directory.people[bob.String()]
	row.Link = iamdomain.Link{Issuer: testIssuer, Blind: blindOf(t, "sub-9")}
	r.directory.people[bob.String()] = row

	got := r.as(administrator(), http.MethodGet, "/iam/people/"+bob.String(), nil)
	if got.status != http.StatusOK {
		t.Fatalf("answered %d", got.status)
	}
	oidc, _ := got.body["oidc"].(map[string]any)
	if oidc["issuer"] != testIssuer {
		t.Errorf("oidc = %v, want the provider named", got.body["oidc"])
	}
	if strings.Contains(fmt.Sprint(got.body), row.Link.Blind) {
		t.Errorf("the view carries the subject's blind: %v", got.body)
	}
	unlinked := r.as(administrator(), http.MethodGet, "/iam/people/"+alice.String(), nil)
	if _, present := unlinked.body["oidc"]; present {
		t.Errorf("somebody linked to nothing renders oidc = %v", unlinked.body["oidc"])
	}
}

// REVOKING A LINK'S CREDENTIAL ID UNLINKS, AND A MACHINE TOKEN MAY NOT.
//
// A link is its claim's row and never part of the person's credential set, so
// the revocation that rewrote the set used to leave the link exactly where it
// was and answer "nothing changed" — an administrator who revoked somebody's
// provider sign-in was told it was done while it still worked. The id now
// unlinks. And a request carrying a MACHINE TOKEN is refused, through the real
// guard, because a link is how its owner signs in and a token proves nobody is
// present.
func TestRevokingALinksCredentialUnlinks(t *testing.T) {
	t.Parallel()
	linkID := uuid.Must(uuid.NewV7()).String()
	link := iamdomain.Link{Issuer: testIssuer, Blind: blindOf(t, "sub-7")}
	r := newRig(t, withProvider(t))
	r.directory.creds = map[string][]iamdomain.CredentialRow{}
	for _, person := range []string{alice.String(), bob.String()} {
		r.directory.creds[person] = []iamdomain.CredentialRow{{
			ID: linkID, PersonID: person, Method: iamdomain.MethodOIDC,
			Issuer: link.Issuer, SubjectBlind: link.Blind, CreatedAt: at,
		}}
	}

	got := r.as(administrator(), http.MethodDelete,
		"/iam/credentials/"+linkID+"?person="+bob.String(), nil)
	if got.status != http.StatusOK {
		t.Fatalf("answered %d (%v)", got.status, got.body)
	}
	if len(r.writer.unlinked) != 1 || r.writer.unlinked[0].person != bob.String() ||
		r.writer.unlinked[0].from != link.Blind {
		t.Errorf("unlinked %+v, want bob's link", r.writer.unlinked)
	}
	for _, call := range r.writer.calls {
		if call == "credentials" {
			t.Error("the revocation also rewrote the credential set, which " +
				"does not hold the link")
		}
	}

	// THROUGH THE REAL GUARD, alice's own token revoking her own link.
	presented, row := aliceToken(t)
	rec := throughTheGuard(t, r, row, http.MethodDelete,
		"/iam/credentials/"+linkID, presented)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a token unlinking its owner answered %d, want 403: %s",
			rec.Code, rec.Body)
	}
	if len(r.writer.unlinked) != 1 {
		t.Error("a machine token took its owner's provider sign-in away")
	}
}
