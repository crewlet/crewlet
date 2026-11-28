package estate

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask/signedasktest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY OPERATION DECLARES HOW LARGE ITS ANSWER GROWS, so none builds an answer
// past the one reply a data node sends: a read answering rows reads them to a
// declared bound through statelog.RowsOf (or its domain's own filler, to a
// bound no larger), a capped list holds a count of rows of a named size within
// one, and every other read is one bounded value or one entry per key asked.
// A write answers what it wrote. A read answering a list or a map is not one
// value. No read is exempt: none answers whole.
//
// Mutations, each red: drop one read's declaration (access.commands'); define
// a read without one (a temporary op); declare a rows bound past
// statelog.AnswerBytes; declare a read answering a slice as one value; cap a
// list whose count times its row bytes passes statelog.AnswerBytes.
func TestEveryOperationDeclaresHowLargeItsAnswerGrows(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec, shape := registry[name], registry[name].answer
		switch {
		case shape.kind == "":
			t.Errorf("%s declares no bound on its answer: declare rowsTo (and read through "+
				"statelog.RowsOf), cappedAt, oneAnswer or perAsked", name)
		case !shape.kind.valid():
			t.Errorf("%s declares the answer kind %q, which this build does not know", name, shape.kind)
		case spec.class != opRead && shape.kind != answerOne:
			t.Errorf("%s is a write declaring a %s answer, and a write answers what it wrote", name,
				shape.kind)
		case (shape.kind == answerRows || shape.kind == answerOwnBound) &&
			(shape.bytes <= 0 || shape.bytes > statelog.AnswerBytes):
			t.Errorf("%s reads its rows to %d bytes, outside (0, %d]", name, shape.bytes,
				statelog.AnswerBytes)
		case shape.kind == answerCapped && (shape.rows <= 0 || shape.bytes <= 0 ||
			shape.rows > statelog.AnswerBytes/shape.bytes):
			t.Errorf("%s caps %d rows of %d bytes, past the %d an answer holds: read it through "+
				"statelog.RowsOf", name, shape.rows, shape.bytes, statelog.AnswerBytes)
		case spec.class == opRead && shape.kind == answerOne && collection(spec.result):
			t.Errorf("%s answers a %s and declares one value: it answers rows, a capped list or one "+
				"entry per key asked", name, spec.result)
		}
	}
}

// collection reports a type that is itself a list or a map.
func collection(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return t.Elem().Kind() != reflect.Uint8
	}
	return false
}

// A READ'S DECLARED BOUND REACHES THE READER THAT SCANS ITS ROWS, on every run:
// the declaration is the bound, not a comment beside it, and only a read
// answering rows carries one.
//
// Mutations, each red: carry no bound for a rows read; carry one for a read
// declared one value.
func TestARowsReadCarriesItsDeclaredBoundToItsReader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		shape answerShape
		kept  int
	}{
		{answerShape{kind: answerRows, bytes: 10}, 2},
		{answerShape{kind: answerOne}, 3},
	} {
		held, kept := statelog.RowsOf(tc.shape.reading(t.Context())), 0
		for range 3 {
			if held.Take(5) {
				kept++
			}
		}
		if kept != tc.kept {
			t.Errorf("a %s read kept %d rows of 5 bytes, want %d", tc.shape.kind, kept, tc.kept)
		}
	}
}

// pagedCommands and pagedSessions are a data node's access and executor
// readers paging their rows as the domains' own do: by statelog.RowsOf, a cut
// page's cursor naming the first row it left out. What the domains' cuts are is
// held in their own packages; these hold what crosses the lane.
type pagedCommands struct {
	AccessReader
	rows []accessdomain.CommandRow
}

type pagedSessions struct {
	ExecutorReader
	rows []executordomain.Session
}

// pageOf is one page of rows from cursor, cut where held refuses one.
func pageOf[R any](ctx context.Context, rows []R, cursor string) ([]R, string) {
	from, _ := strconv.Atoi(cursor)
	held := statelog.RowsOf(ctx)
	for i := from; i < len(rows); i++ {
		if !held.TakeEncoded(rows[i]) {
			return rows[from:i], strconv.Itoa(i)
		}
	}
	return rows[from:], ""
}

func (p pagedCommands) Commands(ctx context.Context, q accessdomain.AuditQuery,
	_ statelog.Freshness) (accessdomain.CommandPage, error) {
	rows, next := pageOf(ctx, p.rows, q.Cursor)
	return accessdomain.CommandPage{Rows: rows, Next: next}, nil
}

