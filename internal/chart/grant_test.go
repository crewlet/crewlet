package chart_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
)

// TestAContentRecordWithAPrivilegedFieldIsRefusedBelowItsGrant is the rule
// grant.go exists for, asked at the DECIDE rather than at a door.
//
// Four surfaces reach these writes and each decides authority for itself. The
// half a GRANT settles is decided here as well, so a door that forgot refuses
// anyway — and what it would otherwise have written is a seat's model chain,
// its credentials, its sandbox cell and its `mcp_env`, which is exec.Command
// on every engine host.
func TestAContentRecordWithAPrivilegedFieldIsRefusedBelowItsGrant(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))

	// A LEAD'S PARTY: no capability at all, which is the ordinary state of
	// somebody who runs a team and does not hold the deployment.
	lead := r.writer.As("mira", chart.AuthorHuman, nil)

	// The public half lands, because a public edit is a RELATION and this
	// package deliberately decides none of it.
	if _, err := lead.WriteSeat(t.Context(), "op-public", chart.SeatContent{
		Handle: "sarah-chen", Kind: chart.SeatAgent, Name: "Sarah Chen",
		Goal: "ship the thing",
	}); err != nil {
		t.Fatalf("a public content write by a party with no grants: %v", err)
	}
	r.drain()

	// The same write carrying the opaque half does not.
	_, err := lead.WriteSeat(t.Context(), "op-runtime", chart.SeatContent{
		Handle: "sarah-chen", Kind: chart.SeatAgent, Name: "Sarah Chen",
		Goal:    "ship the thing",
		Runtime: json.RawMessage(`{"mcp_env":{"SHELL":"/bin/sh"}}`),
	})
	if !errors.Is(err, chart.ErrRefused) {
		t.Fatalf("a runtime half authored with no grants: err = %v, want a refusal", err)
	}
	// NAMING THE GRANT, because the person reading it has to know what to
	// ask for. A refusal that says only "no" sends somebody to the wrong
	// door.
	if want := string(iam.GrantConfigWrite); !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name %q: %v", want, err)
	}

	// AND NOTHING WAS PUBLISHED. A refusal inside the decide must leave the
	// log untouched, or the gate would merely be reporting on a record every
	// node had already applied.
	r.drain()
	got := r.column(`SELECT document FROM chart_seats WHERE handle = ?`, "sarah-chen")
	for _, doc := range got {
		if strings.Contains(doc, "mcp_env") {
			t.Fatalf("the refused runtime half reached the rows: %s", doc)
		}
	}
}

// TestClearingTheRuntimeHalfIsStillAPrivilegedWrite is the control the class
// rule needs, and the one a reader of contentClass will doubt.
//
// A content record is FULL POST-STATE, so a write that OMITS the runtime half
// clears it. If "carries no runtime bytes" meant "public", a party with no
// capability could delete every seat's model chain and every unit's engine
// settings one request at a time — a change to the company's configuration
// made by somebody who may not make one.
//
// BOTH HALVES OF THE CHART, because the two decides read different rows and a
// gate written into one of them looks exactly like a gate written into both.
func TestClearingTheRuntimeHalfIsStillAPrivilegedWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		seed func(r *writeRig)
		wipe func(w *chart.Writer) error
	}{
		{
			name: "a seat",
			seed: func(r *writeRig) {
				r.batch("op-seat", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))
				if _, err := r.writer.WriteSeat(r.t.Context(), "op-runtime",
					chart.SeatContent{
						Handle: "sarah-chen", Kind: chart.SeatAgent,
						Runtime: json.RawMessage(`{"models":["opus"]}`),
					}); err != nil {
					r.t.Fatalf("seed the runtime half: %v", err)
				}
				r.drain()
			},
			wipe: func(w *chart.Writer) error {
				_, err := w.WriteSeat(t.Context(), "op-clear", chart.SeatContent{
					Handle: "sarah-chen", Kind: chart.SeatAgent,
				})
				return err
			},
		},
		{
			name: "a unit",
			seed: func(r *writeRig) {
				r.batch("op-unit", op(chart.OpCreateUnit, chart.KindUnit, "eng", ""))
				if _, err := r.writer.WriteUnit(r.t.Context(), "op-runtime",
					chart.UnitContent{
						Key:     "eng",
						Runtime: json.RawMessage(`{"mcp_env":{"TOKEN":"${T}"}}`),
					}); err != nil {
					r.t.Fatalf("seed the runtime half: %v", err)
				}
				r.drain()
			},
			wipe: func(w *chart.Writer) error {
				_, err := w.WriteUnit(t.Context(), "op-clear", chart.UnitContent{
					Key: "eng",
				})
				return err
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newWriteRig(t)
			c.seed(r)
			err := c.wipe(r.writer.As("mira", chart.AuthorHuman, nil))
			if !errors.Is(err, chart.ErrRefused) {
				t.Fatalf("clearing %s's runtime half with no grants: err = %v, "+
					"want a refusal — a write that omits it SETS it to empty",
					c.name, err)
			}
		})
	}
}

