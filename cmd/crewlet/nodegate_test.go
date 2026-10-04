package main

import (
	"bytes"
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// evictionLog is one identity-claiming log's eviction table, as its domain
// reads it: through the NODE handle, whose replicated peer it reaches itself.
type evictionLog interface {
	Name() string
	Evictions(ctx context.Context, db *store.DB) ([]statelog.EvictionRow, error)
}

// AN EVICTION IS WRITTEN AS WHOEVER PRESSED IT, ON EVERY LOG THE TRIM COUNTS
// NODES ON — through the gate this binary hands the API ([nodeGate]).
//
// Two failures, both through the wiring here. The route handed the gate nothing
// about its caller, so every log's record — and the row every node applies it
// into, the one `crewlet retention` prints as "evicted by" — named THIS NODE's
// writer: "who stopped node-9 writing" had one answer, the node that happened
// to serve the request. And the gate this binary handed over was the TRACKER's
// writer, which wrote the tracker's log alone: every other log went on
// counting the evicted node for ever. Mutation: hand the API the tracker's
// writer again, or drop the caller from the request, and a log either holds no
// row or names the node; drop the credential from the party a log's writer
// acts as, and that log's record does not carry it.
func TestAnEvictionIsWrittenAsWhoPressedItOnEveryLog(t *testing.T) {
	t.Parallel()
	e := bootedEngine(t)
	gate := nodeGate(e)
	if gate == nil {
		t.Fatal("the engine holds no node gate, so the case would certify nothing")
	}

	// A PERSON THROUGH A MACHINE TOKEN, so the author a log records and the
	// credential they pressed it through are two different values, and a
	// wiring that put one where the other goes is caught.
	by := iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, Via: "pat:0192f00d-0000-7000-8000-00000000000a",
		Grants: []iam.Grant{iam.GrantFleetOperate}}
	res, err := gate.Evict(t.Context(), engine.GateRequest{
		Node: "node-9", OpID: statelog.NewOpID(time.Now(), "evict-node-9"), By: by,
	})
	if err != nil || !res.Complete() {
		t.Fatalf("evict: %v (%+v)", err, res)
	}

	// EVERY IDENTITY-CLAIMING LOG, named: a log this build adds has to be
	// added here too, or the gesture answering for it goes unchecked.
	logs := []evictionLog{tracker.Domain{}, pages.Domain{}, iamdomain.Domain{}}
	var answered, want []string
	for _, d := range res.Domains {
		answered = append(answered, d.Domain)
	}
	for _, log := range logs {
		want = append(want, log.Name())
	}
	slices.Sort(answered)
	slices.Sort(want)
	if !slices.Equal(answered, want) {
		t.Fatalf("the gesture answered for %v, want every identity-claiming log %v",
			answered, want)
	}

	// THE RECORD ITSELF, which is what every other node applies: the author
	// on every log, and the credential on every log whose gate records one —
	// which the identity estate's does not, by its own rule
	// ([iamdomain.OperatorRecordVersion]: never on a gate), so it is not
	// asked of that log here.
	author := iam.ActorFor(by).Name
	for _, d := range res.Domains {
		record := gateRecord(t, e, d.Stream, d.OpID)
		wants := []string{`"actor":"` + author + `"`}
		if d.Domain != (iamdomain.Domain{}).Name() {
			wants = append(wants, `"operator_id":"`+by.Via+`"`)
		}
		for _, want := range wants {
			if !bytes.Contains(record, []byte(want)) {
				t.Errorf("%s's eviction record does not carry %s: %s", d.Domain,
					want, record)
			}
		}
	}
	for _, log := range logs {
		for deadline := time.Now().Add(10 * time.Second); ; {
			rows, err := log.Evictions(t.Context(), e.Backends().Store)
			if err != nil {
				t.Fatalf("read %s's evictions: %v", log.Name(), err)
			}
			if len(rows) == 1 {
				if rows[0].NodeID != "node-9" || rows[0].By != author {
					t.Errorf("%s records the eviction of %s by %q, want %q — the "+
						"person who pressed it", log.Name(), rows[0].NodeID,
						rows[0].By, author)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the eviction never applied on %s: %+v", log.Name(), rows)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// A REANCHOR IS PUBLISHED AS WHOEVER RAN IT.
//
// The generation record is what every OTHER node applies into its audit row
// of the reanchor, and it was published through this node's own writer — so
// every peer recorded the node as having declared the log's history
// unreachable, while the node that ran it recorded the person. It is published
// as the operator now: the author in the field the record always had, and
// their credential as the operator id every tracker record carries. Read off
// the log itself, because what a peer applies is the record and not this
// node's row. Mutation: publish through the node's own writer, or drop the
// credential from the facts, and the record names the node.
func TestAReanchorIsPublishedAsWhoRanIt(t *testing.T) {
	t.Parallel()
	e := bootedEngineAt(t, reanchorCompanyYAML, time.Now())
	stream := tracker.Domain{}.Stream().Name
	before := cursorGenerations(t, e)
	view := rebuiltLog(t, e, tracker.Domain{}.Stream())

	by := iam.Actor{Name: "jane.doe", Kind: iam.ActorOperator,
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}
	if _, err := e.Reanchor(t.Context(), engine.ReanchorRequest{
		Stream: stream, Confirm: statelog.ConfirmationOf(view.CreatedAt), By: by,
	}); err != nil {
		t.Fatalf("reanchor: %v", err)
	}

	// ONLY THIS LOG'S CURSOR MOVED. Every other domain's is in a number
	// space none of this log's positions belongs to.
	after := cursorGenerations(t, e)
	for log, generation := range before {
		switch {
		case log == stream && after[log] != generation+1:
			t.Errorf("the re-anchored log is at generation %d, want %d",
				after[log], generation+1)
		case log != stream && after[log] != generation:
			t.Errorf("re-anchoring %s moved %s from generation %d to %d",
				stream, log, generation, after[log])
		}
	}

	broker, ok := e.Backends().Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the engine's queue is %T, not the embedded broker", e.Backends().Queue)
	}
	log, err := broker.DomainLog(t.Context(), stream)
	if err != nil {
		t.Fatalf("open the tracker log: %v", err)
	}
	last, err := log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	for seq := last; seq > 0; seq-- {
		_, payload, _, present, err := log.At(t.Context(), seq)
		if err != nil {
			t.Fatalf("read record %d: %v", seq, err)
		}
		if !present || !bytes.Contains(payload, []byte(`"op":"generation"`)) {
			continue
		}
		// THE BODY, past the frame's signature, which is binary.
		body := payload[max(bytes.IndexByte(payload, '{'), 0):]
		for _, want := range []string{`"reanchored_by":"jane.doe"`,
			`"actor":"jane.doe"`, `"actor_kind":"operator"`,
			`"operator_id":"pat:0192f00d-0000-7000-8000-00000000000a"`} {
			if !bytes.Contains(body, []byte(want)) {
				t.Errorf("the generation record does not carry %s: %s", want, body)
			}
		}
		return
	}
	t.Fatal("the reanchor published no generation record")
}

// AND A REANCHOR THAT NAMES NOBODY IS REFUSED before it touches anything: it
// is the one gesture that declares a log's history unreachable, and an audit
// row naming nobody is the one it would leave on every node.
//
// ON A LOG THERE IS SOMETHING TO RE-ANCHOR, so the refusal is about the name
// and nothing else: the same request naming somebody then runs.
func TestAReanchorNamingNobodyIsRefused(t *testing.T) {
	t.Parallel()
	e := bootedEngine(t)
	stream := tracker.Domain{}.Stream().Name
	view := rebuiltLog(t, e, tracker.Domain{}.Stream())
	confirm := statelog.ConfirmationOf(view.CreatedAt)

	_, err := e.Reanchor(t.Context(), engine.ReanchorRequest{Stream: stream, Confirm: confirm})
	if err == nil || !strings.Contains(err.Error(), "who ran it") {
		t.Fatalf("a reanchor naming nobody answered %v, want a refusal saying "+
			"who has to be named", err)
	}
	if again, err := e.ReanchorStatus(t.Context(), stream); err != nil ||
		again.Generation != view.Generation || again.Case != statelog.ReanchorRecreated {
		t.Fatalf("after the refusal the log stands at generation %d with case %q "+
			"(%v), want %d and still %q — the refusal touched it",
			again.Generation, again.Case, err, view.Generation, statelog.ReanchorRecreated)
	}
	if _, err := e.Reanchor(t.Context(), engine.ReanchorRequest{
		Stream: stream, Confirm: confirm,
		By: iam.Actor{Name: "jane.doe", Kind: iam.ActorOperator, OperatorID: "token:ops"},
	}); err != nil {
		t.Fatalf("the same reanchor naming somebody was refused too, so the "+
			"first refusal was not about the name: %v", err)
	}
}

// gateRecord is the record one log holds under a gate's operation id, as the
// broker stores it — its signed frame, whose JSON body is carried verbatim.
func gateRecord(t *testing.T, e *engine.Engine, stream, opID string) []byte {
	t.Helper()
	broker, ok := e.Backends().Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the engine's queue is %T, not the embedded broker", e.Backends().Queue)
	}
	log, err := broker.DomainLog(t.Context(), stream)
	if err != nil {
		t.Fatalf("open %s: %v", stream, err)
	}
	last, err := log.End(t.Context())
	if err != nil {
		t.Fatalf("read %s's end: %v", stream, err)
	}
	for seq := last; seq > 0; seq-- {
		_, payload, _, present, err := log.At(t.Context(), seq)
		if err != nil {
			t.Fatalf("read %s record %d: %v", stream, seq, err)
		}
		if present && bytes.Contains(payload, []byte(`"op_id":"`+opID+`"`)) {
			return payload
		}
	}
	t.Fatalf("%s holds no record under the gate's operation %s", stream, opID)
	return nil
}

// bootedEngine is one node running the fixture company on its own store and
// embedded broker.
func bootedEngine(t *testing.T) *engine.Engine {
	t.Helper()
	return bootedEngineAt(t, companyYAML, time.Time{})
}

// reanchorCompanyYAML is companyYAML with a unit whose project and knowledge
// space a boot at an activation files on the tracker's and the knowledge
// base's logs — so more than one log holds a cursor before anything else
// writes, and a reanchor moving a log it did not name could be seen.
const reanchorCompanyYAML = companyYAML + `units:
  - name: Engineering
    id: engineering
    project: ENG
    space: ENG
`

// bootedEngineAt is [bootedEngine] over doc, booted at the activation instant
// activatedAt — zero for a company no activation has named.
func bootedEngineAt(t *testing.T, doc string, activatedAt time.Time) *engine.Engine {
	t.Helper()
	company, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: bootstrapFor(t, 0),
		Company: company, ActivatedAt: activatedAt})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

// rebuiltLog deletes one domain's log under the running node and creates it
// again empty — the log a reanchor exists for — and waits until the node says
// a reanchor would follow it, answering what it says.
func rebuiltLog(t *testing.T, e *engine.Engine, spec statelog.StreamSpec) engine.ReanchorView {
	t.Helper()
	q, ok := e.Backends().Queue.(interface{ Conn() *nats.Conn })
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend — there is no "+
			"broker to rebuild a log on", e.Backends().Queue)
	}
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteStream(t.Context(), spec.Name); err != nil {
		t.Fatalf("delete %s: %v", spec.Name, err)
	}
	// The broker keeps creation instants at nanosecond precision and the
	// identity compares microseconds; a rebuild inside the same microsecond
	// as the create would compare equal, so it waits one out.
	time.Sleep(2 * time.Millisecond)
	if _, err := js.CreateStream(t.Context(), natsjs.StreamConfig{
		Name: spec.Name, Subjects: spec.Subjects,
		MaxBytes: 16 << 20, Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("rebuild %s: %v", spec.Name, err)
	}
	var view engine.ReanchorView
	for deadline := time.Now().Add(20 * time.Second); ; {
		view, err = e.ReanchorStatus(t.Context(), spec.Name)
		if err == nil && view.Case == statelog.ReanchorRecreated {
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rebuilt %s never read as recreated: %+v, %v", spec.Name, view, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// cursorGenerations is every log's applied generation on one node.
func cursorGenerations(t *testing.T, e *engine.Engine) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	if err := e.Backends().Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT stream, generation FROM statelog_cursor`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var stream string
			var generation int64
			if err := rows.Scan(&stream, &generation); err != nil {
				return err
			}
			out[stream] = generation
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the cursors: %v", err)
	}
	if len(out) < 2 {
		t.Fatalf("precondition: the node holds %d cursor(s), so a reanchor "+
			"moving more than its own log could not be seen", len(out))
	}
	return out
}
