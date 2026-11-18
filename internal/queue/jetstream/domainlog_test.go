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

// A LOG CANNOT REMOVE AN INTERIOR SEQUENCE. The strict replay protocol treats
// a hole as a fault, which is sound only while these stream settings make an
// interior removal impossible, and each fails silently if it drifts: an age
// bound or a per-subject limit stalls the strict loop on an ordinary write; a
// delete or a rollup erases a committed record from under every applier; a
// discard-old policy drops the company's history under load.
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
	// THE LIVE CONFIGURATION, not the spec sent: a field the server ignored
	// or defaulted differently is the drift this case is for.
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
			"every data node's applier")
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

// AND NOTHING TRIMS PER SUBJECT. A keep-newest purge excludes purging by
// sequence, so it costs one API call per subject per trim, and it removes the
// interior sequences the strict protocol rests on. A static walk, because it
// is what somebody reaches for on first meeting the trimmed-anchor hazard.
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

// parseEveryGoFile parses every .go file in dir, keyed by path, whatever its
// build tags: a purge behind a //go:build line is still a purge.
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

// A DOMAIN LOG ANSWERS AN EMPTY SUBJECT AS A FACT, NOT AS A FAILURE: it tells
// a trimmed anchor (publish at an expectation of zero) from a lost race (never
// do), and a caller reading that from an error would take the trimmed-anchor
// branch on a broker that was merely unreachable.
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
// none at all: zero is a real one, "this subject holds nothing", and taking
// one for the other costs a silent lost update rather than a refused write.
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

// watchAwait bounds every wait on a watch's handler: only how long a broker
// that never delivers gets before a case says so (a delivery takes
// milliseconds). It covers a consumer replaced once its member's silence
// outlasts two heartbeats.
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
	bound := time.NewTimer(watchAwait) //fixedwait:liveness
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

// appendUnheard appends a record outside watchFilter and answers its
// sequence: the log's end, now past the watch's last delivery, which tells a
// Gap reaching the end from one reaching only that delivery.
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

// awaitReplaced waits until w has SETTLED after losing consumer lost, so what
// it tells next is what the case appends next:
//   - it serves through a consumer other than lost, its loss's Gap told;
//   - the broker as it now runs holds that consumer: one made as its member
//     went down serves only until the watch learns the restart took it, and
//     a record appended meanwhile would be told as a Gap;
//   - it has read the connection's reconnect count as it stands, no Gap owed:
//     a consumer made across a reconnect leaves it to the next recheck, whose
//     Gap would reach past a record appended before it.
func awaitReplaced(t *testing.T, w *DomainWatch, lost string) string {
	t.Helper()
	deadline := time.Now().Add(watchAwait)
	for {
		if name, serving := w.consumer(); name != lost && serving && held(t, w, name) && reconnectsRead(w) {
			return name
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			name, serving := w.consumer()
			t.Fatalf("the watch never settled on a replacement of its lost consumer %s within %s "+
				"(now %s, serving %v, held by the broker %v, its reconnects read %v)", lost, watchAwait,
				name, serving, held(t, w, name), reconnectsRead(w))
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
}

// held reports whether the broker as it now runs holds the consumer name on
// w's log. A request it does not answer is not yet, and asked again.
func held(t *testing.T, w *DomainWatch, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), w.timing.attempt)
	defer cancel()
	_, err := w.l.js.Consumer(ctx, w.l.name, name)
	return err == nil
}

