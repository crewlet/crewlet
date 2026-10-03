package usage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// openStore is a fresh node with both estates migrated.
func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return db
}

// loopback is the domain's write authority reduced to what these tests are
// about: every record a publisher sends is applied at once, at the next
// position, by the shipped applier into the replicated estate of `into`.
//
// Nothing about arbitration is modelled because the domain has none — every
// write is additive — and the outcome it answers is scripted, so a test can
// say what an unresolved publish does to the next tick.
type loopback struct {
	t    *testing.T
	into *store.DB

	// node is the publishing node, which the framework's write authority
	// stamps on every record as its writer ([statelog.Stamp]).
	node string

	mu       sync.Mutex
	seq      uint64
	requests []statelog.Request
	payloads [][]byte

	// unresolved answers OutcomeUnknown to this many publishes, WITHOUT
	// applying them — the case where the broker never said.
	unresolved int

	// refuse, when set, answers a publish with the refusal it returns
	// instead of applying it — handed whether the record's subject is one
	// the log does not hold yet, which is the one thing a compacted log at
	// its ceiling tells apart: it still takes a record that replaces its
	// subject's last one.
	refuse func(fresh bool) error

	// held is every subject the log holds a record on.
	held map[string]bool
}

func (l *loopback) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if req.Pattern != statelog.PatternAdditive {
		l.t.Errorf("a usage record was published with pattern %v — one writer "+
			"per subject has nothing to arbitrate", req.Pattern)
	}
	// THE STAMP THE FRAMEWORK HANDS EVERY DECISION: this node as the
	// writer, generation 1 as the loopback's only one.
	decision, err := req.Decide(nil, statelog.Stamp{Writer: l.node, Gen: 1})
	if err != nil {
		return statelog.Result{}, err
	}
	env, err := usage.Domain{}.Envelope(decision.Payload)
	if err != nil {
		return statelog.Result{}, err
	}
	if env.Writer != l.node || env.Gen != 1 {
		l.t.Errorf("a usage record went out naming writer %q at generation %d, "+
			"decided under %q at 1 — the framework refuses it", env.Writer, env.Gen, l.node)
	}
	l.requests = append(l.requests, req)
	l.payloads = append(l.payloads, decision.Payload)
	if l.refuse != nil {
		if err := l.refuse(!l.held[env.Subject.String()]); err != nil {
			return statelog.Result{}, err
		}
	}
	if l.unresolved > 0 {
		l.unresolved--
		return statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: req.OpID}, nil
	}
	l.seq++
	rec := statelog.Record{
		Envelope: env,
		Position: statelog.Position{Stream: usage.Domain{}.Stream().Name,
			Generation: 1, Seq: l.seq},
		Payload: decision.Payload,
	}
	if err := applyRecord(ctx, l.into, rec); err != nil {
		return statelog.Result{}, err
	}
	if l.held == nil {
		l.held = map[string]bool{}
	}
	l.held[env.Subject.String()] = true
	return statelog.Result{Outcome: statelog.OutcomeApplied, OpID: req.OpID}, nil
}

func (l *loopback) sent() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.requests)
}

// applyRecord runs the shipped applier over one record in one transaction, as
// the framework's loop does.
func applyRecord(ctx context.Context, db *store.DB, rec statelog.Record) error {
	return db.Replicated().Tx(ctx, func(tx *sql.Tx) error {
		_, err := usage.NewApplier().Apply(ctx, tx, rec, statelog.ApplyOptions{
			MaxVariables: db.Replicated().Caps().MaxVariables,
		})
		return err
	})
}

// recordAt encodes a record and wraps it at a position, for the apply cases.
func recordAt(t *testing.T, r usage.Record, seq uint64) statelog.Record {
	t.Helper()
	body, err := r.Encode()
	if err != nil {
		t.Fatalf("encode %s: %v", r.Subject, err)
	}
	env, err := usage.Domain{}.Envelope(body)
	if err != nil {
		t.Fatalf("envelope %s: %v", r.Subject, err)
	}
	return statelog.Record{
		Envelope: env,
		Position: statelog.Position{Stream: usage.Domain{}.Stream().Name,
			Generation: 1, Seq: seq},
		Payload: body,
	}
}