func (p pagedSessions) Sessions(ctx context.Context, _, cursor string, _ int,
	_ statelog.Freshness) (executordomain.Page, error) {
	rows, next := pageOf(ctx, p.rows, cursor)
	return executordomain.Page{Sessions: rows, Next: next}, nil
}

// pagedCopy serves both readers from every copy.
type pagedCopy struct {
	commands pagedCommands
	sessions pagedSessions
}

func (p pagedCopy) For(context.Context, string) (Backend, bool) {
	return Backend{
		Access:   AccessHalf{Reader: p.commands},
		Executor: ExecutorHalf{Reader: p.sessions},
		Reached:  func(context.Context, statelog.Position) error { return nil },
		Answers:  func(context.Context) bool { return true },
	}, true
}

// A ROUTED READ WHOSE ROWS PASS ITS BOUND IS CUT ON THE DATA NODE, AND ITS CALLER
// READS THE REST ON THE NEXT PAGE: the bound an operation declares reaches the
// reader serving it off the wire, the cut page's cursor crosses back, and
// paging by it reads every row once, in order. For the two reads whose rows
// carry the most a seat's node asks of the access and executor logs (a sealed
// command, a session with its question's excerpt); the bound is shrunk so a
// page holds two rows.
//
// Mutations, each red: carry no declared bound to the reader (define's
// checked); drop access.commands' or executor.sessions' declaration.
func TestARoutedReadPastItsBoundIsCutAndPagedToTheEnd(t *testing.T) {
	commands := make([]accessdomain.CommandRow, 5)
	sessions := make([]executordomain.Session, 5)
	for i := range 5 {
		commands[i] = accessdomain.CommandRow{Handle: "sre", ID: strconv.Itoa(i),
			CommandSealed: strings.Repeat("s", 1000)}
		sessions[i] = executordomain.Session{Handle: "sre", Turn: strconv.Itoa(i),
			AskExcerpt: strings.Repeat("q", 1000)}
	}
	node := pagedCopy{commands: pagedCommands{rows: commands}, sessions: pagedSessions{rows: sessions}}
	f := &fleet{broker: memory.NewBroker()}
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{Taken: signedasktest.NewLedger(), Self: "data-a", Local: node,
		Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	r, err := NewRouter(RouterOptions{Self: "agent-1", Queue: f.start(t),
		Placement: &fakePlacement{nodes: []string{"data-a"}}, Git: &fakePlacement{},
		Session: NewSession(), Timing: Production(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	twoRows := func(spec *opSpec, row any) {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		declared := spec.answer
		if declared.kind != answerRows {
			t.Fatalf("%s declares a %q answer, want rows", spec.name, declared.kind)
		}
		spec.answer.bytes = 2*len(raw) + len(raw)/2
		t.Cleanup(func() { spec.answer = declared })
	}
	twoRows(opAccessCommands.spec, commands[0])
	twoRows(opExecutorSessions.spec, sessions[0])

	for _, tc := range []struct {
		name string
		page func(cursor string) (ids []string, next string, err error)
	}{
		{"access.commands", func(cursor string) ([]string, string, error) {
			page, err := r.AccessReads().Commands(t.Context(), accessdomain.AuditQuery{Cursor: cursor},
				statelog.Freshness{Level: statelog.ReadStale})
			var ids []string
			for _, row := range page.Rows {
				ids = append(ids, row.ID)
			}
			return ids, page.Next, err
		}},
		{"executor.sessions", func(cursor string) ([]string, string, error) {
			page, err := r.ExecutorReads().Sessions(t.Context(), "sre", cursor, 0,
				statelog.Freshness{Level: statelog.ReadStale})
			var ids []string
			for _, row := range page.Sessions {
				ids = append(ids, row.Turn)
			}
			return ids, page.Next, err
		}},
	} {
		var got []string
		var sizes []int
		cursor := ""
		for range 10 {
			ids, next, err := tc.page(cursor)
			if err != nil {
				t.Fatalf("%s: page at %q: %v", tc.name, cursor, err)
			}
			got, sizes = append(got, ids...), append(sizes, len(ids))
			if cursor = next; cursor == "" {
				break
			}
		}
		if want := []string{"0", "1", "2", "3", "4"}; !slices.Equal(got, want) ||
			!slices.Equal(sizes, []int{2, 2, 1}) {
			t.Errorf("%s: paged %v in pages of %v, want %v in pages of [2 2 1]", tc.name, got, sizes, want)
		}
	}
}