// reconnectsRead reports whether w has read the queue's connection's
// reconnect count as it stands, with no Gap still owed for a reconnect.
func reconnectsRead(w *DomainWatch) bool {
	now := w.l.js.Conn().Stats().Reconnects
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reconnects == now && !w.gapUnread
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

// A WATCH IS TOLD SUBJECTS AND NEVER CARRIES THE RECORDS (package doc, "A
// watch is told which records moved, and never carries them"). It hears each
// record under its filter, in log order, with its append's sequence, while
// the watcher's connection (a node of its own) takes in less than ONE record's
// body for all of them. The broker holds the consumer as the watch describes
// it: headers only, from the log's end, acknowledging nothing, one replica in
// memory, reaped a minute after nobody pulls.
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
	// IN LOG ORDER; the record outside the filter came before the last one
	// heard, so a filter letting it through would put it in this list.
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

// A WATCH BEGINS AT THE LOG'S END: what came before is in the watcher's rows,
// read by its pass once the watch has begun, and redelivering it would send
// every record ever to every watcher at each start. Deliveries are in log
// order, so an earlier record delivered would be heard first.
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

// A WATCH SAYS WHEN IT LOST ITS CONSUMER, AND GOES ON. Its consumer is one
// replica in memory, gone when its member stops, the broker reaps it or
// somebody deletes it, and a deaf watch would leave its caller behind with
// nothing saying why. So a lost consumer is replaced at the log's end, and Gap
// is told BEFORE anything the new one delivers, reaching the log's end when it
// was made. Both losses are staged: deleted, the broker answers the standing
// pull; on a restarted member, nothing serves the pull at all.
//
// Mutation: owe no Gap and none is heard; no replacement and the record after
// the loss is never heard; take only the broker's word and the restart is
// never noticed; reach the last delivery and the Gap stops short of the end.
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
		// ASKED AGAIN IN MILLISECONDS: the shutting-down member takes the first
		// create and leaves those asked while it is down unanswered, and the
		// case is about the replacement, not its spacing.
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
		// also a reconnect, which may say so before the loss is known, and a
		// replacement made as the member went down is lost again. Each reaches
		// the end, as nothing was appended until the watch settled
		// (awaitReplaced).
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

// A GAP REACHES EVERY RECORD THE WATCH MISSED. What cost the watch its records
// can hold the caller's copy back too, and rows read short of them find
// nothing to do; so a Gap reaches at or past every record never told, and the
// caller waits for its copy to get there. Staged with records appended while
// no consumer serves the watch: a consumer of another description holds the
// name the replacement's create asks for, which the broker will not make
// twice, so they land before the replacement can begin, whatever the timing.
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
	// ASKED AGAIN IN MILLISECONDS, so the replacement begins once nothing
	// stands in its way: the case is about where its Gap reaches.
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

// A REPLACEMENT ASKED AGAIN IS THE SAME CONSUMER. A create whose reply was
// lost may still have made its consumer, and a fresh name would orphan it
// beside a second; so every attempt of a generation asks for one name, and
// the broker answers a create it already made with that consumer. Staged by
// making it before the loss, as an earlier attempt would have. A record
// appended between shows where it begins: the log's end when made, which its
// create's reply carries, so the Gap reaches that end and that record is told
// again.
//
// Mutation: name attempts afresh and the replacement is another; a broker
// refusing a create it made never begins it; reach the end when the Gap is
// told and it passes a record the replacement has yet to tell.
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

// A DELIVERY THAT NEVER ARRIVED IS A GAP, AND THE CONSUMER IS KEPT. A watch
// acks nothing, so a delivery to an inbox nobody held is never remade. Each
// carries the consumer's count, rising by one; a rise of more is told as a
// Gap before the revealing subject, through its position, since delivery is
// in log order (driven through the delivery path: no broker misdelivers on
// request). A delivery whose position cannot be read vouches for nothing and
// is a Gap in its place, through the log's end read after it came: no record
// a consumer delivered lies past that end.
//
// Mutation: drop the count's check and the missing delivery is never told;
// reach the count rather than the position, or the consumer's start rather
// than the end, and each Gap stops short.
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

// A RECONNECT IS A GAP, AND THE CONSUMER IS KEPT. A delivery made while the
// connection was away went to an inbox nobody held, and if it was the last
// for a while the consumer's count never reveals it. So the watch reads its
// reconnect count every heartbeat and tells Gap when it moved, through the
// log's end read once noticed, keeping the consumer, which a blip does not
// end ([ephemeralInactive]).
//
// Mutation: drop the recheck, and no Gap is told; reach the watch's last
// delivery rather than the end, and the Gap stops short of it.
func TestAWatchSaysWhenItsConnectionWasAway(t *testing.T) {
	t.Parallel()
	_, q, log := externalLog(t)
	// READ EVERY FEW MILLISECONDS: the case is about the Gap, not the cadence.
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
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("the queue's connection did not come back within %s", watchAwait)
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
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

// A WATCH THAT CANNOT BEGIN IS REFUSED: the first consumer is made before
// Watch returns, so a refusal leaves nothing running, and each names what to
// change. ONE CHECK IS THE WATCH'S OWN: the broker silently creates a consumer
// whose filter meets none of the log's subjects, delivering nothing for ever.
// A filter wider than the log watches all it holds, and begins.
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