// TestStructureIsNeverOneTeamsToChange covers the other class the gate holds:
// a create, a move, a removal, an import and a rekey.
func TestStructureIsNeverOneTeamsToChange(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-unit", op(chart.OpCreateUnit, chart.KindUnit, "eng", ""))
	r.batch("op-seat", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", "eng"))
	lead := r.writer.As("mira", chart.AuthorHuman, nil)

	cases := []struct {
		name string
		run  func() error
	}{
		{"a placement", func() error {
			_, err := lead.WriteBatch(t.Context(), "op-move", chart.Batch{
				Operations: []chart.Operation{
					op(chart.OpCreateUnit, chart.KindUnit, "sales", ""),
				},
			})
			return err
		}},
		{"a removal", func() error {
			_, err := lead.WriteRemoval(t.Context(), "op-remove", chart.Batch{
				Operations: []chart.Operation{
					op(chart.OpRemoveObject, chart.KindSeat, "sarah-chen", ""),
				},
				Reason: "left",
			})
			return err
		}},
		{"a rekey", func() error {
			_, err := lead.WriteRekey(t.Context(), "op-rekey",
				chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"}, "eng")
			return err
		}},
		{"an import", func() error {
			_, err := lead.WriteImport(t.Context(), "op-import", "rev-1",
				[]chart.Edge{{Object: chart.ObjectRef{
					Kind: chart.KindUnit, ID: "eng"}}})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(); !errors.Is(err, chart.ErrRefused) {
				t.Fatalf("%s authored with no grants: err = %v, want a refusal",
					c.name, err)
			}
		})
	}
}

// TestAsReplacesAPartysGrantsRatherThanCarryingThemForward — the signature is
// what makes this unwritable, and this is what says so.
//
// A surface takes ONE writer per party from the node's own, and the node's own
// holds everything. Carrying its grants into a clone would hand an anonymous
// caller the deployment's authority on the first write.
func TestAsReplacesAPartysGrantsRatherThanCarryingThemForward(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	if got := r.writer.As("mira", chart.AuthorHuman, nil).Grants; len(got) != 0 {
		t.Fatalf("a clone with no grants carries %v", got)
	}
	back := r.writer.As("ops", chart.AuthorOperator,
		[]iam.Grant{iam.GrantConfigWrite}).Grants
	if len(back) != 1 || back[0] != iam.GrantConfigWrite {
		t.Fatalf("a clone's grants = %v, want exactly what was stated", back)
	}
	// AND THE ORIGINAL IS UNTOUCHED, which is what makes one writer per
	// party safe to derive concurrently.
	if len(r.writer.Grants) != 1 {
		t.Fatalf("deriving a party changed the writer it came from: %v",
			r.writer.Grants)
	}
}

// TestTheDomainAndTheAuthorityTableAgreeAboutTheChart holds the two places
// that state what a privileged chart write costs against each other.
//
// internal/authz decides the DOOR and this package decides the RECORD, and
// they ask for the same capability by two separate statements. Drift is
// silent in the direction that matters least and confusing in the other: a
// door asking for less lets a request through to a refusal it cannot explain,
// and a door asking for more refuses somebody the domain would have served.
func TestTheDomainAndTheAuthorityTableAgreeAboutTheChart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		class  chart.PayloadClass
		action authz.Action
	}{
		{chart.ClassPrivileged, authz.ActionChartRuntime},
		{chart.ClassStructure, authz.ActionChartStructure},
		{chart.ClassStructure, authz.ActionChartRename},
		{chart.ClassStructure, authz.ActionChartImport},
	}
	for _, c := range cases {
		t.Run(string(c.action), func(t *testing.T) {
			t.Parallel()
			want, known := authz.GrantOf(c.action)
			if !known {
				t.Fatalf("the authority table has no rule for %q", c.action)
			}
			if got := chart.GrantFor(c.class); got != want {
				t.Errorf("a %s record needs %q and %q asks for %q",
					c.class, got, c.action, want)
			}
		})
	}
	// AND THE PUBLIC HALF ASKS FOR NOTHING, because it is decided by a
	// RELATION. A grant here would be this domain having an opinion about
	// who leads a unit, which is the one thing it must not decide.
	if got := chart.GrantFor(chart.ClassPublic); got != "" {
		t.Errorf("a public content record asks for %q, want no capability — "+
			"it is the unit lead's, and a lead relation is not a grant", got)
	}
	if want, _ := authz.GrantOf(authz.ActionChartContent); want != "" {
		t.Errorf("the authority table asks %q for a content write", want)
	}
}
