package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ONE CONCEPT, ONE FLAG, REGISTERED IN ONE PLACE.
//
// The running node a command reaches, and a retry's operation id, each had
// two names in this CLI, because each flag set registered its own and two of
// them spelt it differently ([nodeAPIFlag] and [opIDFlag] say which). So each
// name is registered by one helper that every flag set calls, and this reads
// the package's source for a registration of either name anywhere else — a
// second site is where a second spelling starts, since it can be typed
// differently from the first.
//
// Mutation: register `fs.String("api", ...)` directly in secrets.go, or
// `fs.String("op-id", ...)` in seats.go, and this fails naming the line.
func TestEachConceptsFlagIsRegisteredByOneHelper(t *testing.T) {
	t.Parallel()
	helpers := map[string]string{
		nodeAPIFlagName: "nodeAPIFlag",
		opIDFlagName:    "opIDFlag",
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	consts := map[string]string{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		parsed = append(parsed, f)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i < len(vs.Values) {
						if v, ok := stringLiteral(vs.Values[i]); ok {
							consts[id.Name] = v
						}
					}
				}
			}
		}
	}

	registered := map[string]int{}
	for _, f := range parsed {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name, ok := flagRegistered(call, consts)
				if !ok {
					return true
				}
				helper, guarded := helpers[name]
				switch {
				case !guarded:
				case fn.Name.Name == helper:
					registered[name]++
				default:
					t.Errorf("%s registers -%s in %s rather than through %s, "+
						"which is how one flag comes to be spelt two ways",
						fset.Position(call.Pos()), name, fn.Name.Name, helper)
				}
				return true
			})
		}
	}
	for name, helper := range helpers {
		if registered[name] != 1 {
			t.Errorf("%s registers -%s %d times, want once — the guard "+
				"cannot tell where the one registration is", helper, name,
				registered[name])
		}
	}
}

// flagRegistered is the name a [flag.FlagSet] registration call defines, read
// from a literal or a package constant, or false for any other call.
func flagRegistered(call *ast.CallExpr, consts map[string]string) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	var at int
	switch sel.Sel.Name {
	case "String", "Bool", "Int", "Int64", "Uint", "Uint64", "Float64",
		"Duration", "Func", "BoolFunc":
		at = 0
	case "StringVar", "BoolVar", "IntVar", "Int64Var", "UintVar", "Uint64Var",
		"Float64Var", "DurationVar", "Var", "TextVar":
		at = 1
	default:
		return "", false
	}
	if len(call.Args) <= at {
		return "", false
	}
	if v, ok := stringLiteral(call.Args[at]); ok {
		return v, true
	}
	if id, ok := call.Args[at].(*ast.Ident); ok {
		v, ok := consts[id.Name]
		return v, ok
	}
	return "", false
}

