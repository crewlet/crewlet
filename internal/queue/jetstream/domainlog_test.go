package jetstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// A LOG CANNOT REMOVE AN INTERIOR SEQUENCE, and this is what keeps that a
// TESTED INVARIANT rather than a comment.
//
// The strict replay protocol treats contiguity as an invariant and a hole as a
// fault — which is only sound because six settings on the stream make an
// interior removal impossible. Each of them fails silently if it drifts: an
// age bound or a per-subject limit turns the log into a keyed table and the
// strict loop stalls on the first ordinary write; a delete or a rollup erases
// a committed record from under every node's applier; a discard-old policy
// drops the oldest record to accept a new one, which is the company's own
// history going quietly missing under load.
func TestMutationStreamCannotRemoveInteriorSequences(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, DomainStream{
		Name:       "CREWLET_STRICT_LOG",
		Subjects:   []string{"crewlet.strict.log.>"},
		MaxBytes:   16 << 20,
		Duplicates: 2 * time.Minute,
	})
	s, err := q.js.Stream(t.Context(), "CREWLET_STRICT_LOG")
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	// THE LIVE CONFIGURATION, not the spec that was sent: what matters is
	// what the broker is running, and a field the server ignored or
	// defaulted differently is exactly the drift this case is for.
	info, err := s.Info(t.Context())
	if err != nil {
		t.Fatalf("read the stream info: %v", err)
	}
	cfg := info.Config

	if cfg.MaxMsgsPerSubject > 0 {
		t.Errorf("the log keeps %d message(s) per subject — a per-subject limit "+
			"removes an INTERIOR sequence wherever it sits, and the strict loop "+
			"stalls on the first one for ever", cfg.MaxMsgsPerSubject)
	}
	if cfg.MaxAge != 0 {
		t.Errorf("the log bounds its age at %s — an age bound deletes records a "+
			"node has not applied, and there is no node whose word it takes",
			cfg.MaxAge)
	}
	if !cfg.DenyDelete {
		t.Error("the log permits message deletion — a key-value bucket gets " +
			"DenyDelete for free and a plain stream does not, so without it any " +
			"client holding the API can erase a committed record from under " +
			"every node's applier")
	}
	if cfg.AllowRollup {
		t.Error("the log permits a rollup — a rollup is a delete of everything " +
			"below it, spelled as a publish")
	}
	if cfg.Discard != jetstream.DiscardNew {
		t.Errorf("the log discards %v — a full log must REFUSE the append "+
			"loudly, because a dropped record is nobody's problem until the day "+
			"somebody reads the gap", cfg.Discard)
	}
	if cfg.AllowDirect || cfg.MirrorDirect {
		t.Errorf("the log allows direct gets (direct=%v mirror=%v) — the "+
			"framework asks the broker for a subject's last message to decide "+
			"whether a write raced, and a direct get is served from replica "+
			"state that may be behind an acknowledged write, so the "+
			"discriminator would answer \"no message here\" for a message the "+
			"quorum already has", cfg.AllowDirect, cfg.MirrorDirect)
	}
	if cfg.Retention != jetstream.LimitsPolicy {
		t.Errorf("the log retains under %v — an interest policy removes a "+
			"message once every filtered consumer has acked it, so one "+
			"permanently dead node pins the whole log, and it DROPS a publish "+
			"no consumer's filter covers", cfg.Retention)
	}
}

// AND NOTHING TRIMS PER SUBJECT.
//
// A keep-newest purge would remove the trimmed-anchor hazard outright, and it
// is rejected on cost: the option is mutually exclusive with a
// purge-by-sequence, so a keep-one trim must be issued PER SUBJECT — a hundred
// thousand API calls per trim tick against one stream-wide call — and it would
// destroy the contiguity invariant the strict protocol rests on.
//
// A static walk rather than a comment, because the option is exactly what
// somebody reaches for on meeting the hazard for the first time.
func TestNothingPurgesPerSubject(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	for _, dir := range []string{
		filepath.Join(root, "internal", "queue", "jetstream"),
		filepath.Join(root, "internal", "statelog"),
	} {
		fset := token.NewFileSet()
		for path, file := range parseEveryGoFile(t, fset, dir) {
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "WithPurgeSubject", "WithPurgeKeep":
					t.Errorf("%s calls %s — a keep-newest purge is mutually "+
						"exclusive with purging by sequence, so it must be "+
						"issued per subject, and it removes the interior "+
						"sequences the strict replay protocol treats as an "+
						"invariant", relative(root, path), sel.Sel.Name)
				}
				return true
			})
		}
	}
}