// dump is every row of every table the domain writes, as text, in key order
// — the version column left out, because it is the record's POSITION and a
// republish of the same content lands at a new one by design.
func dump(t *testing.T, db *store.DB) map[string][]string {
	t.Helper()
	orders := map[string]string{
		"usage_turns":         "day, node, agent_id",
		"usage_tokens":        "day, node, agent_id, phase, worker, model, provider_key",
		"usage_reads":         "day, node, agent_id, backend, page_id, via",
		"usage_schedule_runs": "day, node, scope_type, scope_id, name, fired_at, target",
	}
	out := map[string][]string{}
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		for table, order := range orders {
			rows, err := tx.QueryContext(t.Context(), `SELECT * FROM `+table+` ORDER BY `+order)
			if err != nil {
				return err
			}
			cols, err := rows.Columns()
			if err != nil {
				_ = rows.Close()
				return err
			}
			for rows.Next() {
				values := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range values {
					ptrs[i] = &values[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					_ = rows.Close()
					return err
				}
				var cells []string
				for i, c := range cols {
					if c == "version" {
						continue
					}
					cells = append(cells, fmt.Sprintf("%s=%v", c, values[i]))
				}
				out[table] = append(out[table], strings.Join(cells, " "))
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("dump the usage tables: %v", err)
	}
	return out
}

// events writes a node's own records the way the observe writer does: the
// promoted columns come from the tags and the spend from the payload.
type events struct {
	t   *testing.T
	db  *store.DB
	seq int
}

func (e *events) add(typ string, at time.Time, agent, role string, payload map[string]any) {
	e.t.Helper()
	e.seq++
	body, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatalf("encode a payload: %v", err)
	}
	tags := map[string]string{"agent_id": agent, "agent_role": role}
	if turn, ok := payload["turn_id"].(string); ok {
		tags["turn_id"] = turn
	}
	if failed, ok := payload["failed"].(bool); ok && failed {
		tags["failed"] = "true"
	}
	if err := e.db.Events().Append(e.t.Context(), store.EventRecord{
		ID: fmt.Sprintf("ev-%04d", e.seq), Type: typ, Source: role, Category: "agent",
		Time: at, Actor: role, Tags: tags, Payload: body,
	}); err != nil {
		e.t.Fatalf("append %s: %v", typ, err)
	}
}

// phase records one completed phase.
func (e *events) phase(at time.Time, agent, turn, ph, decision string, input, output int) {
	e.add("agent_phase_completed", at, agent, "Dev", map[string]any{
		"agent_id": agent, "role": "Dev", "turn_id": turn, "phase": ph,
		"model": "m-1", "provider_key": "primary", "decision": decision,
		"input_tokens": input, "output_tokens": output,
		"total_tokens": input + output, "cache_read_tokens": input / 2,
	})
}

// turn records a turn's two completion records.
func (e *events) turn(at time.Time, agent, handle, turn string, failed bool, duration time.Duration) {
	e.add("agent_turn_completed", at, agent, "Dev", map[string]any{
		"agent_id": agent, "role": "Dev", "turn_id": turn, "failed": failed,
	})
	e.add("turn_completed", at, agent, "Dev", map[string]any{
		"agent_id": agent, "agent_handle": handle, "role": "Dev", "turn_id": turn,
		"duration_ms": duration.Milliseconds(),
	})
}

// read records one knowledge read of some pages.
func (e *events) read(at time.Time, agent, turn, via, query string, pages ...string) {
	list := make([]map[string]any, 0, len(pages))
	for i, p := range pages {
		list = append(list, map[string]any{"id": p, "rank": i + 1})
	}
	e.add("knowledge_read", at, agent, "Dev", map[string]any{
		"agent_id": agent, "agent_handle": "dev", "role": "Dev", "turn_id": turn,
		"via": via, "backend": "native", "query": query, "pages": list,
	})
}

// sortedKeys is a map's keys in order, for a stable failure message.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
}
