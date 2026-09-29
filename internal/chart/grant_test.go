package chart_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
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
	lead := r.writer.As("mira", chart.AuthorHuman, nil, chart.Provenance{})

	// A PROSE edit lands: who may edit an object's name and goal is the lead
	// relation's to decide, at the door, and this package deliberately does
	// not. A write that CHANGED an authority-bearing relation — `project`,
	// `space`, `email` — would be refused here below the company's grant, as
	// the runtime half is next.
	if _, err := lead.WriteSeat(t.Context(), "op-public", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen",
		Goal: "ship the thing",
	}); err != nil {
		t.Fatalf("a public content write by a party with no grants: %v", err)
	}
	r.drain()

	// The same write carrying the opaque half does not.
	_, err := lead.WriteSeat(t.Context(), "op-runtime", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen",
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

// plainCompany is a company whose unit and first seat declare nothing the
// engine alone reads — no model chain, no credentials, no contact — beside an
// agent seat that does.
const plainCompany = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
units:
  - name: Engineering
    id: eng
    purpose: ships the product
    roles:
      - name: Jane Doe
        handle: jane
        kind: human
        goal: run the team
      - name: SRE
        handle: sre
        llm: zulu
        goal: keep it up
`

// seedCompany places a company file's chart the way a node's first boot does:
// the import, then one content record per object, written by the rig's own
// party, which holds the company's grant.
func (r *writeRig) seedCompany(doc string) chart.Authored {
	r.t.Helper()
	company, err := config.ParseCompany([]byte(doc))
	if err != nil {
		r.t.Fatalf("parse the company: %v", err)
	}
	authored := config.AuthoredChart(company)
	if _, err := r.writer.WriteImport(r.t.Context(), "op-seed", "rev-seed",
		authored.Edges()); err != nil {
		r.t.Fatalf("import the company's structure: %v", err)
	}
	r.drain()
	for _, unit := range authored.Units {
		if _, err := r.writer.WriteUnit(r.t.Context(), "op-seed-u-"+unit.Key,
			chart.UnitContent{
				Key: unit.Key, Name: unit.Name, Type: unit.Type,
				Purpose: unit.Purpose, Goals: unit.Goals, Channel: unit.Channel,
				Project: unit.Project, Space: unit.Space,
				KnowledgeRefs: unit.KnowledgeRefs, Runtime: unit.Runtime,
			}); err != nil {
			r.t.Fatalf("seed the content of unit %s: %v", unit.Key, err)
		}
	}
	for _, seat := range authored.Seats {
		if _, err := r.writer.WriteSeat(r.t.Context(), "op-seed-s-"+seat.Handle,
			chart.SeatContent{
				Handle: seat.Handle, Unit: seat.Unit, Name: seat.Name,
				Email: seat.Email, Backstory: seat.Backstory, Goal: seat.Goal,
				Responsibilities:     seat.Responsibilities,
				BehavioralGuidelines: seat.BehavioralGuidelines,
				Project:              seat.Project, Space: seat.Space,
				Runtime: seat.Runtime,
			}); err != nil {
			r.t.Fatalf("seed the content of seat %s: %v", seat.Handle, err)
		}
	}
	r.drain()
	return authored
}

// A LEAD EDITS THE PROSE OF A PLAIN SEAT AND A PLAIN UNIT, seeded from a file.
//
// The seat and the unit declare nothing the engine alone reads, so nothing a
// lead writes about them can touch the company's configuration — yet every
// seeded object used to carry a runtime half of `{"name":""}`, the encoding of
// a cleared document that held nothing, and a lead holding no grant was
// refused a goal as though it were a credential. Seeded through the SAME
// function a node's boot and `crewlet config import` use, because that is
// where the eleven bytes came from.
func TestALeadEditsThePlainObjectsACompanyFileSeeded(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.seedCompany(plainCompany)
	lead := r.writer.As("mira", chart.AuthorHuman, nil, chart.Provenance{})

	if _, err := lead.WriteSeat(t.Context(), "op-goal", chart.SeatContent{
		Handle: "jane", Unit: "eng", Name: "Jane Doe", Goal: "hire two SREs",
	}); err != nil {
		t.Fatalf("a lead's edit of a plain seat's goal: %v", err)
	}
	if _, err := lead.WriteUnit(t.Context(), "op-purpose", chart.UnitContent{
		Key: "eng", Name: "Engineering", Purpose: "ships and runs the product",
	}); err != nil {
		t.Fatalf("a lead's edit of a plain unit's purpose: %v", err)
	}
	r.drain()
	if seat := r.mustSeat("jane"); seat.Goal != "hire two SREs" || len(seat.Runtime) != 0 {
		t.Errorf("the seat reads goal %q and runtime %s, want the lead's goal "+
			"and still no runtime", seat.Goal, seat.Runtime)
	}
	if unit := r.mustUnit("eng"); unit.Purpose != "ships and runs the product" ||
		len(unit.Runtime) != 0 {
		t.Errorf("the unit reads purpose %q and runtime %s, want the lead's "+
			"purpose and still no runtime", unit.Purpose, unit.Runtime)
	}
}

// AND AN AGENT SEAT THE SAME FILE SEEDED, whose runtime half is its model
// chain: the lead's goal lands and the chain is carried, byte for byte.
func TestALeadEditsAnAgentSeatACompanyFileSeededAndKeepsItsModelChain(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.seedCompany(plainCompany)
	before := r.mustSeat("sre").Runtime
	if !strings.Contains(string(before), "zulu") {
		t.Fatalf("the seeded agent seat carries runtime %s, want its model chain", before)
	}
	lead := r.writer.As("mira", chart.AuthorHuman, nil, chart.Provenance{})
	if _, err := lead.WriteSeat(t.Context(), "op-goal", chart.SeatContent{
		Handle: "sre", Unit: "eng", Name: "SRE", Goal: "keep it up at night too",
	}); err != nil {
		t.Fatalf("a lead's edit of an agent seat's goal: %v", err)
	}
	r.drain()
	seat := r.mustSeat("sre")
	if seat.Goal != "keep it up at night too" || string(seat.Runtime) != string(before) {
		t.Errorf("the agent seat reads goal %q and runtime %s, want the lead's "+
			"goal and its model chain %s", seat.Goal, seat.Runtime, before)
	}
}

// runtimeObject is one of the chart's two content writes, seen as the three
// things a runtime case varies: the prose a lead edits, the runtime half it
// states, and whether it clears that half.
type runtimeObject struct {
	name  string
	seed  json.RawMessage
	place func(r *writeRig)
	write func(w *chart.Writer, opID, prose string, runtime json.RawMessage,
		clear bool) error
	read func(r *writeRig) (prose string, runtime json.RawMessage)
}

// runtimeObjects are a seat and a unit, each seeded with a runtime half by a
// party holding the company's grant. BOTH HALVES OF THE CHART, because the two
// decides read different rows and a rule written into one of them looks
// exactly like a rule written into both.
func runtimeObjects() []runtimeObject {
	return []runtimeObject{{
		name: "a seat",
		seed: json.RawMessage(`{"models":["opus"],"mcp_env":{"gl":{"T":"${T}"}}}`),
		place: func(r *writeRig) {
			r.batch("op-seat", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))
		},
		write: func(w *chart.Writer, opID, prose string, runtime json.RawMessage,
			clear bool) error {
			_, err := w.WriteSeat(context.Background(), opID, chart.SeatContent{
				Handle: "sarah-chen", Name: "Sarah Chen", Goal: prose,
				Runtime: runtime, ClearRuntime: clear,
			})
			return err
		},
		read: func(r *writeRig) (string, json.RawMessage) {
			seat := r.mustSeat("sarah-chen")
			return seat.Goal, seat.Runtime
		},
	}, {
		name: "a unit",
		seed: json.RawMessage(`{"mcp_env":{"gl":{"T":"${T}"}},"token_budget":9007199254740993}`),
		place: func(r *writeRig) {
			r.batch("op-unit", op(chart.OpCreateUnit, chart.KindUnit, "eng", ""))
		},
		write: func(w *chart.Writer, opID, prose string, runtime json.RawMessage,
			clear bool) error {
			_, err := w.WriteUnit(context.Background(), opID, chart.UnitContent{
				Key: "eng", Name: "Engineering", Purpose: prose,
				Runtime: runtime, ClearRuntime: clear,
			})
			return err
		},
		read: func(r *writeRig) (string, json.RawMessage) {
			unit := r.mustUnit("eng")
			return unit.Purpose, unit.Runtime
		},
	}}
}

// AN OMITTED RUNTIME HALF IS THE ONE THE OBJECT HAS, AND WHAT A WRITE ASKS FOR
// IS WHAT IT CHANGES.
//
// A content record is full post-state, so a write that left the runtime half
// out used to CLEAR it — and every edit of a seat with a model chain had to be
// refused to a lead, since the edit would have wiped the chain. The half is
// carried from the row inside the decide instead, so a lead's prose edit
// lands and the seat keeps its credentials; and the class is decided by what
// the record CHANGES against that row, so a runtime restated verbatim — in
// another key order — asks for nothing, while a different one, or a clear,
// asks for the company's grant.
//
// Mutations: stop carrying and the first case wipes the runtime; read the class
// off what the payload carries and the first two are refused; compare the
// bytes rather than the objects and the second is refused; compare decoded
// floats and the unit's budget past 2^53 reads as unchanged; drop the clear
// from the change list and the lead's clear lands.
func TestAWriteAsksForWhatItChangesAndCarriesTheRuntimeItLeftOut(t *testing.T) {
	t.Parallel()
	for _, object := range runtimeObjects() {
		t.Run(object.name, func(t *testing.T) {
			t.Parallel()
			r := newWriteRig(t)
			object.place(r)
			if err := object.write(r.writer, "op-seed", "seeded", object.seed,
				false); err != nil {
				t.Fatalf("seed the runtime half: %v", err)
			}
			r.drain()
			lead := r.writer.As("mira", chart.AuthorHuman, nil, chart.Provenance{})
			admin := r.writer.As("ops", chart.AuthorHuman,
				[]iam.Grant{iam.GrantConfigWrite}, chart.Provenance{})

			if err := object.write(lead, "op-prose", "the lead's", nil, false); err != nil {
				t.Fatalf("a lead's prose edit leaving the runtime out: %v", err)
			}
			r.drain()
			prose, runtime := object.read(r)
			if prose != "the lead's" || string(runtime) != string(object.seed) {
				t.Fatalf("after the lead's edit the object reads %q with runtime "+
					"%s, want the lead's prose and the seeded runtime %s — an "+
					"omitted half is the one the object has", prose, runtime,
					object.seed)
			}

			// THE SAME HALF, RE-ENCODED, IS NO CHANGE.
			var decoded map[string]any
			dec := json.NewDecoder(strings.NewReader(string(object.seed)))
			dec.UseNumber()
			if err := dec.Decode(&decoded); err != nil {
				t.Fatalf("decode the seed: %v", err)
			}
			restated, err := json.MarshalIndent(decoded, "", "  ")
			if err != nil {
				t.Fatalf("restate the seed: %v", err)
			}
			if err := object.write(lead, "op-restated", "restated",
				restated, false); err != nil {
				t.Fatalf("a lead restating the runtime the object already "+
					"holds: %v — a write asks for what it changes", err)
			}
			r.drain()

			for _, refused := range []struct {
				what    string
				runtime json.RawMessage
				clear   bool
			}{
				{"a different runtime", json.RawMessage(`{"models":["haiku"]}`), false},
				// ONE BELOW A BUDGET PAST 2^53, which a float64 reads
				// as the same number.
				{"a budget one below what it holds", json.RawMessage(
					strings.Replace(string(object.seed), "993}", "992}", 1)), false},
				{"a clear", nil, true},
			} {
				if bytes.Equal(refused.runtime, object.seed) {
					continue
				}
				err := object.write(lead, "op-"+refused.what, "x", refused.runtime,
					refused.clear)
				if !errors.Is(err, chart.ErrRefused) {
					t.Errorf("a lead's write of %s to %s: err = %v, want a "+
						"refusal", refused.what, object.name, err)
					continue
				}
				for _, named := range []string{"runtime", string(iam.GrantConfigWrite)} {
					if !strings.Contains(err.Error(), named) {
						t.Errorf("the refusal of %s does not name %q: %v",
							refused.what, named, err)
					}
				}
			}
			r.drain()
			if _, runtime := object.read(r); string(runtime) != string(object.seed) {
				t.Errorf("a refused write changed the runtime to %s", runtime)
			}

			// AND THE COMPANY'S GRANT CLEARS IT.
			if err := object.write(admin, "op-clear", "cleared", nil, true); err != nil {
				t.Fatalf("a clear by the company's grant: %v", err)
			}
			r.drain()
			if _, runtime := object.read(r); len(runtime) != 0 {
				t.Errorf("the object still holds %s after a clear", runtime)
			}
		})
	}
}

// THE FIELDS LEADERSHIP IS DERIVED FROM TAKE THE COMPANY'S GRANT.
//
// A seat's `project`, `space` and `email` say which tracker project and page
// container it leads and whose vendor actions are attributed to it; a unit's
// `project`, `space` and `channel` the same for the team. Decided on the lead
// relation, each was a way for a lead to grow their own authority: pointing
// their unit at another team's project key gave them that project's removals,
// archive and policy. So a write CHANGING one asks for config:write in the
// decide, and is refused with a GrantRefusal naming exactly the fields that
// asked — while a lead sending back what they read changes nothing and writes
// their prose. A seat's `manages` is not among them: it is structure
// ([TestAManagesListIsStructureALeadCannotWrite]).
//
// A SEALED EMAIL IS NEVER RESEALED BY A REFUSED WRITE: the refusal is decided
// before the seal, so the address in the secret store is the one it was.
//
// Mutation: answer false for any one field's comparison, and its case lands.
func TestTheFieldsLeadershipIsDerivedFromTakeTheCompanysGrant(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-tree",
		op(chart.OpCreateUnit, chart.KindUnit, "eng", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "report", "eng"),
		op(chart.OpCreateSeat, chart.KindSeat, "intern", "eng"),
		op(chart.OpCreateSeat, chart.KindSeat, "ceo", ""),
	)
	authored := chart.SeatContent{
		Handle: "report", Unit: "eng", Name: "Report", Goal: "ship",
		Email: "report@example.com", Project: "ENG", Space: "ENG",
	}
	unit := chart.UnitContent{
		Key: "eng", Name: "Engineering", Purpose: "ships the product",
		Channel: "eng-chat", Project: "ENG", Space: "ENG",
	}
	// EVERY CASE STARTS FROM THIS, put back by the company's grant once the
	// case's own admitted write has landed.
	restore := func(opID string, seat chart.SeatContent) {
		t.Helper()
		r.whileDraining(func() {
			if _, err := r.writer.WriteSeat(t.Context(), opID+"-seat", seat); err != nil {
				t.Errorf("write the seat: %v", err)
			}
			if _, err := r.writer.WriteUnit(t.Context(), opID+"-unit", unit); err != nil {
				t.Errorf("write the unit: %v", err)
			}
		})
	}
	restore("op-seed", authored)
	// WHAT A READ SERVES, which is what a lead sends back: the sealed
	// email's reference — and what every case is put back to, since a
	// literal restated seals under the restoring write's own name.
	seat := authored
	seat.Email = r.mustSeat("report").Email
	sealed, _ := envref.Whole(seat.Email)

	lead := r.writer.As("mira", chart.AuthorHuman, nil, chart.Provenance{})
	admin := r.writer.As("ops", chart.AuthorHuman,
		[]iam.Grant{iam.GrantConfigWrite}, chart.Provenance{})

	prose := seat
	prose.Goal = "ship faster"
	if _, err := lead.WriteSeat(t.Context(), "op-prose", prose); err != nil {
		t.Fatalf("a lead's prose edit sending back what they read: %v", err)
	}
	r.drain()
	masked := prose
	masked.Email = redacted()
	if _, err := lead.WriteSeat(t.Context(), "op-masked", masked); err != nil {
		t.Fatalf("a lead's prose edit sending back the masked email: %v", err)
	}
	unitProse := unit
	unitProse.Purpose = "ships and runs the product"
	if _, err := lead.WriteUnit(t.Context(), "op-unit-prose", unitProse); err != nil {
		t.Fatalf("a lead's edit of a unit's purpose: %v", err)
	}
	r.drain()
	if got := r.mustSeat("report"); got.Goal != "ship faster" || got.Email != seat.Email {
		t.Fatalf("after the lead's edits the seat reads goal %q and email %q, "+
			"want theirs and the email it had", got.Goal, got.Email)
	}

	edit := func(change func(*chart.SeatContent)) chart.SeatContent {
		next := seat
		change(&next)
		return next
	}
	editUnit := func(change func(*chart.UnitContent)) chart.UnitContent {
		next := unit
		change(&next)
		return next
	}
	for _, c := range []struct {
		name   string
		write  func(w *chart.Writer, opID string) error
		fields []string
	}{
		{"the seat's project moves to another team's", seatWrite(edit(func(s *chart.SeatContent) {
			s.Project = "OPS"
		})), []string{"project"}},
		{"the seat's space moves to the org root's", seatWrite(edit(func(s *chart.SeatContent) {
			s.Space = "ORG"
		})), []string{"space"}},
		{"the seat's email changes", seatWrite(edit(func(s *chart.SeatContent) {
			s.Email = "somebody.else@example.com"
		})), []string{"email"}},
		{"the seat's email is cleared", seatWrite(edit(func(s *chart.SeatContent) {
			s.Email = ""
		})), []string{"email"}},
		{"two at once, named in order", seatWrite(edit(func(s *chart.SeatContent) {
			s.Space, s.Project = "ORG", "OPS"
		})), []string{"project", "space"}},
		{"the unit's project moves to another team's", unitWrite(editUnit(func(u *chart.UnitContent) {
			u.Project = "OPS"
		})), []string{"project"}},
		{"the unit's space moves to the org root's", unitWrite(editUnit(func(u *chart.UnitContent) {
			u.Space = "ORG"
		})), []string{"space"}},
		{"the unit's channel changes", unitWrite(editUnit(func(u *chart.UnitContent) {
			u.Channel = "ops-chat"
		})), []string{"channel"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.write(lead, "op-lead-"+c.name)
			var refusal *chart.GrantRefusal
			if !errors.As(err, &refusal) || !errors.Is(err, chart.ErrRefused) {
				t.Fatalf("a lead's write: err = %v, want a grant refusal", err)
			}
			if !slices.Equal(refusal.Fields, c.fields) ||
				!slices.Equal(refusal.Grants, []iam.Grant{iam.GrantConfigWrite}) {
				t.Errorf("refused naming fields %v and grants %v, want %v and "+
					"[config:write]", refusal.Fields, refusal.Grants, c.fields)
			}
			if got, _ := r.sealer.get(sealed); got != "report@example.com" {
				t.Errorf("a refused write left the sealed email as %q", got)
			}
			// APPLIED AS THEY LAND, so each write finds the one before
			// it rather than waiting out the resolve budget for it.
			r.whileDraining(func() {
				if err := c.write(admin, "op-admin-"+c.name); err != nil {
					t.Errorf("the company's grant making the same change: %v", err)
				}
			})
			restore("op-restore-"+c.name, seat)
		})
	}
}

// seatWrite and unitWrite are one content write, as a case states it.
func seatWrite(content chart.SeatContent) func(*chart.Writer, string) error {
	return func(w *chart.Writer, opID string) error {
		_, err := w.WriteSeat(context.Background(), opID, content)
		return err
	}
}

func unitWrite(content chart.UnitContent) func(*chart.Writer, string) error {
	return func(w *chart.Writer, opID string) error {
		_, err := w.WriteUnit(context.Background(), opID, content)
		return err
	}
}

// A RUNTIME HALF THAT IS NOT AN OBJECT, OR IS STATED AND CLEARED AT ONCE, IS
// REFUSED WHOEVER WRITES IT.
//
// The half is decoded onto the running seat, and a value of any other shape
// decodes onto nothing, so storing one is a seat that silently runs with no
// model chain; and a write that states a half and clears it is two answers to
// one question. The company's grant is what the refusals are held against,
// because a rule a grant could buy past is not a rule about the value.
func TestARuntimeHalfIsOneObjectOrNone(t *testing.T) {
	t.Parallel()
	for _, object := range runtimeObjects() {
		t.Run(object.name, func(t *testing.T) {
			t.Parallel()
			r := newWriteRig(t)
			object.place(r)
			for _, c := range []struct {
				what    string
				runtime json.RawMessage
				clear   bool
			}{
				{"null", json.RawMessage(`null`), false},
				{"a list", json.RawMessage(`["opus"]`), false},
				{"a string", json.RawMessage(`"opus"`), false},
				{"two values", json.RawMessage(`{} {}`), false},
				{"a runtime and a clear", json.RawMessage(`{"models":["opus"]}`), true},
			} {
				err := object.write(r.writer, "op-"+c.what, "x", c.runtime, c.clear)
				if !errors.Is(err, chart.ErrRefused) {
					t.Errorf("%s as %s's runtime: err = %v, want a refusal",
						c.what, object.name, err)
				}
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
	lead := r.writer.As("mira", chart.AuthorHuman, nil, chart.Provenance{})

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
		{"a rename", func() error {
			_, err := lead.WriteBatch(t.Context(), "op-rename", chart.Batch{
				Operations: []chart.Operation{
					renameOp(chart.KindUnit, "eng", "engineering"),
				},
			})
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

// A REMOVAL TAKES THE DEPLOYMENT'S GRANT AS WELL AS THE COMPANY'S, AT THE
// RECORD.
//
// It is the one structural change nothing undoes: the address is tombstoned
// for ever and the seat's mailbox goes with it. internal/authz asks both hats
// at the route; the record asks them too, because a door that skipped its own
// check would otherwise publish a removal on the company's grant alone. A
// party holding only the company's grant is refused naming exactly what it
// lacks, and the chart still holds the seat; the same party holding both
// removes it. A placement on the company's grant alone is the control that the
// second hat is the removal's.
//
// Mutation: state `structural` for the removal record and the first party
// removes the seat; name every required grant in the refusal and it names
// config:write too.
func TestARemovalTakesTheDeploymentsGrantAtTheRecord(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))
	removal := chart.Batch{Reason: "left", Operations: []chart.Operation{
		op(chart.OpRemoveObject, chart.KindSeat, "sarah-chen", ""),
	}}
	admin := r.writer.As("ops", chart.AuthorHuman,
		[]iam.Grant{iam.GrantConfigWrite}, chart.Provenance{}).WithHolders(noHolders{})

	_, err := admin.WriteRemoval(t.Context(), "op-remove", removal)
	var refusal *chart.GrantRefusal
	if !errors.As(err, &refusal) || !errors.Is(err, chart.ErrRefused) ||
		refusal.Class != chart.ClassRemoval ||
		!slices.Equal(refusal.Grants, []iam.Grant{iam.GrantFleetOperate}) {
		t.Fatalf("a removal on the company's grant alone: err = %v, want a "+
			"grant refusal naming exactly [%s]", err, iam.GrantFleetOperate)
	}
	r.drain()
	if got := r.column(`SELECT handle FROM chart_seats`); len(got) != 1 {
		t.Fatalf("the refused removal reached the rows: the chart holds %v", got)
	}
	// THE CONTROL: the same party places a seat on the company's grant.
	if _, err := admin.WriteBatch(t.Context(), "op-hire", chart.Batch{
		Operations: []chart.Operation{
			op(chart.OpCreateSeat, chart.KindSeat, "omar", ""),
		}}); err != nil {
		t.Fatalf("a placement on the company's grant was refused: %v", err)
	}
	r.drain()

	both := r.writer.As("ops", chart.AuthorHuman,
		[]iam.Grant{iam.GrantConfigWrite, iam.GrantFleetOperate},
		chart.Provenance{}).WithHolders(noHolders{})
	if _, err := both.WriteRemoval(t.Context(), "op-remove-both", removal); err != nil {
		t.Fatalf("a removal holding both grants: %v", err)
	}
	r.drain()
	if got := r.column(`SELECT handle FROM chart_seats WHERE handle = 'sarah-chen'`); len(got) != 0 {
		t.Errorf("the seat is still in the chart after a removal holding both "+
			"grants: %v", got)
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
	if got := r.writer.As("mira", chart.AuthorHuman, nil, chart.Provenance{}).Grants; len(got) != 0 {
		t.Fatalf("a clone with no grants carries %v", got)
	}
	back := r.writer.As("ops", chart.AuthorOperator,
		[]iam.Grant{iam.GrantConfigWrite}, chart.Provenance{}).Grants
	if len(back) != 1 || back[0] != iam.GrantConfigWrite {
		t.Fatalf("a clone's grants = %v, want exactly what was stated", back)
	}
	// AND THE ORIGINAL IS UNTOUCHED, which is what makes one writer per
	// party safe to derive concurrently.
	if !slices.Equal(r.writer.Grants,
		[]iam.Grant{iam.GrantConfigWrite, iam.GrantFleetOperate}) {
		t.Fatalf("deriving a party changed the writer it came from: %v",
			r.writer.Grants)
	}
}

// A PARTY'S PROVENANCE IS ITS OWN, and it lands on the history row.
//
// The operator a write was made through travels with the party through As
// and is REPLACED there: a party derived from one that carried a machine token
// must not inherit it, and one that states its token must have it on the row
// the chart's history keeps. As took no provenance at all, so every write a
// person made through `/chart` recorded no credential, and one made through
// somebody's token read as theirs. Mutation: carry the source writer's
// operator in As and the second party's row names the first party's token.
func TestAPartysProvenanceIsItsOwnAndLandsOnTheRow(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-unit", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))
	const via = "pat:0192f00d-0000-7000-8000-00000000000a"
	through := r.writer.As("mira", chart.AuthorHuman,
		[]iam.Grant{iam.GrantConfigWrite}, chart.Provenance{OperatorID: via})
	if through.OperatorID != via {
		t.Fatalf("the party acts through %q, want %q", through.OperatorID, via)
	}
	if again := through.As("dana", chart.AuthorHuman, nil,
		chart.Provenance{}); again.OperatorID != "" || again.TurnID != "" {
		t.Errorf("a party derived from one acting through a token inherited "+
			"operator %q and turn %q", again.OperatorID, again.TurnID)
	}
	if _, err := through.WriteUnit(t.Context(), "op-unit-via", chart.UnitContent{
		Key: "platform", Name: "Platform",
	}); err != nil {
		t.Fatalf("write through the token: %v", err)
	}
	r.drain()
	got := r.column(`SELECT operator_id FROM chart_history ORDER BY version DESC LIMIT 1`)
	if len(got) != 1 || got[0] != via {
		t.Errorf("the history row names the operator %v, want %q", got, via)
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
		// TWO GRANTS, both needed — the row's `also` and the class's
		// second one, compared as a set.
		{chart.ClassRemoval, authz.ActionChartRemove},
	}
	for _, c := range cases {
		t.Run(string(c.action), func(t *testing.T) {
			t.Parallel()
			grant, known := authz.GrantOf(c.action)
			if !known {
				t.Fatalf("the authority table has no rule for %q", c.action)
			}
			want := []iam.Grant{grant}
			if also, _ := authz.AlsoGrantOf(c.action); also != "" {
				want = append(want, also)
			}
			got := chart.GrantsFor(c.class)
			slices.Sort(want)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("a %s record needs %v and %q asks for %v",
					c.class, got, c.action, want)
			}
		})
	}
	// AND THE PUBLIC HALF ASKS FOR NOTHING, because it is decided by a
	// RELATION. A grant here would be this domain having an opinion about
	// who leads a unit, which is the one thing it must not decide.
	if got := chart.GrantsFor(chart.ClassPublic); len(got) != 0 {
		t.Errorf("a public content record asks for %v, want no capability — "+
			"it is the unit lead's, and a lead relation is not a grant", got)
	}
	if want, _ := authz.GrantOf(authz.ActionChartContent); want != "" {
		t.Errorf("the authority table asks %q for a content write", want)
	}
}
