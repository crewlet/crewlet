package authz_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// SEAT-ONLY, HUMAN-ONLY AND SEATED ARE CHECKED BEFORE THE CLASS.
//
// All three are facts about the VERB: an admin person is refused a seat-only
// verb as [authz.ReasonSeatOnly], a seat a human-only one as
// [authz.ReasonSeatRefused], and a principal acting as no seat a seated one as
// [authz.ReasonUnseated], each naming no permission, before any class weighs one.
//
// THE HUMAN-ONLY ORDER IS PROVED BY THE SEAT THE CLASS REFUSES (no_grant,
// not_self, not_lead if decided after it); a seat holding admin alone could not
// tell. The SEAT-ONLY ORDER is observable only on a [authz.ClassSelf] row naming
// no app, by the admin person; any other class FAILS this walk.
// Mutation: check humanOnly or seated after the class and the rows go red.
func TestSeatOnlyHumanOnlyAndSeatedAreCheckedBeforeTheClass(t *testing.T) {
	t.Parallel()
	var seatOnly, humanOnly, seatedRows int
	for _, a := range authz.Actions() {
		object := everythingAbout(seat("sre"))
		if only, _ := authz.SeatOnlyOf(a); only {
			seatOnly++
			class, _ := authz.ClassOf(a)
			app, _ := authz.AppOf(a)
			caller := seat("sre")
			switch {
			case app != "" && class == authz.ClassApp:
				// AN AGENT'S OWN VERB ON AN APP'S SHARED SURFACE (ADR-0132):
				// a person the class refuses, one without the app, is still
				// told the verb is an agent's, naming no permission; and a
				// seat is admitted exactly where it holds the app.
				stranger := authz.Decide(t.Context(), person("jane.doe"), a, object, nimbus(), decidedAt)
				if stranger.Allowed || stranger.Reason != authz.ReasonSeatOnly || len(stranger.Grants) != 0 {
					t.Errorf("%s: a person without %s decided %+v, want refused as seat-only "+
						"naming nothing", a, app, stranger)
				}
				without := authz.Decide(t.Context(), caller, a, object, nimbus(), decidedAt)
				if without.Allowed || !slices.Equal(without.Grants, []iam.Grant{app}) {
					t.Errorf("%s: a seat without %s decided %+v, want refused naming it", a, app, without)
				}
				caller = seat("sre", app)
			case app != "":
				t.Errorf("%s is seat-only and decided by %s on the app %q, so the order of "+
					"its bar is observable: add a case deciding it as a person that "+
					"class refuses, who must be refused seat_only naming nothing", a,
					class, app)
			case class == authz.ClassEverybody:
			case class == authz.ClassSelf:
				// THE PERSON BELOW IS ONE THE CLASS REFUSES: the object is the
				// seat's own record, and the self class has no admin path, so a
				// bar checked after it would tell them not_self.
			default:
				t.Errorf("%s is seat-only and decided by %s, so the order of its bar is "+
					"observable: add a case deciding it as a person that class refuses, "+
					"who must be refused seat_only naming nothing", a, class)
			}
			if d := authz.Decide(t.Context(), caller, a, object, nimbus(),
				decidedAt); !d.Allowed {
				t.Errorf("%s: a seat holding %v decided %+v, want admitted: a seat-only "+
					"verb asks no permission beyond the app its row names", a, caller.Grants, d)
			}
			d := authz.Decide(t.Context(), person("jane.doe", iam.GrantAdmin), a,
				object, nimbus(), decidedAt)
			if d.Allowed || d.Reason != authz.ReasonSeatOnly || len(d.Grants) != 0 {
				t.Errorf("%s: a person holding admin decided %+v, want refused "+
					"as seat-only naming nothing", a, d)
			}
		}
		if only, _ := authz.HumanOnlyOf(a); only {
			humanOnly++
			d := authz.Decide(t.Context(), seat("cto", iam.GrantAdmin), a, object,
				nimbus(), decidedAt)
			if d.Allowed || d.Reason != authz.ReasonSeatRefused || len(d.Grants) != 0 {
				t.Errorf("%s: a seat holding admin decided %+v, want refused as a "+
					"seat naming nothing", a, d)
			}
			// A SEAT THE CLASS REFUSES: holding nothing, about somebody else's
			// record, in a container nobody leads. On an artifact row that
			// seat holds a viewer share: one with no relation at all is told
			// the artifact is not found, whatever bar the verb carries
			// (TestABarOnTheVerbIsNoExistenceOracle).
			refused := authz.Object{Kind: authz.KindPerson, Owner: "someone.else",
				Container: "NOWHERE"}
			if class, _ := authz.ClassOf(a); class == authz.ClassShared {
				refused = authz.Object{Kind: authz.KindArtifact, Owner: "someone.else",
					Shares: []artifacts.Share{seatShare("sam", artifacts.RoleViewer)}}
			}
			d = authz.Decide(t.Context(), seat("sam"), a, refused, nimbus(), decidedAt)
			if d.Allowed || d.Reason != authz.ReasonSeatRefused || len(d.Grants) != 0 {
				t.Errorf("%s: a seat the class refuses decided %+v, want refused as a "+
					"seat naming nothing", a, d)
			}
		}
		if only, _ := authz.SeatedOf(a); only {
			seatedRows++
			for _, unseated := range []iam.Principal{
				person("jane.doe", iam.GrantAdmin),
				machine("ci:release", iam.GrantAdmin),
				// THE CLASS WOULD REFUSE THIS ONE naming a permission, so
				// it is what tells the bar's place from a bar after it.
				person("lee.nobody"),
			} {
				d := authz.Decide(t.Context(), unseated, a, everythingAbout(unseated),
					nimbus(), decidedAt)
				if d.Allowed || d.Reason != authz.ReasonUnseated || len(d.Grants) != 0 {
					t.Errorf("%s: %s acting as no seat decided %+v, want refused as "+
						"unseated naming nothing", a, unseated.Login, d)
				}
			}
			for _, bound := range []iam.Principal{
				personLeading("cto", iam.GrantAdmin), personLeading("sam"),
				seat("sre"), seat("cto", iam.GrantAdmin),
			} {
				d := authz.Decide(t.Context(), bound, a, everythingAbout(bound),
					nimbus(), decidedAt)
				if d.Reason == authz.ReasonUnseated {
					t.Errorf("%s: %s acts as the seat %q and was refused as "+
						"unseated", a, bound.Login, bound.Seat)
				}
			}
		}
	}
	if seatOnly == 0 || humanOnly == 0 || seatedRows == 0 {
		t.Fatalf("%d seat-only, %d human-only and %d seated rows: a walk over "+
			"none of one certifies nothing about it", seatOnly, humanOnly, seatedRows)
	}
}

