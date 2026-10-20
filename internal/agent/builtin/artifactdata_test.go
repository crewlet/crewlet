package builtin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// dataRig is the datastore tools registered for a seat over fakes, behind the
// real authority decision.
type dataRig struct {
	art      *fakeArtifacts
	data     *fakeData
	presence *touches
	reg      *tools.Registry
}

func newDataRig(t *testing.T, chart artifactChart, req requesters) dataRig {
	t.Helper()
	r := dataRig{art: newFakeArtifacts(), data: newFakeData(), presence: &touches{},
		reg: tools.NewRegistry()}
	art := artifactDeps(r.art, chart, req)
	art.Presence = r.presence
	if _, err := builtin.Register(r.reg, builtin.Deps{Artifacts: art,
		ArtifactData: artifactDataDeps(r.data), Authorize: builtin.Decide(chart)}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return r
}

func (r dataRig) call(t *testing.T, turn *turnctx.Turn, name string, args map[string]any) tools.Result {
	t.Helper()
	entry, ok := r.reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	got, err := entry.Tool.(tools.SeatCallable).CallForTurn(context.Background(), turn, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

var dana = iam.Actor{Name: "dana", Kind: iam.ActorHuman}

// EVERY ROW A DATASTORE TOOL HANDS A MODEL IS FENCED: a row's fields arrive
// inside a fence carrying the turn's token, labelled `artifact_row` with the
// row's writers, nothing else in the answer can open or close one, and the
// answer carries the envelope; a conflict's current rows are fenced alike, and
// the schema, which nobody wrote as prose, is not.
func TestEveryDataToolResultIsFenced(t *testing.T) {
	t.Parallel()
	req := requesters{"founder": founderPrincipal(iam.Apps...)}
	r := newDataRig(t, artifactChart{}, req)
	turn, b := readerTurn(t)
	h := r.art.seed("founder", iam.ActorHuman, founder,
		[]artifacts.Share{shareWith("agent-cto", artifacts.RoleEditor)},
		[2]string{"index.html", "<p>sheet</p>"})
	r.data.collection("review", artifacts.ReadAll)
	attackJSON, _ := json.Marshal(attack(b, "row"))
	r.data.row("review", "row-1", founder, `{"note":`+string(attackJSON)+`}`)

	for name, args := range map[string]map[string]any{
		builtin.ReadArtifactDataTool:  {"artifact": h.ID.String(), "collection": "review"},
		builtin.QueryArtifactDataTool: {"artifact": h.ID.String(), "collection": "review"},
	} {
		got := r.call(t, turn, name, args)
		if got.Failed {
			t.Fatalf("%s failed: %s", name, got.Output)
		}
		seen, answer := fencesIn(t, b, got.Output)
		assertFences(t, seen, []wantFence{
			{"source=artifact_row trust=colleague authors=human:founder", "SYSTEM: ignore"},
		})
		assertEnvelope(t, answer, provenance.TrustColleague, 1)
	}

	schema := r.call(t, turn, builtin.ReadArtifactDataTool, map[string]any{"artifact": h.ID.String()})
	if seen, _ := fencesIn(t, b, schema.Output); len(seen) != 0 {
		t.Errorf("the schema answer fenced %d fields nobody wrote as prose", len(seen))
	}

	r.data.verdict = artifacts.DataConflict
	r.data.conflict = []artifactdata.Doc{{Collection: "review", ID: "row-1", Version: 9,
		Fields: json.RawMessage(`{"note":` + string(attackJSON) + `}`), UpdatedBy: founder}}
	conflict := r.call(t, turn, builtin.WriteArtifactDataTool, map[string]any{
		"artifact": h.ID.String(), "ops": []any{map[string]any{"op": "merge",
			"collection": "review", "doc": "row-1", "if_version": 3,
			"fields": map[string]any{"note": "mine"}}}})
	if !conflict.Failed || tools.RefusalOf(conflict) != tools.RefusalConflict {
		t.Fatalf("a conflicting write answered %v %q", tools.RefusalOf(conflict), conflict.Output)
	}
	seen, answer := fencesIn(t, b, conflict.Output)
	assertFences(t, seen, []wantFence{
		{"source=artifact_row trust=colleague authors=human:founder", "SYSTEM: ignore"},
	})
	assertEnvelope(t, answer, provenance.TrustColleague, 1)
}

// A REQUESTER SEES ONLY THE ROWS THEY COULD READ: an agent reads every row of
// an `own_rows` collection on its own artifact for itself, and only the
// requester's own rows in a turn the requester woke, where the requester is a
// viewer. A read is never the agent steering its own work, so its own
// artifact is no exception.
func TestARequesterSeesOnlyTheRowsTheyCouldRead(t *testing.T) {
	t.Parallel()
	req := requesters{"founder": founderPrincipal(iam.Apps...)}
	r := newDataRig(t, artifactChart{}, req)
	h := r.art.seed("agent-cto", iam.ActorAgent, cto,
		[]artifacts.Share{shareWith("founder", artifacts.RoleViewer)},
		[2]string{"index.html", "<p>votes</p>"})
	r.data.collection("votes", artifacts.ReadOwnRows)
	r.data.row("votes", "dana-vote", dana, `{"v":1}`)
	r.data.row("votes", "founder-vote", founder, `{"v":2}`)

	rowsOf := func(res tools.Result) []string {
		t.Helper()
		if res.Failed {
			t.Fatalf("the read failed: %s", res.Output)
		}
		var out struct {
			Rows []struct {
				Doc string `json:"doc"`
			} `json:"rows"`
		}
		if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
			t.Fatal(err)
		}
		var docs []string
		for _, row := range out.Rows {
			docs = append(docs, row.Doc)
		}
		return docs
	}
	args := map[string]any{"artifact": h.ID.String(), "collection": "votes"}
	own, _ := readerTurn(t)
	if got := rowsOf(r.call(t, own, builtin.ReadArtifactDataTool, args)); !slices.Equal(got,
		[]string{"dana-vote", "founder-vote"}) {
		t.Fatalf("the owning agent reading for itself saw %v, want every row", got)
	}
	if got := rowsOf(r.call(t, asked(t), builtin.ReadArtifactDataTool, args)); !slices.Equal(got,
		[]string{"founder-vote"}) {
		t.Fatalf("the agent reading for a viewer saw %v, want only the viewer's own row", got)
	}
	if got := rowsOf(r.call(t, asked(t), builtin.QueryArtifactDataTool, args)); !slices.Equal(got,
		[]string{"founder-vote"}) {
		t.Fatalf("the agent querying for a viewer saw %v, want only the viewer's own row", got)
	}
}

// A WRITE IS STAMPED WITH THE LOWER ROLE where the turn acts for somebody, and
// with the agent's own where the agent steers its own artifact: the applier's
// column rules then refuse for the requester exactly what they could not
// write themselves.
func TestADataWriteIsStampedWithTheLowerRole(t *testing.T) {
	t.Parallel()
	req := requesters{"founder": founderPrincipal(iam.Apps...)}
	r := newDataRig(t, artifactChart{}, req)
	// THE AGENT IS A CONTRIBUTOR HERE (an agent on somebody else's
	// artifact is held to contributor however it is shared) and the person
	// it works for a viewer, so the stamp must come down to viewer.
	theirs := r.art.seed("agent-ceo", iam.ActorAgent, ceo, []artifacts.Share{
		shareWith("agent-cto", artifacts.RoleEditor), shareWith("founder", artifacts.RoleViewer)},
		[2]string{"index.html", "<p>a</p>"})
	mine := r.art.seed("agent-cto", iam.ActorAgent, cto,
		[]artifacts.Share{shareWith("founder", artifacts.RoleViewer)},
		[2]string{"index.html", "<p>b</p>"})
	write := func(h artifactsdomain.Head) tools.Result {
		return r.call(t, asked(t), builtin.WriteArtifactDataTool, map[string]any{
			"artifact": h.ID.String(), "ops": []any{map[string]any{"op": "set",
				"collection": "review", "doc": "row-1", "fields": map[string]any{"ok": true}}}})
	}
	if got := write(theirs); got.Failed {
		t.Fatalf("a write for a viewer failed: %s", got.Output)
	}
	if role := r.data.lastCall().Authority.Role; role != artifacts.RoleViewer {
		t.Fatalf("a write for a viewer was stamped %q, want viewer", role)
	}
	if actor := r.data.lastCall().Authority.Actor; actor.Kind != iam.ActorAgent ||
		actor.Name != "agent-cto" {
		t.Fatalf("the write is attributed to %+v, want the agent", actor)
	}
	if got := write(mine); got.Failed {
		t.Fatalf("an agent's write to its own artifact failed: %s", got.Output)
	}
	if role := r.data.lastCall().Authority.Role; role != artifacts.RoleOwner {
		t.Fatalf("an agent's write to its own artifact was stamped %q, want owner", role)
	}
	if origin := r.data.lastCall().Authority.Origin; origin != artifacts.OriginTool {
		t.Fatalf("a tool's write was stamped origin %q", origin)
	}
}

// A TURN PAST ITS DATASTORE OPS IS TOLD TO STOP, with nothing written.
func TestATurnPastItsDataOpsIsToldToStop(t *testing.T) {
	t.Parallel()
	r := newDataRig(t, artifactChart{}, requesters{})
	h := r.art.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	turn, _ := readerTurn(t)
	// THE RUN'S CALL LOG, recorded after each call as the runner records
	// it: what tells one call's operation from the next one's.
	turn.Calls = turnctx.NewCallLog()
	batch := func(i int) map[string]any {
		var ops []any
		for j := range artifacts.MaxBatchOps {
			ops = append(ops, map[string]any{"op": "set", "collection": "rows",
				"doc": fmt.Sprintf("r-%d-%d", i, j), "fields": map[string]any{"n": j}})
		}
		return map[string]any{"artifact": h.ID.String(), "ops": ops}
	}
	for i := range artifacts.TurnDataOps / artifacts.MaxBatchOps {
		args := batch(i)
		if got := r.call(t, turn, builtin.WriteArtifactDataTool, args); got.Failed {
			t.Fatalf("write %d of the allowance failed: %s", i, got.Output)
		}
		turn.CallLog().Record(builtin.WriteArtifactDataTool, toolargs.Of(nil, args))
	}
	// A RETRY OF THE LAST CALL IS THE SAME OPERATION, and is not charged
	// again: the turn lost the answer, not the allowance.
	last := batch(artifacts.TurnDataOps/artifacts.MaxBatchOps - 1)
	if got := r.call(t, turn, builtin.WriteArtifactDataTool, last); got.Failed {
		t.Fatalf("a retry of the last write was refused: %s", got.Output)
	}
	written := len(r.data.writes)
	over := r.call(t, turn, builtin.WriteArtifactDataTool, batch(99))
	if !over.Failed || !strings.Contains(over.Output, "turn_quota") {
		t.Fatalf("a write past the turn's ops answered %q", over.Output)
	}
	if len(r.data.writes) != written {
		t.Fatal("a write refused by the turn's guardrail reached the datastore")
	}
	// ANOTHER TURN STARTS AFRESH.
	next := turnFor(t, "agent-cto")
	next.RunID = "run-2"
	if got := r.call(t, next, builtin.WriteArtifactDataTool, batch(100)); got.Failed {
		t.Fatalf("the next turn's first write failed: %s", got.Output)
	}
}

// A SEAT READING OR WRITING AN ARTIFACT IS IN ITS ROOM: viewing on a read and
// editing on a write, focused where the call named.
func TestADataCallPutsTheSeatInTheRoom(t *testing.T) {
	t.Parallel()
	r := newDataRig(t, artifactChart{}, requesters{})
	h := r.art.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	r.data.collection("review", artifacts.ReadAll)
	r.data.row("review", "row-4", cto, `{"ok":true}`)
	turn, _ := readerTurn(t)
	r.call(t, turn, builtin.ReadArtifactDataTool, map[string]any{"artifact": h.ID.String(),
		"collection": "review", "doc": "row-4"})
	r.call(t, turn, builtin.WriteArtifactDataTool, map[string]any{"artifact": h.ID.String(),
		"ops": []any{map[string]any{"op": "delete", "collection": "review", "doc": "row-4"}}})
	want := []string{
		string(h.ID.Label()) + " agent-cto viewing review/row-4",
		string(h.ID.Label()) + " agent-cto editing review/row-4",
	}
	if got := r.presence.all(); !slices.Equal(got, want) {
		t.Fatalf("the seat was put in the room as %v, want %v", got, want)
	}
	// AND NAMING THE TURN IT WORKS IN, which a person opens from the room.
	r.presence.mu.Lock()
	defer r.presence.mu.Unlock()
	if !slices.Equal(r.presence.turns, []string{turn.RunID, turn.RunID}) || turn.RunID == "" {
		t.Fatalf("the touches named the turns %v, want the call's run %q twice",
			r.presence.turns, turn.RunID)
	}
}

// A CALLER WITH NO RELATION IS TOLD THE ARTIFACT DOES NOT EXIST, and nothing
// is read; a write to an artifact in the trash is refused and nothing written.
func TestADataCallIsDecidedOnTheArtifact(t *testing.T) {
	t.Parallel()
	r := newDataRig(t, artifactChart{}, requesters{})
	private := r.art.seed("founder", iam.ActorHuman, founder, nil, [2]string{"index.html", "<p>x</p>"})
	turn, _ := readerTurn(t)
	got := r.call(t, turn, builtin.ReadArtifactDataTool, map[string]any{"artifact": private.ID.String()})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalNotFound {
		t.Fatalf("a read with no relation answered %v %q", tools.RefusalOf(got), got.Output)
	}
	if len(r.data.readings) != 0 {
		t.Fatal("a read with no relation reached the datastore")
	}

	trashed := r.art.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>y</p>"})
	r.art.mu.Lock()
	held := r.art.heads[trashed.ID]
	held.Status = artifactsdomain.StatusTrashed
	r.art.heads[trashed.ID] = held
	r.art.mu.Unlock()
	refused := r.call(t, turn, builtin.WriteArtifactDataTool, map[string]any{
		"artifact": trashed.ID.String(), "ops": []any{map[string]any{"op": "delete",
			"collection": "rows", "doc": "a"}}})
	if !refused.Failed || len(r.data.writes) != 0 {
		t.Fatalf("a write to a trashed artifact answered %q and wrote %d", refused.Output,
			len(r.data.writes))
	}
}

// A SCHEMA CHANGE GOES TO THE WRITER WHOLE, stamped, which judges whether an
// agent loosened anything; its refusal is answered in the writer's own words.
func TestASchemaChangeIsTheWritersToJudge(t *testing.T) {
	t.Parallel()
	r := newDataRig(t, artifactChart{}, requesters{})
	h := r.art.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	turn, _ := readerTurn(t)
	args := map[string]any{"artifact": h.ID.String(), "collections": []any{
		map[string]any{"name": "review", "preset": "contribute", "columns": []any{
			map[string]any{"key": "status", "type": "select", "humans_only": true}}}}}
	if got := r.call(t, turn, builtin.SetArtifactSchemaTool, args); got.Failed {
		t.Fatalf("a schema change failed: %s", got.Output)
	}
	if len(r.data.configured) != 1 || r.data.configured[0][0].Name != "review" ||
		!r.data.configured[0][0].Columns[0].HumansOnly {
		t.Fatalf("the writer was handed %+v", r.data.configured)
	}
	if call := r.data.lastCall(); call.Authority.Actor.Kind != iam.ActorAgent ||
		call.Authority.Role != artifacts.RoleOwner {
		t.Fatalf("the schema change was stamped %+v", call.Authority)
	}
	r.data.verdict, r.data.reason = artifacts.DataForbidden, "an agent never loosens a rule"
	refused := r.call(t, turn, builtin.SetArtifactSchemaTool, args)
	if !refused.Failed || tools.RefusalOf(refused) != tools.RefusalForbidden ||
		!strings.Contains(refused.Output, "never loosens") {
		t.Fatalf("a refused schema change answered %v %q", tools.RefusalOf(refused), refused.Output)
	}
}

// A LIMIT ON WHAT AN ARTIFACT'S DATA MAY HOLD IS TOLD TO A PERSON IN A
// PERSON'S WORDS (ADR-0060), on a write and on a schema change alike.
//
// The applier refuses either for one verdict, `quota`, whichever limit it was:
// the company's storage settings, a collection's own row caps, or the
// engine's caps on one store. The tool's sentence names the applier's reason
// for the model; a person's surface answers the catalog's phrasing in its
// place, which is true of every one of those limits, where without one it
// answered nothing but its code's own sentence.
//
// Mutation: drop the phrasing from the verdict's quota arm, and both go red.
func TestADatastoreLimitIsToldToAPersonInTheirWords(t *testing.T) {
	t.Parallel()
	r := newDataRig(t, artifactChart{}, requesters{})
	h := r.art.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	turn, _ := readerTurn(t)
	r.data.verdict, r.data.reason = artifacts.DataQuota,
		"review would hold 501 rows and its maximum is 500"
	for name, args := range map[string]map[string]any{
		builtin.WriteArtifactDataTool: {"artifact": h.ID.String(), "ops": []any{
			map[string]any{"op": "set", "collection": "review", "doc": "a",
				"fields": map[string]any{"x": 1}}}},
		builtin.SetArtifactSchemaTool: {"artifact": h.ID.String(), "collections": []any{
			map[string]any{"name": "review", "preset": "contribute"}}},
	} {
		got := r.call(t, turn, name, args)
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalQuotaExceeded ||
			!strings.Contains(got.Output, "its maximum is 500") {
			t.Errorf("%s past a limit answered %v %q, want quota_exceeded naming the "+
				"applier's reason", name, tools.RefusalOf(got), got.Output)
		}
		if got.PersonRefusal != caveat.DatastoreQuotaReached() {
			t.Errorf("%s past a limit answers a person %q, want the catalog's words", name,
				got.PersonRefusal.Person())
		}
	}
}

// A DATA WRITE WHOSE OUTCOME IS UNKNOWN IS NOT REPORTED DONE.
func TestADataWriteWhoseOutcomeIsUnknownIsNotReportedDone(t *testing.T) {
	t.Parallel()
	r := newDataRig(t, artifactChart{}, requesters{})
	h := r.art.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	r.data.unknown = true
	turn, _ := readerTurn(t)
	got := r.call(t, turn, builtin.WriteArtifactDataTool, map[string]any{"artifact": h.ID.String(),
		"ops": []any{map[string]any{"op": "set", "collection": "rows", "doc": "a",
			"fields": map[string]any{"x": 1}}}})
	if !got.Failed || !strings.Contains(got.Output, "unknown") {
		t.Fatalf("a write of unknown outcome answered %q", got.Output)
	}
}

// A SEAT READING OR PUBLISHING AN ARTIFACT IS IN ITS ROOM TOO, the artifacts
// tools sharing the datastore tools' presence: viewing the version it read,
// editing the head it published over, and nowhere it was refused.
func TestAnArtifactCallPutsTheSeatInTheRoom(t *testing.T) {
	t.Parallel()
	r := newDataRig(t, artifactChart{}, requesters{})
	h := r.art.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	private := r.art.seed("founder", iam.ActorHuman, founder, nil, [2]string{"index.html", "<p>y</p>"})
	turn, _ := readerTurn(t)
	r.call(t, turn, builtin.GetArtifactTool, map[string]any{"artifact": h.ID.String()})
	r.call(t, turn, builtin.PublishArtifactTool, map[string]any{"artifact": h.ID.String(),
		"base_version": 1, "content": "<p>z</p>"})
	r.call(t, turn, builtin.GetArtifactTool, map[string]any{"artifact": private.ID.String()})
	want := []string{
		string(h.ID.Label()) + " agent-cto viewing /",
		string(h.ID.Label()) + " agent-cto editing /",
	}
	if got := r.presence.all(); !slices.Equal(got, want) {
		t.Fatalf("the seat was put in rooms as %v, want %v", got, want)
	}
}
