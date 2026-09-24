package main

import (
	"bytes"
	"context"
	"database/sql"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN EVICTION IS WRITTEN AS WHOEVER PRESSED IT, through the node's own
// tracker writer.
//
// The route handed the gate nothing about its caller, so the record — and the
// row every node applies it into, the one `crewlet retention` prints as
// "evicted by" — named THIS NODE's writer: "who stopped node-9 writing" had
// one answer, the node that happened to serve the request. Mutation: hand the
// gate's writer through unchanged and the row names the node.
func TestAnEvictionIsWrittenAsWhoPressedIt(t *testing.T) {
	t.Parallel()
	boot := bootstrapFor(t, 0)
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	gate := nativeNodes(e)
	if gate == nil {
		t.Fatal("the node runs no tracker, so the case would certify nothing")
	}

	by := iam.Actor{Name: "jane.doe", Kind: iam.ActorOperator,
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}
	if _, err := gate.EvictNode(t.Context(), uuid.NewString(), "node-9", by); err != nil {
		t.Fatalf("evict: %v", err)
	}
	stream := tracker.Domain{}.Stream().Name
	for deadline := time.Now().Add(10 * time.Second); ; {
		rows, err := tracker.Evictions(t.Context(), e.Backends().Store.Replicated(), stream)
		if err != nil {
			t.Fatalf("read the evictions: %v", err)
		}
		if len(rows) == 1 {
			if rows[0].NodeID != "node-9" || rows[0].By != by.Name {
				t.Errorf("the eviction of %s is recorded by %q, want %q — the "+
					"person who pressed it", rows[0].NodeID, rows[0].By, by.Name)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the eviction never applied: %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
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
// the log itself, because on the node that ran it the audit row it wrote first
// hides what the record says. Mutation: publish through the node's own writer
// and the record names the node.
func TestAReanchorIsPublishedAsWhoRanIt(t *testing.T) {
	t.Parallel()
	boot := bootstrapFor(t, 0)
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	stream := tracker.Domain{}.Stream().Name
	created, _, err := e.ReanchorStatus(t.Context(), stream)
	if err != nil {
		t.Fatalf("reanchor status: %v", err)
	}
	by := iam.Actor{Name: "jane.doe", Kind: iam.ActorOperator,
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}
	before := cursorGenerations(t, e)
	if _, err := e.Reanchor(t.Context(), engine.ReanchorRequest{
		Stream: stream, Confirm: created.UTC().Format(time.RFC3339), By: by,
	}); err != nil {
		t.Fatalf("reanchor: %v", err)
	}

	// ONLY THIS LOG'S CURSOR MOVED. Every other domain's was moved to a
	// position computed from the tracker's stream, which is a number space
	// none of their own logs has.
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
func TestAReanchorNamingNobodyIsRefused(t *testing.T) {
	t.Parallel()
	boot := bootstrapFor(t, 0)
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	stream := tracker.Domain{}.Stream().Name
	// A ROW THE RESET WOULD MOVE, so "before it touches anything" is
	// something this case can see: the reset is the step before the one a
	// nameless party is refused at inside the writer.
	if _, err := e.TrackerWriter().WriteDocument(t.Context(), "op-project",
		tracker.ProjectSubject("ENG"), "", tracker.Project{
			V: 1, Key: "ENG", Name: "Engineering",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed a project: %v", err)
	}
	version := func() int64 {
		t.Helper()
		var v int64
		for deadline := time.Now().Add(10 * time.Second); ; {
			err := e.Backends().Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
				return tx.QueryRowContext(t.Context(),
					`SELECT version FROM tracker_projects WHERE key = 'ENG'`).Scan(&v)
			})
			if err == nil {
				return v
			}
			if time.Now().After(deadline) {
				t.Fatalf("the project never applied: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	seeded := version()

	created, generation, err := e.ReanchorStatus(t.Context(), stream)
	if err != nil {
		t.Fatalf("reanchor status: %v", err)
	}
	if _, err := e.Reanchor(t.Context(), engine.ReanchorRequest{
		Stream: stream, Confirm: created.UTC().Format(time.RFC3339),
	}); err == nil {
		t.Fatal("a reanchor naming nobody ran")
	}
	if _, after, _ := e.ReanchorStatus(t.Context(), stream); after != generation {
		t.Errorf("the refused reanchor moved the generation from %d to %d",
			generation, after)
	}
	if after := version(); after != seeded {
		t.Errorf("the refused reanchor reset the project's version from %d to %d",
			seeded, after)
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

// A LOG WHOSE DOMAIN HAS NO REANCHOR OF ITS OWN IS REFUSED, naming the ones
// that do, before anything moves. The tracker's steps were run for any log
// named: they reset the TRACKER's rows and published a TRACKER generation for
// a log that was not the tracker's, and moved every cursor with them.
// Mutation: drop the refusal and the reanchor runs.
func TestAReanchorOfALogWithNoStepsOfItsOwnIsRefused(t *testing.T) {
	t.Parallel()
	boot := bootstrapFor(t, 0)
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	stream := pages.Domain{}.Stream().Name
	created, _, err := e.ReanchorStatus(t.Context(), stream)
	if err != nil {
		t.Fatalf("reanchor status: %v", err)
	}
	before := cursorGenerations(t, e)
	_, err = e.Reanchor(t.Context(), engine.ReanchorRequest{
		Stream: stream, Confirm: created.UTC().Format(time.RFC3339),
		By: iam.Actor{Name: "jane.doe", Kind: iam.ActorOperator,
			OperatorID: "token:ops"},
	})
	if err == nil || !strings.Contains(err.Error(), tracker.Domain{}.Stream().Name) {
		t.Fatalf("re-anchoring %s answered %v, want a refusal naming the logs "+
			"a reanchor applies to", stream, err)
	}
	if after := cursorGenerations(t, e); !maps.Equal(after, before) {
		t.Errorf("a refused reanchor moved cursors: %v, then %v", before, after)
	}
}