// NO SEAT-ONLY ROW IS ALSO SEATED.
//
// A seat-only verb is an agent seat's already, and an agent always acts as its
// seat, so the second mark changes no answer — and in the table it reads as a
// decision somebody made about a credential the row already refuses.
// Mutation: mark a2a_ask seated and this goes red.
func TestNoSeatOnlyRowIsAlsoSeated(t *testing.T) {
	t.Parallel()
	for _, a := range authz.Actions() {
		seatOnly, _ := authz.SeatOnlyOf(a)
		seated, _ := authz.SeatedOf(a)
		if seatOnly && seated {
			t.Errorf("%s is seat-only and seated, and the second mark says "+
				"nothing the first does not", a)
		}
	}
	if _, known := authz.SeatedOf("no.such.verb"); known {
		t.Error("SeatedOf knows a verb the table has no row for")
	}
}

// NO ROW IS BOTH SEAT-ONLY AND HUMAN-ONLY.
//
// Such a row admits nobody at all, and it reads in the table as two careful
// decisions rather than as the dead verb it is. Mutation: mark a2a_ask
// human-only and this goes red.
func TestNoRowIsBothSeatOnlyAndHumanOnly(t *testing.T) {
	t.Parallel()
	for _, a := range authz.Actions() {
		seatOnly, _ := authz.SeatOnlyOf(a)
		humanOnly, _ := authz.HumanOnlyOf(a)
		if seatOnly && humanOnly {
			t.Errorf("%s is both seat-only and human-only, so nobody may take it", a)
		}
	}
	if _, known := authz.SeatOnlyOf("no.such.verb"); known {
		t.Error("SeatOnlyOf knows a verb the table has no row for")
	}
	if _, known := authz.HumanOnlyOf("no.such.verb"); known {
		t.Error("HumanOnlyOf knows a verb the table has no row for")
	}
}

// EVERY DOTTED VERB IS ASKED BY A CALL SITE THAT IS NOT A TEST.
//
// A verb named after a tool is held to the registered tools by a walk in
// internal/agent/builtin; a DOTTED verb has no tool, so only this holds it. A
// dotted row nothing asks decides nothing, yet reads as a gated gesture. The
// walk ([treeScan]) reads rules.go for every `Action…` constant's wire name,
// then every non-test Go file outside this package for a selector naming one,
// and fails on a dotted verb none names.
//
// Mutation: delete `work.thread`'s one call site in internal/api/workapi and
// this goes red naming it.
func TestEveryDottedVerbIsAskedByANonTestCallSite(t *testing.T) {
	t.Parallel()
	scan := treeScan(t)
	dotted := 0
	for constant, verb := range scan.wire {
		if !strings.Contains(verb, ".") {
			continue
		}
		dotted++
		if !scan.asked(constant) {
			t.Errorf("%s (%q) is a dotted verb no non-test code asks, so its row "+
				"decides nothing — ask it where the gesture is decided, or delete it",
				constant, verb)
		}
	}
	if dotted == 0 {
		t.Fatal("no dotted verb was read, so this walk certifies nothing")
	}
}