// `GET /iam/check` NAMES A DANGLING BINDING THROUGH THE SEAM THIS NODE WIRES,
// not through one a case builds.
//
// The directory report asks the seam [api.NewHumanSurfaces] wires whether a
// binding dangles, and that seam is the one place the engine's rule — the
// request path's own seat table — is connected to the report, the one place a
// dangling binding is said. internal/api/iamapi's suite hands the report a
// function of its own, so a node whose wiring passed nil, or a seam asking a
// narrower question, left every suite green while `crewlet iam check` said
// nothing about a principal refused on every request. So this case boots a
// node, serves its API the way `crewlet run` does, and reads the report over
// HTTP.
//
// THE RESIDUE IS MADE THE WAY IT IS REALLY LEFT. Every bind is checked against
// the running org as a HUMAN seat, so no write binds anybody to an agent's;
// what leaves one is a company write that turns a held human seat into an
// agent's — an offline import, or a revision another node applied first — and
// the engine's own apply makes no seat-held check. So a service account is
// created on the human seat `founder`, the report is read clean with the seat
// held, and then the company is applied with `founder` an agent's: the
// binding is the same row, and only the seat moved under it. Mutation: decide
// a binding by whether the chart still holds the handle at all, rather than
// as a human seat, and the flipped seat goes unreported.
func TestTheCheckNamesADanglingBindingThroughTheNodesOwnWiring(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := bootedEngineAt(t, peopleCompanyYAML, time.Time{})
	boot := bootstrapFor(t, 0)
	boot.API.Port = reservePort(t)
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serve the node's API: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	base := "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)

	// THE CONTROL FIRST: nobody is bound to anything, so the report has no
	// binding to name — which is what makes the finding below the binding's
	// rather than something the fixture always says.
	if got := danglingFindings(t, base); len(got) != 0 {
		t.Fatalf("a directory with no binding in it reports %v", got)
	}

	bot := uuid.Must(uuid.NewV7()).String()
	writer := e.IAMWriter()
	if writer == nil {
		t.Fatal("the node runs no identity domain, so this case proves nothing")
	}
	// OPERATION IDS IN THE ENGINE'S GRAMMAR, as every surface mints them: an
	// id carrying no instant is one no ledger can vouch for once it has
	// swept.
	if _, err := writer.Create(ctx, iamdomain.Creation{
		PersonID: bot, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "ci:bot", Seat: "founder",
		OpID: statelog.NewOpID(time.Now(), "create-bot"), Reason: "a pipeline",
	}); err != nil {
		t.Fatalf("create the service account on founder: %v", err)
	}
	// IN THIS NODE'S ROWS before the report is read, so "no finding" below
	// is the binding judged rather than a row this node has not written yet
	// — a create answered pending is durable and not applied here.
	for deadline := time.Now().Add(10 * time.Second); ; {
		row, err := e.IAM().Person(ctx, bot)
		if err == nil && row.Seat == "founder" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the service account never reached this node's rows on "+
				"founder: %+v (%v)", row, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// THE SECOND CONTROL: the binding exists, on a seat the company holds
	// as a human seat, so it does not dangle.
	if got := danglingFindings(t, base); len(got) != 0 {
		t.Fatalf("a binding to a held human seat is reported dangling: %v", got)
	}

	// AND NOW THE SEAT MOVES UNDER IT: the same company, founder an agent's.
	flipped, err := config.ParseCompany([]byte(companyYAML +
		"  - name: Founder\n    handle: founder\n    llm: primary\n"))
	if err != nil {
		t.Fatalf("parse the flipped company: %v", err)
	}
	if _, _, err := e.Apply(ctx, flipped, time.Now()); err != nil {
		t.Fatalf("apply the company with founder an agent's seat: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, f := range danglingFindings(t, base) {
			detail, _ := f["detail"].(string)
			if f["person"] == bot && f["seat"] == "founder" &&
				strings.Contains(detail, "Unbind them") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /iam/check never named %s's binding to founder once "+
				"it became an agent's seat, with a service account's remedy; "+
				"it reports %v", bot, danglingFindings(t, base))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// danglingFindings is every `binding_dangling` row the node's report carries.
func danglingFindings(t *testing.T, base string) []map[string]any {
	t.Helper()
	body := getJSON(t, base+"/iam/check")
	// A REFUSAL HAS NO FINDINGS EITHER, so the report's own position is
	// what says this was a report at all: without it every "names nothing"
	// above would be a 401 or a 503 read as a clean directory.
	if _, reported := body["position"]; !reported {
		t.Fatalf("GET /iam/check answered %v, which is not a report", body)
	}
	rows, _ := body["findings"].([]any)
	var out []map[string]any
	for _, row := range rows {
		fields, _ := row.(map[string]any)
		if fields["kind"] == "binding_dangling" {
			out = append(out, fields)
		}
	}
	return out
}

// THE BOOT SAYS WHERE THE FIRST PERSON GOES, and says nothing it cannot know.
//
// A person is always invited onto a human seat, so "invite its first person"
// is an instruction an operator can follow only once the company declares one
// — and the invitation has to name it. So the line names the seats where there
// are some, and the step before the invitation where there are none: a company
// to import, or a human seat to declare. ONLY THE VACANT SEATS are named: a
// seat a service account holds, or an open invitation names, refuses an
// invitation onto it, and "nobody is in the directory" counts people alone —
// so an open invitation is said to be the step already taken, a company whose
// every seat is held says what holds each and what frees one, and a seat
// listing that cannot be read names no seat at all. Where somebody is already
// in, or this node cannot read its directory, it says nothing — the second is
// a node joining a fleet before its identity log has caught up, which telling
// to invite a founder would be telling to invite one into a company that has
// one. Mutation: drop the handles from the sentence, the vacancy filter, the
// no-company arm, or the silence on an unreadable estate, and a row fails.
func TestTheBootSaysWhereTheFirstPersonIsInvited(t *testing.T) {
	t.Parallel()
	nobody := func(context.Context) (bool, error) { return false, nil }
	founder := session.Seat{Handle: "founder", Name: "Founder", Kind: "human"}
	ops := session.Seat{Handle: "ops-lead", Name: "Ops lead", Kind: "human"}
	both := func() ([]session.Seat, bool) { return []session.Seat{founder, ops}, true }
	claimed := func(c iamdomain.SeatClaims) seatClaims {
		return func(context.Context, time.Time) (iamdomain.SeatClaims, error) { return c, nil }
	}
	free := claimed(iamdomain.SeatClaims{})
	machine := iamdomain.SeatBinding{Person: "m-ops", Kind: iam.KindMachine,
		Login: "token:ops", Stage: iam.StageActive, Seat: "ops-lead"}
	invitation := iamdomain.SeatInvitation{Invitation: "inv-7", Seat: "founder",
		ExpiresAt: time.Now().Add(time.Hour)}
	for _, tc := range []struct {
		name    string
		anybody func(context.Context) (bool, error)
		seats   func() ([]session.Seat, bool)
		claims  seatClaims
		// said is false where the line must not be logged at all.
		said bool
		want []string
		not  []string
	}{
		{"no company", nobody, func() ([]session.Seat, bool) { return nil, false }, free,
			true, []string{"has no company yet", "kind: human", "crewlet config import",
				"Org chart › Edit org", "-seat <handle>"}, []string{"founder", "Agents ›"}},
		{"no human seat", nobody, func() ([]session.Seat, bool) { return nil, true }, free,
			true, []string{"has no human seats", "kind: human", "-seat <handle>"},
			[]string{"has no company yet"}},
		{"human seats", nobody, both, free,
			true, []string{"vacant human seat (founder, ops-lead)",
				"Company › Org chart › the seat › Invite",
				"crewlet iam invite <address> -seat <handle>"},
			[]string{"has no human seats", "has no company yet", "Agents ›"}},
		// A SERVICE ACCOUNT ON A SEAT is not a vacancy: an invitation onto
		// it is refused.
		{"a seat a service account holds", nobody, both,
			claimed(iamdomain.SeatClaims{Bindings: []iamdomain.SeatBinding{machine}}),
			true, []string{"vacant human seat (founder)"}, []string{"ops-lead"}},
		// THE FOUNDER INVITED, THE NODE RESTARTED before the link was
		// redeemed: the invitation is the step already taken, and its seat
		// is not offered again.
		{"a seat an open invitation holds", nobody, both,
			claimed(iamdomain.SeatClaims{Invitations: []iamdomain.SeatInvitation{invitation}}),
			true, []string{"invitation is open for founder (invitation inv-7)",
				"crewlet iam cancel-invite ID", "Vacant human seats: ops-lead"},
			[]string{"(founder, ops-lead)", "Invite the first person to a vacant"}},
		{"every seat held", nobody, func() ([]session.Seat, bool) {
			return []session.Seat{ops}, true
		}, claimed(iamdomain.SeatClaims{Bindings: []iamdomain.SeatBinding{machine}}),
			true, []string{"every human seat is taken",
				"ops-lead (service account token:ops, id m-ops)", "crewlet iam unbind ID",
				"kind: human"},
			[]string{"Invite the first person to a vacant"}},
		// A SEAT LISTING THAT CANNOT BE READ names no seat it cannot vouch
		// is free, and points at the command that asks again.
		{"the seats cannot be read", nobody, both,
			func(context.Context, time.Time) (iamdomain.SeatClaims, error) {
				return iamdomain.SeatClaims{}, errors.New("not caught up")
			},
			true, []string{"crewlet iam seats -unheld", "Company › Org chart",
				"-seat <handle>"},
			[]string{"founder", "ops-lead", "Agents ›"}},
		{"somebody is in", func(context.Context) (bool, error) { return true, nil },
			func() ([]session.Seat, bool) { return []session.Seat{founder}, true }, free,
			false, nil, nil},
		{"the estate cannot be read",
			func(context.Context) (bool, error) {
				return false, errors.New("the identity log is not caught up")
			},
			func() ([]session.Seat, bool) { return []session.Seat{founder}, true }, free,
			false, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail, said := unclaimedDetail(t.Context(), tc.anybody, tc.seats, tc.claims,
				time.Now())
			if said != tc.said {
				t.Fatalf("logged %v (%q), want %v", said, detail, tc.said)
			}
			for _, want := range tc.want {
				if !strings.Contains(detail, want) {
					t.Errorf("the line %q does not say %q", detail, want)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(detail, not) {
					t.Errorf("the line %q says %q", detail, not)
				}
			}
		})
	}
}
