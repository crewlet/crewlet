package iamapi_test

import (
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE DASHBOARD OFFERS EXACTLY THE ENGINE'S GRANTS, in its order, and withholds
// from a token exactly the grants the engine never lets one carry.
//
// People & access confers grants — an invitation, an edit, a service account,
// a token — by ticking them off `GRANTS`, and greys out of a token's mint the
// ones in `TOKEN_WITHHELD_GRANTS`. A grant the engine grew that the list lacks
// is one no administrator can confer from the dashboard, and one the list kept
// that the engine dropped is a box every write refuses.
//
// Mutation: drop a grant from either list, or reorder `GRANTS`, and this fails.
func TestTheDashboardOffersExactlyTheEnginesGrants(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name string
		want []iam.Grant
	}{
		{"GRANTS", iam.AllGrants},
		{"TOKEN_WITHHELD_GRANTS", iam.PersonPresentGrants},
	} {
		body, err := clientsource.Literal(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		got := clientsource.Strings(body)
		if len(got) == 0 {
			t.Fatalf("%s reads as empty, so this gate certifies nothing", tc.name)
		}
		want := make([]string, 0, len(tc.want))
		for _, g := range tc.want {
			want = append(want, string(g))
		}
		if !slices.Equal(got, want) {
			t.Errorf("the dashboard's %s is %v, want the engine's, in order: %v",
				tc.name, got, want)
		}
	}
}

// THE DASHBOARD CHECKS A LOGIN BY THE ENGINE'S GRAMMAR, login for login.
//
// Every form that takes a login holds it to `PERSON_LOGIN` or `MACHINE_LOGIN`,
// `MAX_LOGIN` and `CREDENTIAL_CLASSES` before it posts, so a person who types
// their own name is told the rule under the field rather than handed the
// domain's refusal. Held by BEHAVIOUR rather than by spelling: the patterns are
// compiled here and a corpus run through them and through [iam.ValidLoginFor],
// so a grammar change on either side that answers any of these differently
// fails — and a pattern that compiles to nothing fails on the corpus's controls.
//
// Mutation: drop the dot from `PERSON_LOGIN`'s separator, raise `MAX_LOGIN`, or
// drop a class from `CREDENTIAL_CLASSES`, and this fails.
func TestTheDashboardChecksALoginByTheEnginesGrammar(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	pattern := func(name string) *regexp.Regexp {
		source, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		re, err := regexp.Compile(source)
		if err != nil {
			t.Fatalf("%s does not compile: %v", name, err)
		}
		return re
	}
	person, machine := pattern("PERSON_LOGIN"), pattern("MACHINE_LOGIN")
	raw, err := clientsource.Scalar(tree, "MAX_LOGIN")
	if err != nil {
		t.Fatal(err)
	}
	maxLogin, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("MAX_LOGIN is %q: %v", raw, err)
	}
	body, err := clientsource.Literal(tree, "CREDENTIAL_CLASSES")
	if err != nil {
		t.Fatal(err)
	}
	classes := clientsource.Strings(body)

	// THE DASHBOARD'S ANSWER, as lib/login.ts composes the four.
	dashboard := func(kind iam.Kind, login string) bool {
		re := person
		if kind == iam.KindMachine {
			re = machine
			for _, c := range classes {
				if strings.HasPrefix(login, c) {
					return false
				}
			}
		}
		return re.MatchString(login) && len(login) <= maxLogin
	}
	corpus := []string{
		"jane.doe", "erin.ng", "dana-sre.ops", "a.b", "a1.b2.c3",
		"ci:release", "token:ops", "deploy:bot-1", "a:b:c",
		"Frank", "erin", "Erin NG", "erin ng", "jane..doe", ".jane", "jane.",
		"jane.doe-", "-jane.doe", "jane_doe", "jane.Doe", "jane.doé",
		"ci::release", "ci:", ":ci", "ci:Release", "ci:erin.ng", "jane.doe:x",
		"pat:release", "session:abc", "pat:", "patrol:x", "sessions:x",
		strings.Repeat("a", 30) + "." + strings.Repeat("b", 33),
		strings.Repeat("a", 30) + "." + strings.Repeat("b", 34),
		strings.Repeat("a", 30) + ":" + strings.Repeat("b", 33),
		strings.Repeat("a", 30) + ":" + strings.Repeat("b", 34),
		"",
	}
	admitted := 0
	for _, kind := range []iam.Kind{iam.KindPerson, iam.KindMachine} {
		for _, login := range corpus {
			engine := iam.ValidLoginFor(kind, login)
			if engine {
				admitted++
			}
			if got := dashboard(kind, login); got != engine {
				t.Errorf("a %s login %q: the dashboard answers %v, the engine %v",
					kind, login, got, engine)
			}
		}
	}
	if admitted == 0 {
		t.Fatal("the engine admits nothing in the corpus, so it certifies nothing")
	}
}

// THE DASHBOARD MINTS WITHIN THE ENGINE'S TOKEN LIFETIMES.
//
// The token dialog says what an empty lifetime takes and refuses one past the
// ceiling before it posts — it posted `400` and showed the engine's sentence
// back — so its two numbers are the engine's, in days. Mutation: move either
// constant on one side and this fails.
func TestTheDashboardMintsWithinTheEnginesTokenLifetimes(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, want := range map[string]time.Duration{
		"TOKEN_DEFAULT_DAYS": credential.DefaultTokenLifetime,
		"TOKEN_MAX_DAYS":     credential.MaxTokenLifetime,
	} {
		raw, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		days, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s is %q: %v", name, raw, err)
		}
		if got := time.Duration(days) * 24 * time.Hour; got != want {
			t.Errorf("the dashboard's %s is %d days, the engine's %v", name, days, want)
		}
	}
}