// parseEveryGoFile parses every .go file in dir, keyed by the path a reader
// would type.
//
// Not parser.ParseDir, which Go 1.25 deprecated because it associates files
// with packages without consulting build tags. A static walk wants EVERY file
// regardless of tags — a purge hidden behind a //go:build line is still a
// purge — so the listing is explicit rather than inherited from a helper whose
// behaviour here was a happy accident.
func parseEveryGoFile(t *testing.T, fset *token.FileSet, dir string) map[string]*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	files := make(map[string]*ast.File, len(paths))
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files[path] = file
	}
	if len(files) == 0 {
		t.Fatalf("no Go files under %s — the walk would pass by scanning nothing", dir)
	}
	return files
}

// relative renders a path the way a person reading the failure would type it:
// from the module root, where every make target and CI step runs.
func relative(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(rel)
	}
	return path
}

// A DOMAIN LOG ANSWERS AN EMPTY SUBJECT AS A FACT, NOT AS A FAILURE.
//
// It is what tells a trimmed anchor from a lost race, and the two have
// opposite remedies: one publishes at an expectation of zero and the other
// must never. A caller that had to re-derive that distinction from an error
// would take the trimmed-anchor branch on a broker that was merely
// unreachable.
func TestAnEmptySubjectIsAFactRatherThanAnError(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}

	seq, found, err := log.LastSeq(t.Context(), "crewlet.probe.log.object.never-written")
	if err != nil {
		t.Fatalf("LastSeq on an empty subject = %v, want no error — \"no "+
			"message here\" is the answer rather than a failure", err)
	}
	if found || seq != 0 {
		t.Fatalf("LastSeq on an empty subject = (%d, %v), want (0, false)", seq, found)
	}

	// AND AN OCCUPIED ONE ANSWERS ITS SEQUENCE.
	at, dup, err := log.Append(t.Context(), "crewlet.probe.log.object.a", "op-1", nil, []byte("x"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if dup {
		t.Error("a first append was reported as a duplicate")
	}
	seq, found, err = log.LastSeq(t.Context(), "crewlet.probe.log.object.a")
	if err != nil || !found || seq != at {
		t.Fatalf("LastSeq after an append = (%d, %v, %v), want (%d, true, nil)",
			seq, found, err, at)
	}
}

// A CONDITIONAL APPEND IS ARBITRATED BY THE BROKER, and a nil expectation is
// no expectation at all.
//
// The pointer is what makes those two states distinguishable: zero is a real
// expectation meaning "this subject holds nothing", and it is the one branch
// where being wrong costs a silent lost update rather than a refused write.
func TestAnExpectationIsArbitratedAndNilIsNoExpectation(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	subject := "crewlet.probe.log.object.a"

	zero := uint64(0)
	first, _, err := log.Append(t.Context(), subject, "op-1", &zero, []byte("one"))
	if err != nil {
		t.Fatalf("an expectation of zero on an empty subject: %v", err)
	}
	// The same expectation now names a subject that holds something.
	if _, _, err := log.Append(t.Context(), subject, "op-2", &zero, []byte("two")); err == nil {
		t.Fatal("an expectation of zero was accepted on an occupied subject — " +
			"the broker is the only party that sees every writer, and this is " +
			"the whole concurrency control")
	}
	// The current one is accepted.
	if _, _, err := log.Append(t.Context(), subject, "op-3", &first, []byte("three")); err != nil {
		t.Fatalf("the current expectation was refused: %v", err)
	}
	// AND NO EXPECTATION IS NOT AN EXPECTATION OF ZERO.
	if _, _, err := log.Append(t.Context(), subject, "op-4", nil, []byte("four")); err != nil {
		t.Fatalf("an append with no expectation was refused: %v — a nil "+
			"expectation must send no header at all, which is what the additive "+
			"pattern is", err)
	}
}

// watchFilter is the subjects the watch cases watch: one kind of the probe
// log's objects, so another kind's records are outside it.
const watchFilter = "crewlet.probe.log.repo.>"

// watchAwait bounds every wait for what a watch tells its handler. It only
// decides how long a broker that never delivers is given before a case says
// so — a delivery takes milliseconds — and it covers a consumer replaced only
// once its member's silence has outlasted two heartbeats.
const watchAwait = 30 * time.Second

// heardCall is one call a watch made to its handler: a subject and its
// sequence, or a Gap and the position it reaches.
type heardCall struct {
	gap     bool
	subject string
	seq     uint64
}

// gapThrough is a Gap reaching through, as [watchHeard] records one.
func gapThrough(through uint64) heardCall { return heardCall{gap: true, seq: through} }

func (c heardCall) String() string {
	if c.gap {
		return fmt.Sprintf("gap through %d", c.seq)
	}
	return fmt.Sprintf("%s@%d", c.subject, c.seq)
}

// watchHeard records every call a watch makes to its handler, in the order it
// made them, and fails the case if two of them ever overlap: the handler's
// calls are one at a time, which is what lets a caller keep their state with
// no lock of its own.
type watchHeard struct {
	t      *testing.T
	inside atomic.Int32

	mu    sync.Mutex
	calls []heardCall
	// moved is signalled after every call. Capacity one, so a call never
	// waits for the case.
	moved chan struct{}
}

func newWatchHeard(t *testing.T) *watchHeard {
	return &watchHeard{t: t, moved: make(chan struct{}, 1)}
}

func (h *watchHeard) handler() WatchHandler {
	record := func(c heardCall) {
		if h.inside.Add(1) != 1 {
			h.t.Errorf("the watch called its handler (%s) while another call was "+
				"still running — the calls are one at a time", c)
		}
		h.mu.Lock()
		h.calls = append(h.calls, c)
		h.mu.Unlock()
		h.inside.Add(-1)
		select {
		case h.moved <- struct{}{}:
		default:
		}
	}
	return WatchHandler{
		Subject: func(subject string, seq uint64) { record(heardCall{subject: subject, seq: seq}) },
		Gap:     func(through uint64) { record(gapThrough(through)) },
	}
}

func (h *watchHeard) heard() []heardCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

// await waits until what the watch told its handler satisfies done, and fails
// the case naming what it was waiting for when the bound runs out first.
func (h *watchHeard) await(what string, done func([]heardCall) bool) []heardCall {
	h.t.Helper()
	bound := time.NewTimer(watchAwait)
	defer bound.Stop()
	for {
		if heard := h.heard(); done(heard) {
			return heard
		}
		select {
		case <-h.moved:
		case <-bound.C:
			h.t.Fatalf("the watch never %s within %s: its handler heard %v",
				what, watchAwait, h.heard())
		}
	}
}

// atLeast is a wait for n calls.
func atLeast(n int) func([]heardCall) bool {
	return func(heard []heardCall) bool { return len(heard) >= n }
}

// appendHeard appends one record and answers the call a watch on it hears.
func appendHeard(t *testing.T, log *DomainLog, subject string) heardCall {
	t.Helper()
	seq, _, err := log.Append(t.Context(), subject, "", nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("append to %s: %v", subject, err)
	}
	return heardCall{subject: subject, seq: seq}
}

// appendUnheard appends one record of another kind of object, outside
// watchFilter, and answers its sequence: the log's end, now past the last
// record a watch was told — which is what tells a Gap reaching the end from
// one reaching only the watch's last delivery.
func appendUnheard(t *testing.T, log *DomainLog) uint64 {
	t.Helper()
	return appendHeard(t, log, "crewlet.probe.log.name.a").seq
}

// watchLog begins a watch on log under watchFilter, stopped when the case
// ends, and answers it with what its handler heard.
func watchLog(t *testing.T, log *DomainLog) (*DomainWatch, *watchHeard) {
	t.Helper()
	heard := newWatchHeard(t)
	w, err := log.Watch(t.Context(), watchFilter, heard.handler())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(w.Stop)
	return w, heard
}

// awaitReplaced waits until w serves through a consumer other than lost, with
// the Gap its loss owed already told.
func awaitReplaced(t *testing.T, w *DomainWatch, lost string) string {
	t.Helper()
	deadline := time.Now().Add(watchAwait)
	for {
		if name, serving := w.consumer(); name != lost && serving {
			return name
		}
		if time.Now().After(deadline) {
			name, serving := w.consumer()
			t.Fatalf("the watch never replaced its lost consumer %s within %s "+
				"(now %s, serving %v)", lost, watchAwait, name, serving)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// externalLog provisions the probe domain on a queue dialled to an external
// server the case can restart, and opens its log there.
func externalLog(t *testing.T) (*externaltest.Server, *Queue, *DomainLog) {
	t.Helper()
	srv := externaltest.Start(t, queue.MaxPayloadBytes)
	q := newQueueWith(t, Config{URL: srv.URL(), Token: externaltest.Token})
	if err := q.EnsureDomainStream(t.Context(), probeDomain()); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	return srv, q, log
}

// A WATCH IS TOLD SUBJECTS AND NEVER CARRIES THE RECORDS.
//
// A watcher wants to know WHICH object moved, which it reads from the rows its
// own copy derives — and a domain's records are its own, the code log's as
// large as 8 MiB, so carrying each to every watcher as well would cost each of
// them the log's whole volume again. So the watch hears each record under its
// filter, in log order, with the sequence its append was given and nothing
// outside the filter, while the watcher's connection — a node of its own,
// carrying nothing but what its watch receives — takes in less than ONE of
// the records' bodies for all of them together. And the broker holds the
// consumer as the watch describes it: headers only, from the log's end,
// acknowledging nothing, one replica in memory, reaped a minute after nobody
// pulls.
//
// Mutation: drop HeadersOnly and the bodies cross the connection; widen the
// filter and the other kind's subject is heard; change any setting and the
// broker's copy says so.
func TestAWatchDeliversSubjectsNotPayloads(t *testing.T) {
	t.Parallel()
	spec := probeDomain()
	q := domainQueue(t, spec)
	writer, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	watcher, err := q.Peer(t.Context())
	if err != nil {
		t.Fatalf("open a second node on the broker: %v", err)
	}
	t.Cleanup(func() {
		if err := watcher.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the second node: %v", err)
		}
	})
	log, err := watcher.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log on the second node: %v", err)
	}
	w, heard := watchLog(t, log)

	body := bytes.Repeat([]byte("x"), 32<<10)
	before := watcher.Conn().Stats().InBytes
	var want []heardCall
	for _, subject := range []string{
		"crewlet.probe.log.repo.a",
		"crewlet.probe.log.name.a", // another kind of object: outside the filter
		"crewlet.probe.log.repo.b",
		"crewlet.probe.log.repo.a",
	} {
		seq, _, err := writer.Append(t.Context(), subject, "", nil, body)
		if err != nil {
			t.Fatalf("append to %s: %v", subject, err)
		}
		if strings.HasPrefix(subject, "crewlet.probe.log.repo.") {
			want = append(want, heardCall{subject: subject, seq: seq})
		}
	}
	// IN LOG ORDER, and the record outside the filter was appended before
	// the last one heard, so a filter that let it through would have put
	// it in this list.
	if got := heard.await("heard the records under its filter", atLeast(len(want))); !slices.Equal(got, want) {
		t.Fatalf("the watch told its handler %v, want %v — every record under "+
			"its filter, in log order, with the sequence its append was given, "+
			"nothing outside it and no Gap", got, want)
	}
	if received := watcher.Conn().Stats().InBytes - before; received >= uint64(len(body)) {
		t.Errorf("the watcher's connection took in %d bytes while %d records of "+
			"%d bytes were heard — a watch is told subjects, and the records' "+
			"bodies crossed its connection", received, len(want), len(body))
	}

	name, _ := w.consumer()
	cons, err := q.js.Consumer(t.Context(), spec.Name, name)
	if err != nil {
		t.Fatalf("read the watch's consumer %s: %v", name, err)
	}
	cfg := cons.CachedInfo().Config
	for _, c := range []struct {
		field     string
		got, want any
	}{
		{"headers_only", cfg.HeadersOnly, true},
		{"deliver_policy", cfg.DeliverPolicy, jetstream.DeliverNewPolicy},
		{"ack_policy", cfg.AckPolicy, jetstream.AckNonePolicy},
		{"num_replicas", cfg.Replicas, 1},
		{"mem_storage", cfg.MemoryStorage, true},
		{"inactive_threshold", cfg.InactiveThreshold, ephemeralInactive},
		{"durable_name", cfg.Durable, ""},
		{"filter_subject", cfg.FilterSubject, watchFilter},
	} {
		if c.got != c.want {
			t.Errorf("the broker holds the watch's consumer with %s %v, want %v",
				c.field, c.got, c.want)
		}
	}
}

// A WATCH BEGINS AT THE LOG'S END.
//
// What the log held before the watch began is what the watcher's rows already
// say, read by the pass it makes once the watch has begun; delivering it again
// would be every record since the log began, to every watcher, each time one
// starts. So the records appended before Watch returned are never heard and
// the ones after are, from the first of them. Deliveries are in log order, so
// an earlier record that was delivered would be heard first.
//
// Mutation: deliver all, and the first heard is the first the log ever held.
func TestAWatchBeginsAtTheLogsEnd(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	appendHeard(t, log, "crewlet.probe.log.repo.a")
	appendHeard(t, log, "crewlet.probe.log.repo.b")

	_, heard := watchLog(t, log)
	want := []heardCall{
		appendHeard(t, log, "crewlet.probe.log.repo.b"),
		appendHeard(t, log, "crewlet.probe.log.repo.c"),
	}
	if got := heard.await("heard the records appended after it began", atLeast(len(want))); !slices.Equal(got, want) {
		t.Fatalf("the watch told its handler %v, want %v — the records appended "+
			"after it began and none before", got, want)
	}
}

// A WATCH SAYS WHEN IT LOST ITS CONSUMER, AND GOES ON.
//
// A watch's consumer is one replica in memory, so it goes whenever the member
// holding it stops, the broker reaps it while this node was away, or somebody
// deletes it — and a watch that went deaf with it would leave its caller a
// whole cadence behind every record after, with nothing saying why. So a lost
// consumer is replaced by one of the watch's own at the log's end, and the
// handler is told Gap BEFORE anything the new one delivers, reaching the log's
// end when the broker made it: the caller reads its rows for what the gap
// held once its copy is there. Two ways a consumer goes, because the broker
// says so for one and nothing does for the other: deleted, it answers the
// standing pull that the consumer is gone; held in memory by a member that
// restarted, nothing serves the pull at all. Another kind's record after the
// last one heard puts the log's end past the watch's last delivery.
//
// Mutation: owe no Gap for a replacement, and the handler never hears one; no
// replacement, and the record after the loss is never heard; take only the
// broker's word for a loss, and the restarted member's is never noticed; reach
// the last delivery rather than where the replacement began, and the Gap stops
// short of the end.
func TestAWatchSaysWhenItLostItsConsumer(t *testing.T) {
	t.Parallel()

	t.Run("deleted", func(t *testing.T) {
		t.Parallel()
		q := domainQueue(t, probeDomain())
		log, err := q.DomainLog(t.Context(), probeDomain().Name)
		if err != nil {
			t.Fatalf("open the log: %v", err)
		}
		w, heard := watchLog(t, log)
		first := appendHeard(t, log, "crewlet.probe.log.repo.a")
		heard.await("heard the record before the loss", atLeast(1))
		end := appendUnheard(t, log)

		lost, _ := w.consumer()
		if err := q.js.DeleteConsumer(t.Context(), probeDomain().Name, lost); err != nil {
			t.Fatalf("delete the watch's consumer: %v", err)
		}
		awaitReplaced(t, w, lost)
		second := appendHeard(t, log, "crewlet.probe.log.repo.b")
		want := []heardCall{first, gapThrough(end), second}
		if got := heard.await("heard the record after its replacement", atLeast(len(want))); !slices.Equal(got, want) {
			t.Fatalf("the watch told its handler %v, want %v — a Gap for the "+
				"lost consumer reaching the log's end when its replacement was "+
				"made, before what the replacement delivered", got, want)
		}
	})

	t.Run("its member restarted", func(t *testing.T) {
		t.Parallel()
		srv, q, log := externalLog(t)
		// ASKED AGAIN IN MILLISECONDS rather than seconds: the member says it
		// is shutting down, so the first create goes with it and the ones
		// asked while it is down go unanswered, and the case is about the
		// replacement rather than its spacing.
		timing := log.watching()
		timing.attempt = 500 * time.Millisecond
		timing.retry, timing.retryCeiling = 20*time.Millisecond, 200*time.Millisecond
		log.watchTiming = timing
		w, heard := watchLog(t, log)
		first := appendHeard(t, log, "crewlet.probe.log.repo.a")
		heard.await("heard the record before the restart", atLeast(1))
		end := appendUnheard(t, log)

		lost, _ := w.consumer()
		srv.Restart(queue.MaxPayloadBytes, q.Conn())
		awaitReplaced(t, w, lost)
		second := appendHeard(t, log, "crewlet.probe.log.repo.b")
		got := heard.await("heard the record after its replacement",
			func(heard []heardCall) bool { return len(heard) > 0 && heard[len(heard)-1] == second })
		// ONE GAP AT LEAST, AND NOTHING ELSE between the two: the restart is
		// also a reconnect, which may have said so before the loss was known.
		// Every one of them reaches the end: the replacement began after it,
		// a reconnect's reads it, and nothing was appended until the record
		// after.
		if len(got) < 3 || got[0] != first {
			t.Fatalf("the watch told its handler %v, want %s, then a Gap through %d, "+
				"then %s", got, first, end, second)
		}
		for _, between := range got[1 : len(got)-1] {
			if between != gapThrough(end) {
				t.Fatalf("the watch told its handler %v — between the record before "+
					"the restart and the one after, only Gaps through the log's end, %d",
					got, end)
			}
		}
	})
}

// A GAP REACHES EVERY RECORD THE WATCH MISSED.
//
// A caller reads its rows for what a Gap held, and its rows say only what its
// copy has applied — and what costs a watch its records, a member restarting
// or a connection lost, can hold that copy back as well. Rows read at a copy
// behind the gap's records find nothing to do, and the caller would hear of
// them only at its next pass over its rows. So a Gap reaches a position at or
// past every record the watch never told, and the caller waits for its copy
// to get there. Staged as the case it exists for: records appended while no
// consumer serves the watch, which its replacement — beginning at the log's
// end — never delivers. The replacement is held off by a consumer of another
// description under the name its create asks for, which the broker refuses to
// make twice, so those records are appended before it can begin whatever the
// timing.
//
// Mutation: reach the watch's last delivery rather than where its
// replacement began, and the records appended while it was deaf lie past the
// Gap.
func TestAGapReachesEveryRecordTheWatchMissed(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	// ASKED AGAIN IN MILLISECONDS rather than seconds, so the replacement
	// begins as soon as nothing stands in its way: the case is about the
	// position its Gap reaches, not the spacing of its attempts.
	timing := log.watching()
	timing.retry, timing.retryCeiling = 20*time.Millisecond, 200*time.Millisecond
	log.watchTiming = timing
	w, heard := watchLog(t, log)
	first := appendHeard(t, log, "crewlet.probe.log.repo.a")
	heard.await("heard the record before the loss", atLeast(1))

	next := w.prefix + "2"
	standing := w.config(next)
	standing.Description = "a consumer standing where the watch's replacement would"
	if _, err := q.js.CreateConsumer(t.Context(), probeDomain().Name, standing); err != nil {
		t.Fatalf("make a consumer under the replacement's name: %v", err)
	}
	lost, _ := w.consumer()
	if err := q.js.DeleteConsumer(t.Context(), probeDomain().Name, lost); err != nil {
		t.Fatalf("delete the watch's consumer: %v", err)
	}
	// DELIVERED BY NOBODY: the watch's consumer is gone, its replacement
	// cannot be made, and nothing pulls from the consumer standing in its
	// place.
	appendHeard(t, log, "crewlet.probe.log.repo.b")
	missed := appendHeard(t, log, "crewlet.probe.log.repo.c")
	if err := q.js.DeleteConsumer(t.Context(), probeDomain().Name, next); err != nil {
		t.Fatalf("delete the consumer standing in the replacement's place: %v", err)
	}
	awaitReplaced(t, w, lost)
	second := appendHeard(t, log, "crewlet.probe.log.repo.d")
	want := []heardCall{first, gapThrough(missed.seq), second}
	if got := heard.await("heard the record after its replacement", atLeast(len(want))); !slices.Equal(got, want) {
		t.Fatalf("the watch told its handler %v, want %v — the records appended "+
			"while no consumer served it are never told, and its Gap reaches the "+
			"last of them", got, want)
	}
}

// A REPLACEMENT ASKED AGAIN IS THE SAME CONSUMER.
//
// A create whose reply was lost — the member it reached went down, the
// metadata group answered after the attempt's bound — may still have made its
// consumer, and asking again under a fresh name would leave that one behind
// for nobody, beside a second. So every attempt for one generation asks for
// the same name, and the broker answers a create it has already made with the
// consumer it made. Staged by making the replacement's consumer before the
// loss, exactly as an earlier attempt of the watch's would have, then losing
// the watch's consumer: the watch serves through that one, and hears through
// it. A record appended between the two shows WHERE that consumer begins:
// where it was made, the log's end then, which its create's reply carries —
// so the Gap reaches that end and the record after it is told again, by the
// consumer that begins before it.
//
// Mutation: name each attempt afresh, and the watch replaces its consumer with
// another; should the broker ever refuse a create it already made, the
// replacement never begins; reach the log's end when the Gap is told rather
// than where the replacement began, and the Gap reaches past a record the
// replacement has yet to tell.
func TestAReplacementAskedAgainIsTheSameConsumer(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	w, heard := watchLog(t, log)
	first := appendHeard(t, log, "crewlet.probe.log.repo.a")
	heard.await("heard the record before the loss", atLeast(1))

	lost, _ := w.consumer()
	next := w.prefix + "2"
	if _, err := q.js.CreateConsumer(t.Context(), probeDomain().Name, w.config(next)); err != nil {
		t.Fatalf("make the consumer an earlier attempt would have made: %v", err)
	}
	between := appendHeard(t, log, "crewlet.probe.log.repo.b")
	heard.await("heard the record appended after the earlier attempt", atLeast(2))
	if err := q.js.DeleteConsumer(t.Context(), probeDomain().Name, lost); err != nil {
		t.Fatalf("delete the watch's consumer: %v", err)
	}
	if got := awaitReplaced(t, w, lost); got != next {
		t.Fatalf("the watch replaced its consumer with %s, want %s — the one its "+
			"own earlier create made", got, next)
	}
	second := appendHeard(t, log, "crewlet.probe.log.repo.c")
	want := []heardCall{first, between, gapThrough(first.seq), between, second}
	if got := heard.await("heard the record after its replacement", atLeast(len(want))); !slices.Equal(got, want) {
		t.Fatalf("the watch told its handler %v, want %v — a Gap reaching the "+
			"log's end when the replacement was made, and everything after it "+
			"again from the replacement", got, want)
	}
}

// A DELIVERY THAT NEVER ARRIVED IS A GAP, AND THE CONSUMER IS KEPT.
//
// A watch acknowledges nothing, so a delivery the broker made to an inbox
// nobody held — this node's subscription moving between servers, a leaf's
// link coming back — is never made again, while the consumer that made it
// goes on serving. Every delivery carries the consumer's own count of its
// deliveries, which rises by one each time; one that rose by more says a
// delivery went missing, and the handler is told Gap before the subject that
// revealed it, reaching that subject's own position: the consumer delivers in
// log order, so what went missing came before it. Driven through the delivery
// path itself, since no broker misdelivers on request.
//
// A delivery whose position cannot be read vouches for nothing — neither
// whether one went missing before it nor how far the record it carried lies —
// and is a Gap in place of its subject, reaching the log's end, read once the
// delivery has come: no record a consumer delivered is past the end read
// after it. Driven on a real watch, whose supervisor reads that end.
//
// Mutation: drop the count's check, and the missing delivery is never told;
// reach the consumer's count rather than the log's position, or where the
// watch's consumer began rather than the end, and each Gap stops short.
func TestAWatchSaysWhenADeliveryNeverReachedIt(t *testing.T) {
	t.Parallel()

	t.Run("its count skipped one", func(t *testing.T) {
		t.Parallel()
		heard := newWatchHeard(t)
		w := &DomainWatch{l: &DomainLog{name: probeDomain().Name}, filter: watchFilter,
			h: heard.handler(), logger: slog.New(slog.DiscardHandler),
			gen: 1, live: true, name: "watch_probe_1"}
		const subject = "crewlet.probe.log.repo.a"
		deliver := func(count, seq uint64) heardCall {
			w.take(1, subject, &jetstream.MsgMetadata{
				Sequence: jetstream.SequencePair{Consumer: count, Stream: seq}}, nil)
			return heardCall{subject: subject, seq: seq}
		}
		want := []heardCall{deliver(1, 10), deliver(2, 11)}
		revealing := deliver(4, 15) // the third went to an inbox nobody held
		want = append(want, gapThrough(revealing.seq), revealing, deliver(5, 16))
		if got := heard.heard(); !slices.Equal(got, want) {
			t.Fatalf("the watch told its handler %v, want %v", got, want)
		}
	})

	t.Run("its position cannot be read", func(t *testing.T) {
		t.Parallel()
		q := domainQueue(t, probeDomain())
		log, err := q.DomainLog(t.Context(), probeDomain().Name)
		if err != nil {
			t.Fatalf("open the log: %v", err)
		}
		w, heard := watchLog(t, log)
		first := appendHeard(t, log, "crewlet.probe.log.repo.a")
		heard.await("heard the record before the unreadable one", atLeast(1))
		end := appendUnheard(t, log)

		w.mu.Lock()
		gen := w.gen
		w.mu.Unlock()
		w.take(gen, "crewlet.probe.log.repo.b", nil,
			errors.New("a reply subject that names no position"))
		heard.await("said a delivery's position could not be read", atLeast(2))
		second := appendHeard(t, log, "crewlet.probe.log.repo.c")
		want := []heardCall{first, gapThrough(end), second}
		if got := heard.await("heard the record after", atLeast(len(want))); !slices.Equal(got, want) {
			t.Fatalf("the watch told its handler %v, want %v — a Gap through the "+
				"log's end in place of the delivery it could not read", got, want)
		}
	})
}

// A RECONNECT IS A GAP, AND THE CONSUMER IS KEPT.
//
// A delivery the broker made while this node's connection was away went to an
// inbox nobody held, and when it was the last for a while nothing would ever
// reveal it — the consumer's count shows a missing delivery only on the next
// one. So the watch reads its connection's reconnect count every heartbeat
// and tells its handler Gap when it moved, reaching the log's end read once
// it was noticed, and keeps the consumer, which a blip does not end
// ([ephemeralInactive]). Another kind's record after the last one heard puts
// the end past the watch's last delivery.
//
// Mutation: drop the recheck, and no Gap is told; reach the watch's last
// delivery rather than the end, and the Gap stops short of it.
func TestAWatchSaysWhenItsConnectionWasAway(t *testing.T) {
	t.Parallel()
	_, q, log := externalLog(t)
	// READ EVERY FEW MILLISECONDS rather than every heartbeat: the case is
	// about the Gap, not the cadence.
	timing := log.watching()
	timing.recheck = 20 * time.Millisecond
	log.watchTiming = timing
	w, heard := watchLog(t, log)
	first := appendHeard(t, log, "crewlet.probe.log.repo.a")
	heard.await("heard the record before the reconnect", atLeast(1))
	end := appendUnheard(t, log)

	kept, _ := w.consumer()
	reconnects := q.Conn().Stats().Reconnects
	if err := q.Conn().ForceReconnect(); err != nil {
		t.Fatalf("reconnect the queue's connection: %v", err)
	}
	heard.await("said its connection was away", atLeast(2))
	deadline := time.Now().Add(watchAwait)
	for !q.Conn().IsConnected() || q.Conn().Stats().Reconnects == reconnects {
		if time.Now().After(deadline) {
			t.Fatalf("the queue's connection did not come back within %s", watchAwait)
		}
		time.Sleep(5 * time.Millisecond)
	}
	second := appendHeard(t, log, "crewlet.probe.log.repo.b")
	want := []heardCall{first, gapThrough(end), second}
	if got := heard.await("heard the record after the reconnect", atLeast(len(want))); !slices.Equal(got, want) {
		t.Fatalf("the watch told its handler %v, want %v — a Gap through the "+
			"log's end, read once the reconnect was noticed", got, want)
	}
	if name, _ := w.consumer(); name != kept {
		t.Errorf("the watch replaced its consumer %s with %s over a reconnect it "+
			"outlived", kept, name)
	}
}

// A WATCH THAT CANNOT BEGIN IS REFUSED.
//
// Nothing is left running for a caller that was told no: the first consumer
// is made before Watch returns, and a refusal returns no watch. Each refusal
// names what to change. ONE OF THEM IS THE WATCH'S OWN, because the broker no
// longer makes it: a filter that meets none of the log's subjects is a
// consumer the broker creates without a word and that delivers nothing, for
// ever. A filter wider than the log watches every record it holds, which a
// caller can mean, and begins.
//
// Mutation: drop the watch's own check, and the filter outside the log begins
// a watch that hears nothing; check containment rather than overlap, and the
// wider filter is refused.
func TestAWatchThatCannotBeginIsRefused(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	heard := newWatchHeard(t).handler()
	for _, tc := range []struct {
		name    string
		filter  string
		handler WatchHandler
		says    string
	}{
		{"no filter", "", heard, "names no subjects"},
		{"no Gap", watchFilter, WatchHandler{Subject: heard.Subject}, "needs both handlers"},
		{"no Subject", watchFilter, WatchHandler{Gap: heard.Gap}, "needs both handlers"},
		{"outside the log", "crewlet.elsewhere.>", heard, "would hear nothing"},
		// REFUSED BELOW THE WATCH, by the client library and the broker
		// both, which is why its syntax is left to them.
		{"not a subject pattern", "crewlet.probe.>.log", heard, "crewlet.probe.>.log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := log.Watch(t.Context(), tc.filter, tc.handler)
			if err == nil {
				w.Stop()
				t.Fatalf("Watch began a watch, want a refusal saying %q", tc.says)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal %q does not say %q", err, tc.says)
			}
		})
	}

	t.Run("wider than the log", func(t *testing.T) {
		w, err := log.Watch(t.Context(), "crewlet.probe.>", heard)
		if err != nil {
			t.Fatalf("a filter wider than the log was refused: %v — it watches "+
				"every record the log holds", err)
		}
		w.Stop()
	})

	t.Run("a stopped queue", func(t *testing.T) {
		stopped := domainQueue(t, probeDomain())
		log, err := stopped.DomainLog(t.Context(), probeDomain().Name)
		if err != nil {
			t.Fatalf("open the log: %v", err)
		}
		if err := stopped.Stop(t.Context()); err != nil {
			t.Fatalf("stop the queue: %v", err)
		}
		if _, err := log.Watch(t.Context(), watchFilter, heard); !errors.Is(err, queue.ErrNotLive) {
			t.Fatalf("Watch on a stopped queue = %v, want %v", err, queue.ErrNotLive)
		}
	})
}