// THE DASHBOARD WORDS EVERY FINDING KIND, IN THE ENGINE'S ORDER.
//
// People & access words each kind `GET /iam/check` reports off
// `FINDING_KINDS`, which promises the order the report is sorted in — and it
// was a copy nothing held: a kind the engine grew and the list lacked was drawn
// as its raw code, which is what `person_without_seat` would have been. Held
// in both directions and in order, against [iamapi.FindingKinds], which the
// report itself sorts by.
//
// Mutation: drop a kind from either side, or swap two, and this fails.
func TestTheDashboardWordsEveryFindingKindInTheEnginesOrder(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "FINDING_KINDS")
	if err != nil {
		t.Fatal(err)
	}
	got := clientsource.Strings(body)
	want := make([]string, 0, len(iamapi.FindingKinds))
	for _, kind := range iamapi.FindingKinds {
		want = append(want, string(kind))
	}
	if len(got) == 0 || len(want) == 0 {
		t.Fatalf("read %v off the dashboard and %v off the engine, so this "+
			"gate compares nothing", got, want)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's FINDING_KINDS is %v, want the engine's, in "+
			"order: %v", got, want)
	}
}

// THE SEAT SURFACES READ WHAT THE SEAT LISTING SENDS.
//
// The org chart's card, the seat peek, a person's seat page and People & access
// all read `GET /iam/seats` through four declarations — the answer, a seat, its
// holder and its invitation — and a key renamed on the engine's side alone
// would draw an invited seat as vacant on every one of them, with nothing
// failing. So each is held to the answer itself, from a fixture that fills
// every half — a held seat, an invited one whose address opens, an invited
// one whose address does not, and a vacant one — in both directions: every
// member is sent by some row, a REQUIRED member by every row, and every key a
// row sends is declared. And a field the engine OMITS when empty is optional in
// the declaration, read off the struct, because a required member the wire can
// leave out promises every screen a value that is not there.
//
// Mutation: rename a JSON key on [iamapi.SeatInvitation] or
// [iamapi.SeatHolding], drop a member from a declaration, or mark the
// holder's login required, and this fails.
func TestTheSeatSurfacesReadWhatTheSeatListingSends(t *testing.T) {
	t.Parallel()
	r := seatsRig(t)
	r.directory.seatInvites = append(r.directory.seatInvites,
		iamdomain.SeatInvitation{Invitation: "inv-foreign", Seat: "sales",
			Sealed: "foreign", CreatedAt: at, ExpiresAt: at.Add(time.Hour)})
	got := r.as(administrator(), http.MethodGet, "/iam/seats", nil)
	if got.status != http.StatusOK {
		t.Fatalf("GET /iam/seats = %d: %v", got.status, got.body)
	}
	var seats, holders, invitations []map[string]any
	rows, _ := got.body["seats"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		seats = append(seats, row)
		if held := part(row, "holder"); held != nil {
			holders = append(holders, held)
		}
		if invited := part(row, "invitation"); invited != nil {
			invitations = append(invitations, invited)
		}
	}
	for _, tc := range []struct {
		declaration string
		rows        []map[string]any
		typ         reflect.Type
	}{
		{"HumanSeatsAnswer", []map[string]any{got.body}, nil},
		{"HumanSeat", seats, reflect.TypeFor[iamapi.SeatRow]()},
		{"SeatHolder", holders, reflect.TypeFor[iamapi.SeatHolding]()},
		{"SeatInvitation", invitations, reflect.TypeFor[iamapi.SeatInvitation]()},
	} {
		t.Run(tc.declaration, func(t *testing.T) {
			t.Parallel()
			readsWhatIsSent(t, tc.declaration, tc.rows, tc.typ)
		})
	}
}

// readsWhatIsSent holds one declaration's members to the rows an answer sent,
// both ways, and — given the Go type the rows were encoded from — every field
// that type omits when empty to an optional member.
func readsWhatIsSent(t *testing.T, declaration string, rows []map[string]any,
	typ reflect.Type) {

	t.Helper()
	members, err := clientsource.Interface(clientsource.Tree(t), declaration)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{} // member → required
	for _, m := range members {
		declared[m.Name] = !m.Optional
	}
	if len(declared) == 0 || len(rows) == 0 {
		t.Fatalf("read %d member(s) off %s and %d row(s) off the answer, so "+
			"this gate compares nothing", len(declared), declaration, len(rows))
	}
	sent := map[string]bool{}
	for _, row := range rows {
		for key := range row {
			sent[key] = true
			if _, ok := declared[key]; !ok {
				t.Errorf("the answer sends %q on a %s and the dashboard does not "+
					"declare it, so no screen can read it", key, declaration)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if !sent[name] {
			t.Errorf("%s declares %q and no row the answer sent carries it — a "+
				"renamed or removed key leaves exactly this behind", declaration, name)
			continue
		}
		for _, row := range rows {
			if _, ok := row[name]; declared[name] && !ok {
				t.Errorf("%s declares %q REQUIRED and a row does not send it: %v",
					declaration, name, row)
			}
		}
	}
	if typ == nil {
		return
	}
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, opts, _ := strings.Cut(field.Tag.Get("json"), ",")
		omitted := slices.ContainsFunc(strings.Split(opts, ","), func(o string) bool {
			return o == "omitempty" || o == "omitzero"
		})
		if required, ok := declared[name]; ok && omitted && required {
			t.Errorf("%s omits %q when it is empty and %s declares it REQUIRED — "+
				"every screen reading it would read undefined as a value", typ,
				name, declaration)
		}
	}
}
