package issuereport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

const genuineIssue = "https://github.com/crewlet/crewlet/issues/4242"

// releaseOrigin is the test-only override: a hand-written origin that may report,
// which the engine's wiring never makes ([TestNoSettingReachesTheOrigin]).
var releaseOrigin = Origin{Build: Build{Version: "v1.2.3", Commit: CommitHash(strings.Repeat("a", 40)),
	Go: "go1.27.2", OS: "linux", Arch: "amd64"}}

// fakeRelay validates as the relay does, strictly, and records what it took.
type fakeRelay struct {
	t      *testing.T
	server *httptest.Server

	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header

	// answer, when set, replaces the normal answer.
	answer func(w http.ResponseWriter, attempt int) bool
	// duplicate answers a fingerprint seen before as the relay would.
	seen map[string]bool
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	f := &fakeRelay{t: t, seen: map[string]bool{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRelay) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.headers = append(f.headers, r.Header.Clone())
	attempt := len(f.bodies)
	answer := f.answer
	f.mu.Unlock()
	if answer != nil && answer(w, attempt) {
		return
	}
	export, err := CatalogueJSON()
	if err != nil {
		f.t.Errorf("the export: %v", err)
	}
	if refused := relayRefusal(export, body); refused != nil {
		f.t.Errorf("the relay would refuse this report: %v\n%s", refused, body)
	}
	var said struct {
		Fingerprint string `json:"fingerprint"`
	}
	_ = json.Unmarshal(body, &said)
	f.mu.Lock()
	dup := f.seen[said.Fingerprint]
	f.seen[said.Fingerprint] = true
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if dup {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"duplicate","issue":"`+genuineIssue+`"}`)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, `{"status":"filed","issue":"`+genuineIssue+`"}`)
}

func (f *fakeRelay) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.bodies)
}

// clock is a test's movable time.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 6, 7, 42, 13, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// rig is a reporter on a memory fleet, sending to a fake relay.
type rig struct {
	t      *testing.T
	relay  *fakeRelay
	fleet  *memory.Fleet
	clock  *clock
	report *Reporter
	stop   func()
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newRigOn(t, memory.NewFleet(), newFakeRelay(t), newClock())
}

func newRigOn(t *testing.T, fleet *memory.Fleet, relay *fakeRelay, now *clock,
	options ...func(*Config)) *rig {

	t.Helper()
	cfg := Config{
		Store:     fleet,
		Client:    relay.server.Client(),
		URL:       relay.server.URL,
		Origin:    releaseOrigin,
		Topology:  func(context.Context) Topology { return NewTopology(1, "embedded", "local", "direct") },
		RetryBase: time.Millisecond,
		Now:       now.Now,
	}
	for _, option := range options {
		option(&cfg)
	}
	reporter := New(cfg)
	reporter.SetEnabled(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reporter.Run(ctx)
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(stop)
	return &rig{t: t, relay: relay, fleet: fleet, clock: now, report: reporter, stop: stop}
}

// waitFor polls a condition a sender goroutine makes true.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// settled waits for a record to leave `sending`, and returns it.
func (r *rig) settled(fingerprint Fingerprint) coord.IssueReport {
	r.t.Helper()
	var held coord.IssueReport
	waitFor(r.t, "the report to settle", func() bool {
		rec, found, err := r.fleet.IssueReport(context.Background(), string(fingerprint))
		if err != nil {
			r.t.Fatalf("IssueReport: %v", err)
		}
		held = rec
		return found && rec.Status != coord.IssueReportSending
	})
	return held
}

// idle waits for the sender to have nothing queued or under way.
func (r *rig) idle() {
	r.t.Helper()
	waitFor(r.t, "the sender to go idle", func() bool {
		r.report.mu.Lock()
		defer r.report.mu.Unlock()
		return len(r.report.queue) == 0 && len(r.report.queued) == 0
	})
}

func (r *rig) panicOnce(subject string) {
	r.report.Observe(context.Background(), panicSignalFor(subject))
}

// panicFacts are a contained panic's facts, for a signal a test writes by hand.
var panicFacts = map[FactKey]FactValue{
	FactScope:     enumFact(string(panics.ScopeTurn)),
	FactPanicKind: enumFact(string(panics.KindNilDereference)),
}

// panicSignalFor is a panic that names no place of its own, so it falls back to
// the class's: one fingerprint, for the cases about sending.
func panicSignalFor(subject string) Signal {
	return Signal{Class: ClassEnginePanicked, Subject: subject, Facts: panicFacts}
}

var panicFingerprint = FingerprintOf(ClassEnginePanicked, "internal/panics", "internal/panics.Recovered")

//go:noinline
func writesNilMap() { noMap()["x"] = 1 }

//go:noinline
func noMap() map[string]int { return nil }

//go:noinline
func callsWritesNilMap() { writesNilMap() }

// contained is what a site that contains fn's panic hands the reporter.
func contained(scope panics.Scope, fn func()) (p *panics.Panic) {
	defer func() { p = panics.Recovered(scope, recover()) }()
	fn()
	return nil
}

// ---- the fingerprint ----------------------------------------------------- //

// THE SAME DEFECT IS ONE FINGERPRINT on every engine, and the relay recomputes
// it, so this vector is the contract's own: changing it splits every issue.
func TestTheFingerprintIsTheContractsDigest(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		got  Fingerprint
		want Fingerprint
	}{
		{"no identity", FingerprintOf(ClassContextExhaustedLoop, "internal/sandbox", "internal/sandbox.collectedRecord"),
			"e865a51da6c1494ca24e7c7d4922d5050583f27bf7d7682b1d60fd889887e7bb"},
		{"a harness and a boolean", FingerprintOf(ClassContextExhaustedLoop, "internal/sandbox", "internal/sandbox.collectedRecord",
			"harness.name=claude-code", "session_continued=true"),
			"8674191f61126015bcb551cc47a50c70b212966a4bf95390702c7123e5136840"},
		{"an enum", FingerprintOf(ClassRunStranded, "internal/sandbox", "internal/sandbox.(*Coordinator).settleFailed",
			"reason=no_execute_state"),
			"ca85cd9e630e638787ae1c57b85aa46ce5b0cbd65c8d4949549d1398c7f1e6fc"},
	} {
		if c.got != c.want {
			t.Errorf("%s: fingerprint = %s, want %s", c.name, c.got, c.want)
		}
	}
}

// AN IDENTITY FACT SPLITS A CLASS, and nothing else does. The same defect on two
// engines is one fingerprint whatever an integer or an optional fact says; a session
// continued and one not are two defects, and so are two harnesses.
func TestAnIdentityFactSplitsAClassAndNothingElseDoes(t *testing.T) {
	t.Parallel()
	entry, _ := entryOf(ClassContextExhaustedLoop)
	claude, opencode := harnessOf("claude-code", "2.1.310"), harnessOf("opencode", "1.0.70")
	base := func(continued bool, size string) map[FactKey]FactValue {
		return map[FactKey]FactValue{FactSessionContinued: boolFact(continued), FactSessionSizeBucket: enumFact(size)}
	}
	of := func(harness *Harness, facts map[FactKey]FactValue) Fingerprint {
		return entry.fingerprintOf(entry.Component, entry.Site, harness, facts)
	}
	first := of(claude, base(true, "0-64KiB"))
	if got := of(harnessOf("claude-code", "9.9.9"), base(true, "16MiB+")); got != first {
		t.Errorf("a harness release or a size bucket split the defect: %s and %s", got, first)
	}
	windowed := base(true, "0-64KiB")
	windowed[FactContextWindowTokens] = intFact(100_000)
	if got := of(claude, windowed); got != first {
		t.Errorf("an optional integer split the defect: %s and %s", got, first)
	}
	if got := of(claude, base(false, "0-64KiB")); got == first {
		t.Error("a session continued and one not are one fingerprint")
	}
	if got := of(opencode, base(true, "0-64KiB")); got == first {
		t.Error("two harnesses are one fingerprint")
	}
}

// AN IDENTITY STAYS SMALL AND CLOSED: required, a boolean, a closed set or the
// harness's name, sorted, with at most 16 combinations. Anything per-engine in it
// would split one defect into an issue per engine.
func TestAnIdentityIsSmallAndClosed(t *testing.T) {
	t.Parallel()
	for _, entry := range catalogue {
		for _, problem := range identityProblems(entry) {
			t.Errorf("%s: %s", entry.ID, problem)
		}
	}
	base, _ := entryOf(ClassContextExhaustedLoop)
	with := func(identity ...FactKey) Entry {
		e := base
		e.Identity = identity
		return e
	}
	big := make([]string, 17)
	for i := range big {
		big[i] = strings.Repeat("v", i+1)
	}
	wide := base
	wide.Facts = []Fact{{Key: "wide", Type: FactEnum, Enum: big}}
	wide.Identity = []FactKey{"wide"}
	noHarness := with(IdentityHarnessName)
	noHarness.Harness = false
	for name, e := range map[string]Entry{
		"an integer": with(FactContextWindowTokens),
		"an optional fact": func() Entry {
			e := with("o")
			e.Facts = []Fact{{Key: "o", Type: FactBoolean, Optional: true}}
			return e
		}(),
		"an undeclared fact":           with(FactDeferCause),
		"unsorted":                     with(FactSessionContinued, IdentityHarnessName),
		"a repeat":                     with(FactSessionContinued, FactSessionContinued),
		"a harness on a class of none": noHarness,
		"past sixteen":                 wide,
	} {
		if len(identityProblems(e)) == 0 {
			t.Errorf("%s was accepted", name)
		}
	}
}

// identityProblems is everything wrong with a class's declared identity.
func identityProblems(e Entry) []string {
	var problems []string
	if !slices.IsSorted(e.Identity) {
		problems = append(problems, "identity is not sorted")
	}
	if len(slices.Compact(slices.Clone(e.Identity))) != len(e.Identity) {
		problems = append(problems, "identity repeats a path")
	}
	product := 1
	for _, path := range e.Identity {
		if path == IdentityHarnessName {
			if !e.Harness {
				problems = append(problems, "harness.name on a class with no harness")
			}
			product *= len(knownHarnesses)
			continue
		}
		i := slices.IndexFunc(e.Facts, func(f Fact) bool { return f.Key == path })
		if i < 0 {
			problems = append(problems, string(path)+" is not a declared fact")
			continue
		}
		fact := e.Facts[i]
		if fact.Optional {
			problems = append(problems, string(path)+" is optional")
		}
		switch fact.Type {
		case FactBoolean:
			product *= 2
		case FactEnum:
			product *= len(fact.Enum)
		default:
			problems = append(problems, string(path)+" is neither a boolean nor a closed set")
		}
	}
	if product > 16 {
		problems = append(problems, "identity has more than 16 combinations")
	}
	return problems
}

// ---- nothing confidential leaves ----------------------------------------- //

// A REPORT HOLDS NO PLAIN STRING. A `string` field anywhere in its type is a
// place a sentence could reach, so one added later fails here, whatever the
// code that fills it does today.
func TestAReportHoldsNoPlainString(t *testing.T) {
	t.Parallel()
	seen := map[reflect.Type]bool{}
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		if seen[typ] || typ == reflect.TypeFor[time.Time]() {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.String:
			if typ == reflect.TypeFor[string]() {
				t.Errorf("%s is a plain string: a report's strings are named types with a validating constructor", path)
			}
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(path+"[]", typ.Elem())
		case reflect.Map:
			walk(path+".key", typ.Key())
			walk(path+".value", typ.Elem())
		case reflect.Interface:
			t.Errorf("%s is an interface: it could hold anything", path)
		case reflect.Struct:
			for field := range typ.Fields() {
				walk(path+"."+field.Name, field.Type)
			}
		}
	}
	walk("Report", reflect.TypeFor[Report]())
}

// canaries are what a company's failure carries: if any of them reaches a
// report, the promise is broken.
var canaries = []string{
	"Acme-Payroll-Corp", "jane.doe@acme.example", "/srv/acme/secret-project/main.go",
	"https://internal.acme.example/ticket/ACME-1234", "10.42.0.17", "sk-ant-api03-LEAKED",
	"U0FOUNDER", "9f8c7b6a-0000-4000-8000-acmeacmeacme", "payroll-agent",
	"DROP TABLE seats; --", "<script>alert(1)</script>", "line one\nline two\x00 of a prompt",
}

// fillStrings writes canaries into every string a value can hold, however deep.
func fillStrings(rng *rand.Rand, v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(canaries[rng.IntN(len(canaries))])
		}
	case reflect.Pointer:
		if v.CanSet() && v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		if !v.IsNil() {
			fillStrings(rng, v.Elem())
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			return
		}
		for i := range v.NumField() {
			fillStrings(rng, v.Field(i))
		}
	case reflect.Slice:
		if v.CanSet() {
			v.Set(reflect.MakeSlice(v.Type(), 2, 2))
			for i := range v.Len() {
				fillStrings(rng, v.Index(i))
			}
		}
	case reflect.Map:
		if v.CanSet() && v.Type().Key().Kind() == reflect.String {
			m := reflect.MakeMap(v.Type())
			value := reflect.New(v.Type().Elem()).Elem()
			fillStrings(rng, value)
			m.SetMapIndex(reflect.ValueOf(canaries[rng.IntN(len(canaries))]).Convert(v.Type().Key()), value)
			v.Set(m)
		}
	}
}

// assertAllowlisted holds every value of a report's JSON to its path's shape,
// so what is checked is what is there and not only what a canary would show.
func assertAllowlisted(t *testing.T, body []byte) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("a report is not JSON: %v", err)
	}
	class, _ := doc["class"].(string)
	entry, ok := entryOf(Class(class))
	if !ok {
		t.Fatalf("class %q is not in the catalogue", class)
	}
	inSet := func(values []string) *regexp.Regexp {
		quoted := make([]string, len(values))
		for i, v := range values {
			quoted[i] = regexp.QuoteMeta(v)
		}
		return regexp.MustCompile("^(" + strings.Join(quoted, "|") + ")$")
	}
	shapes := map[string]*regexp.Regexp{
		"class":                 inSet([]string{class}),
		"fingerprint":           regexp.MustCompile(`^[0-9a-f]{64}$`),
		"component":             inSet([]string{string(entry.Component)}),
		"site":                  siteShape(),
		"site_file":             sourceFileShape(),
		"engine.version":        versionShape(),
		"engine.commit":         commitShape(),
		"engine.go":             goShape(),
		"engine.os":             inSet(enumStrings(knownOS)),
		"engine.arch":           inSet(enumStrings(knownArch)),
		"harness.name":          inSet(enumStrings(knownHarnesses)),
		"harness.version":       harnessShape(),
		"topology.nodes":        inSet(enumStrings(knownNodeBuckets)),
		"topology.stream":       inSet(enumStrings(knownStreams)),
		"topology.coordination": inSet(enumStrings(knownCoordination)),
		"topology.sandbox":      inSet(enumStrings(knownSandboxes)),
		"occurrence.first_seen": regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:00:00Z$`),
		"occurrence.last_seen":  regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:00:00Z$`),
	}
	if entry.SiteVaries {
		shapes["component"] = regexp.MustCompile(`^internal(/[a-z0-9_]+)+$`)
	}
	for _, fact := range entry.Facts {
		if fact.Type == FactEnum {
			shapes["facts."+string(fact.Key)] = inSet(fact.Enum)
		}
	}
	numbers := map[string]bool{"schema": true, "occurrence.count": true}
	if entry.Frames {
		shapes["frames.function"], shapes["frames.file"] = siteShape(), sourceFileShape()
		numbers["frames.line"] = true
	}
	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		switch v := value.(type) {
		case []any:
			if !entry.Frames || prefix != "frames" {
				t.Errorf("%s holds a list no rule allows", prefix)
			}
			for _, inner := range v {
				walk(prefix, inner)
			}
		case map[string]any:
			for key, inner := range v {
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				walk(path, inner)
			}
		case string:
			shape, ok := shapes[prefix]
			if !ok {
				t.Errorf("%s holds a string no rule allows: %q", prefix, v)
			} else if !shape.MatchString(v) {
				t.Errorf("%s = %q is not of its shape", prefix, v)
			}
		case float64:
			declared := strings.HasPrefix(prefix, "facts.")
			if !numbers[prefix] && !declared {
				t.Errorf("%s holds a number no rule allows", prefix)
			}
		case bool:
			if !strings.HasPrefix(prefix, "facts.") {
				t.Errorf("%s holds a boolean no rule allows", prefix)
			}
		default:
			t.Errorf("%s holds %T", prefix, value)
		}
	}
	walk("", doc)
}

// NO ADVERSARIAL VALUE REACHES A REPORT, whichever path it came in by. Every
// string the real failure records can hold is filled with what a company's
// failure carries, those records go through the real listener, and the bytes
// sent are searched and held to the allowlist. Only the fields a detection
// decides on are set back, and the test fails if nothing was sent.
func TestNoAdversarialValueReachesAReport(t *testing.T) {
	t.Parallel()
	for seed := range uint64(40) {
		rng := rand.New(rand.NewPCG(seed, seed^0x5eed))
		r := newRig(t)
		ctx := context.Background()

		// HALF THE RUNS NAME A HARNESS THIS BUILD WOULD NOT REPORT: that class
		// then has no report the relay would take, and sends nothing.
		goodHarness := seed%2 == 0
		for i := range entryThreshold(ClassContextExhaustedLoop) {
			var collected types.ExecutorCollected
			fillStrings(rng, reflect.ValueOf(&collected).Elem())
			collected.Failed, collected.ErrorKind = true, types.ExecutorErrorContextExhausted
			collected.Agent, collected.ConversationKey = "the same seat", "the same conversation"
			collected.SessionTakenBytes = int64(rng.Uint32())
			collected.HarnessSessionContinued = i%2 == 0
			if goodHarness {
				collected.Harness, collected.HarnessVersion = "claude-code", "2.1.310"
			}
			r.report.OnPublish(ctx, "t", events.New(collected, events.TraceContext{}))
		}
		for range entryThreshold(ClassFailureLoop) {
			var completed types.AgentTurnCompleted
			fillStrings(rng, reflect.ValueOf(&completed).Elem())
			completed.Failed, completed.ErrorKind, completed.Suspended, completed.Stopped = true, kindError, false, false
			completed.Agent = "the same seat"
			r.report.OnPublish(ctx, "t", events.New(completed, events.TraceContext{}))
		}
		var broke panics.Panic
		fillStrings(rng, reflect.ValueOf(&broke).Elem())
		broke.Scope, broke.Kind = panics.ScopeTurn, panics.KindExplicit
		broke.Value = canaries[rng.IntN(len(canaries))]
		broke.Frames = append([]panics.Frame{{Function: modulePrefix + "internal/x.f",
			File: canaries[rng.IntN(len(canaries))], Line: 7}}, broke.Frames...)
		r.report.Panicked(&broke)
		r.report.DeadLettered(ctx, canaries[rng.IntN(len(canaries))], queue.DeferParked)
		var failed types.ExecutorFailed
		fillStrings(rng, reflect.ValueOf(&failed).Elem())
		failed.Reason, failed.ErrorKind = types.ExecutorFailureResumeFailing, kindError
		r.report.OnPublish(ctx, "t", events.New(failed, events.TraceContext{}))

		want := 4
		if goodHarness {
			want = 5
		}
		waitFor(t, "every class to be sent", func() bool { return len(r.relay.received()) >= want })
		r.idle()
		if got := len(r.relay.received()); got != want {
			t.Fatalf("seed %d: %d reports, want %d", seed, got, want)
		}
		for _, body := range r.relay.received() {
			for _, canary := range canaries {
				if bytes.Contains(body, []byte(canary)) {
					t.Fatalf("seed %d: %q reached a report: %s", seed, canary, body)
				}
			}
			assertAllowlisted(t, body)
		}
		r.stop()
	}
}

func entryThreshold(c Class) int {
	entry, _ := entryOf(c)
	return entry.Threshold.Count
}

// A VALUE OF THE WRONG SHAPE BECOMES NO VALUE, at each constructor a value
// outside the engine's source comes through.
func TestAConstructorRefusesWhatIsNotItsShape(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"v1.2.3-acme-internal", "v1.2.3+acme", "v1.2.3+dirty", "1.2.3", "main-XYZ", "main-dev", "main-4fa80e3baf3",
		"main-4fa80e3baf3dd", "main-4FA80E3BAF3D", "main-4fa80e3baf3d+dirty", "main-4fa80e3baf3d\n", "xmain-4fa80e3baf3d",
		"unknown", "/srv/acme", strings.Repeat("v1.2.3", 20), "v1.2.3\n", "(devel) acme",
		"v0.1.1-0.20261106203610-4fa80e3baf3d+dirty",
	} {
		if got := versionOf(v); got != unknownValue {
			t.Errorf("versionOf(%q) = %q, want it refused", v, got)
		}
	}
	for _, v := range []string{
		"v1.2.3", "v1.2.3-rc.1", "v0.1.1-0.20261106203610-4fa80e3baf3d", "v0.0.0-20261106203610-4fa80e3baf3d",
		"v1.2.3-rc.1.0.20261106203610-4fa80e3baf3d", "main-4fa80e3baf3d",
	} {
		if got := versionOf(v); string(got) != v {
			t.Errorf("versionOf(%q) = %q, want it kept", v, got)
		}
	}
	for _, v := range []string{"dev", "(devel)"} {
		if got := versionOf(v); got != "dev" {
			t.Errorf("versionOf(%q) = %q, want dev", v, got)
		}
	}
	if got := commitOf("acme"); got != "" {
		t.Errorf("commitOf(acme) = %q, want none", got)
	}
	if got := goVersionOf("go1.27.2 acme"); got != unknownValue {
		t.Errorf("goVersionOf = %q, want it refused", got)
	}
	if got := harnessOf("claude-code", "2.1.310; curl https://acme.example"); got != nil {
		t.Errorf("harnessOf took a sentence for a release: %+v", got)
	}
	if got := harnessOf("acme-cli", "1.0.0"); got != nil {
		t.Errorf("harnessOf took a harness this build does not name: %+v", got)
	}
	for _, fn := range []string{
		"github.com/acme/secret.Payroll", "main.main", "internal/engine.run",
		"github.com/crewlet/crewlet/internal/engine.run/../../etc",
		"github.com/crewlet/crewlet/internal/engine.run acme", "",
		"github.com/crewlet/crewlet/cmd/crewlet.main",
		"github.com/crewlet/crewlet/internal/" + strings.Repeat("a", 300) + ".f",
	} {
		if got := SiteOf(fn); got != "" {
			t.Errorf("SiteOf(%q) = %q, want none", fn, got)
		}
	}
	const closure = "github.com/crewlet/crewlet/internal/tools.(*Surface).call.func1"
	if got := SiteOf(closure); got != "internal/tools.(*Surface).call.func1" {
		t.Errorf("SiteOf(%q) = %q", closure, got)
	}
	if got := NewTopology(5000, "acme", "acme", "acme"); got.Nodes != "9+" || got.Stream != unknownValue ||
		got.Coordination != unknownValue || got.Sandbox != unknownValue {
		t.Errorf("NewTopology kept what is not its closed set: %+v", got)
	}
}

// A VARYING SITE COMES WITH ITS FILE, or it is no site: the relay reads the file
// to check the function, so what the engine cannot name it does not send. The
// compiler's own symbols have no declaration line for it to find.
func TestALocationIsAFunctionAndItsFile(t *testing.T) {
	t.Parallel()
	const fn = "github.com/crewlet/crewlet/internal/tools.(*Surface).call.func1"
	for _, file := range []string{
		"/home/jane/acme-secret/crewlet/internal/tools/surface.go",
		"github.com/crewlet/crewlet@v0.2.0/internal/tools/surface.go",
		"surface.go",
	} {
		want := Location{Site: "internal/tools.(*Surface).call.func1", File: "internal/tools/surface.go"}
		if got := LocationOf(fn, file); got != want {
			t.Errorf("LocationOf(%q) = %+v, want %+v: only the file's base name is read", file, got, want)
		}
	}
	for name, c := range map[string]struct{ function, file string }{
		"no file":           {fn, ""},
		"a directory":       {fn, "/home/jane/"},
		"not Go":            {fn, "surface.txt"},
		"a sentence":        {fn, "Acme Payroll.go"},
		"no function":       {"", "surface.go"},
		"the binary's main": {"main.main", "main.go"},
		"another module":    {"github.com/acme/secret.F", "f.go"},
		"a package's init":  {"github.com/crewlet/crewlet/internal/x.init", "x.go"},
		"an init closure":   {"github.com/crewlet/crewlet/internal/x.init.func1", "x.go"},
		"a numbered init":   {"github.com/crewlet/crewlet/internal/x.init.0", "x.go"},
		"an older closure":  {"github.com/crewlet/crewlet/internal/x.glob..func1", "x.go"},
	} {
		if got := LocationOf(c.function, c.file); got != (Location{}) {
			t.Errorf("%s: LocationOf = %+v, want none", name, got)
		}
	}
	for _, fn := range []string{"F", "F[...]", "(*T).F", "(*T[...]).F", "T.F", "T[...].F", "F.func1.2", "F.func1.func1",
		"F.gowrap1", "F.deferwrap1", "T.F-fm", "F-range1-range2"} {
		if got := LocationOf("github.com/crewlet/crewlet/internal/x."+fn, "x.go"); got.Site != Site("internal/x."+fn) {
			t.Errorf("LocationOf refused %q, which the site grammar admits", fn)
		}
	}
	if _, ok := FrameOf(fn, "surface.go", 0); ok {
		t.Error("FrameOf took a line zero")
	}
	if _, ok := FrameOf(fn, "surface.go", maxFrameLine+1); ok {
		t.Error("FrameOf took a line past its bound")
	}
	if got, ok := FrameOf(fn, "/srv/acme/surface.go", 437); !ok ||
		got != (Frame{Function: "internal/tools.(*Surface).call.func1", File: "internal/tools/surface.go", Line: 437}) {
		t.Errorf("FrameOf = %+v, %v", got, ok)
	}
}

// A NODE COUNT IS A BUCKET: an exact count in a public issue says more about who
// is asking than about the defect.
func TestANodeCountIsABucket(t *testing.T) {
	t.Parallel()
	for nodes, want := range map[int]NodeBucket{
		-1: "1", 0: "1", 1: "1", 2: "2-3", 3: "2-3", 4: "4-8", 8: "4-8", 9: "9+", 5000: "9+",
	} {
		if got := NewTopology(nodes, "", "", "").Nodes; got != want {
			t.Errorf("%d nodes = %q, want %q", nodes, got, want)
		}
		if !slices.Contains(knownNodeBuckets, want) {
			t.Errorf("%q is not in the exported set", want)
		}
	}
}

// A WIRED SITE CANNOT WIDEN WHAT A REPORT SAYS: a fact the class does not
// declare, or a value outside its set, stops the report.
func TestAFactOutsideItsDeclarationIsRefused(t *testing.T) {
	t.Parallel()
	entry, _ := entryOf(ClassContextExhaustedLoop)
	withBase := func(key FactKey, value FactValue) map[FactKey]FactValue {
		facts := map[FactKey]FactValue{FactSessionSizeBucket: enumFact("0-64KiB"), FactSessionContinued: boolFact(true)}
		facts[key] = value
		return facts
	}
	if _, err := entry.checked(withBase(FactSessionContinued, boolFact(false))); err != nil {
		t.Fatalf("a valid set of facts was refused: %v", err)
	}
	for name, facts := range map[string]map[FactKey]FactValue{
		"undeclared":  withBase(FactDeferCause, enumFact(string(queue.DeferParked))),
		"not its set": withBase(FactSessionSizeBucket, enumFact("Acme-Payroll-Corp")),
		"wrong type":  withBase(FactSessionSizeBucket, intFact(1)),
		"negative":    withBase(FactContextWindowTokens, intFact(-1)),
		"too large":   withBase(FactContextWindowTokens, intFact(maxWindowFact+1)),
	} {
		if _, err := entry.checked(facts); err == nil {
			t.Errorf("%s fact was accepted", name)
		}
	}
}

// A WIRED SITE THAT LEAVES OUT A REQUIRED FACT FAILS HERE, not at the relay, whose
// refusal is not retried; only a fact the class calls optional may be absent.
func TestARequiredFactLeftOutStopsTheReport(t *testing.T) {
	t.Parallel()
	entry, _ := entryOf(ClassContextExhaustedLoop)
	if _, err := entry.checked(map[FactKey]FactValue{FactSessionSizeBucket: enumFact("0-64KiB")}); err == nil {
		t.Error("a required fact was left out and nothing refused it")
	}
	if _, err := entry.checked(map[FactKey]FactValue{
		FactSessionSizeBucket: enumFact("0-64KiB"), FactSessionContinued: boolFact(false),
	}); err != nil {
		t.Errorf("the optional fact was required: %v", err)
	}
}

// AN OCCURRENCE IS TO THE HOUR, AND CAPPED, so a report says neither when the
// failure happened to the second nor how busy the company is.
func TestAnOccurrenceIsRoundedAndCapped(t *testing.T) {
	t.Parallel()
	entry, _ := entryOf(ClassEnginePanicked)
	at := time.Date(2026, 11, 6, 7, 42, 13, 7, time.UTC)
	report, err := build(pending{entry: entry, facts: panicFacts, count: 99_999, first: at, last: at.Add(3 * time.Hour)}, releaseOrigin.Build, Topology{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Occurrence.Count != maxOccurrenceCount {
		t.Errorf("count = %d, want the cap", report.Occurrence.Count)
	}
	if want := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC); !report.Occurrence.FirstSeen.Equal(want) {
		t.Errorf("first_seen = %s, want %s", report.Occurrence.FirstSeen, want)
	}
	if want := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC); !report.Occurrence.LastSeen.Equal(want) {
		t.Errorf("last_seen = %s, want %s", report.Occurrence.LastSeen, want)
	}
}

// NO HEADER SAYS ANYTHING: no credential and no identity goes with a report.
func TestAReportCarriesNoCredential(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.panicOnce("seat")
	r.settled(panicFingerprint)
	r.relay.mu.Lock()
	defer r.relay.mu.Unlock()
	h := r.relay.headers[0]
	for _, name := range []string{"Authorization", "Cookie", "X-Api-Key", "Forwarded", "X-Forwarded-For"} {
		if h.Get(name) != "" {
			t.Errorf("a report carried %s", name)
		}
	}
	if got := h.Get("User-Agent"); got != "crewlet-engine" {
		t.Errorf("User-Agent = %q: it must name no version, host or company", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

// ---- thresholds ---------------------------------------------------------- //

func collectedExhausted(conversation string) types.ExecutorCollected {
	return types.ExecutorCollected{Agent: "seat", Failed: true, ErrorKind: types.ExecutorErrorContextExhausted,
		ConversationKey: conversation, Harness: "claude-code", HarnessVersion: "2.1.310",
		SessionTakenBytes: 700 << 10, HarnessSessionContinued: true, ContextWindowTokens: 1_000_000}
}

func (r *rig) publish(payload events.Payload) {
	r.report.OnPublish(context.Background(), "t", events.NewFrom(payload, events.TraceContext{}))
}

// BELOW THE THRESHOLD IS NOISE: nothing is sent until the Nth, and what is sent
// is the production case exactly.
func TestTheContextLoopIsReportedAtItsThresholdAndNotBefore(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	threshold := entryThreshold(ClassContextExhaustedLoop)
	for range threshold - 1 {
		r.publish(collectedExhausted("conversation"))
	}
	r.idle()
	if got := r.relay.received(); len(got) != 0 {
		t.Fatalf("%d sent below the threshold", len(got))
	}
	r.publish(collectedExhausted("conversation"))
	entry, _ := entryOf(ClassContextExhaustedLoop)
	rec := r.settled(FingerprintOf(entry.ID, entry.Component, entry.Site, "harness.name=claude-code", "session_continued=true"))
	if rec.Status != coord.IssueReportFiled || rec.Issue != genuineIssue {
		t.Errorf("record = %+v, want it filed with the relay's address", rec)
	}
	var sent struct {
		Harness    Harness        `json:"harness"`
		Occurrence Occurrence     `json:"occurrence"`
		Facts      map[string]any `json:"facts"`
	}
	if err := json.Unmarshal(r.relay.received()[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Harness.Name != "claude-code" || sent.Harness.Version != "2.1.310" {
		t.Errorf("harness = %+v", sent.Harness)
	}
	if sent.Occurrence.Count != threshold {
		t.Errorf("count = %d, want %d", sent.Occurrence.Count, threshold)
	}
	if sent.Facts["context_window_tokens"] != float64(1_000_000) || sent.Facts["session_size_bucket"] != "512KiB-1MiB" ||
		sent.Facts["session_continued"] != true {
		t.Errorf("facts = %v", sent.Facts)
	}
}

// IT COUNTS PER SEAT AND CONVERSATION, and each of these ends a run: a run of
// the same seat that succeeded or failed otherwise, and the window lapsing.
func TestAContextLoopIsInARowOnOneConversation(t *testing.T) {
	t.Parallel()
	threshold := entryThreshold(ClassContextExhaustedLoop)
	for name, breaker := range map[string]func(r *rig){
		"a success": func(r *rig) { r.publish(types.ExecutorCollected{Agent: "seat", ConversationKey: "c"}) },
		"another failure": func(r *rig) {
			r.publish(types.ExecutorCollected{Agent: "seat", Failed: true, ErrorKind: types.ExecutorErrorLaunchFailed, ConversationKey: "c"})
		},
		"the window lapsing": func(r *rig) { r.clock.Advance(7 * time.Hour) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			for range threshold - 1 {
				r.publish(collectedExhausted("c"))
			}
			breaker(r)
			r.publish(collectedExhausted("c"))
			r.publish(collectedExhausted("other"))
			r.idle()
			if got := r.relay.received(); len(got) != 0 {
				t.Errorf("%d sent after the row was broken: %s", len(got), got)
			}
		})
	}
}

// A THREAD IS SHARED: another seat succeeding in the same conversation does not
// end this seat's row, and two seats' failures there do not add up to one.
func TestAContextLoopIsCountedPerSeatInASharedConversation(t *testing.T) {
	t.Parallel()
	threshold := entryThreshold(ClassContextExhaustedLoop)
	other := func(failed bool) types.ExecutorCollected {
		rec := collectedExhausted("thread")
		rec.Agent = "another seat"
		if !failed {
			rec = types.ExecutorCollected{Agent: "another seat", ConversationKey: "thread"}
		}
		return rec
	}

	split := newRig(t)
	for range threshold - 1 {
		split.publish(collectedExhausted("thread"))
		split.publish(other(true))
	}
	split.idle()
	if got := split.relay.received(); len(got) != 0 {
		t.Fatalf("%d sent for two seats below the threshold each: %s", len(got), got)
	}

	r := newRig(t)
	for range threshold {
		r.publish(collectedExhausted("thread"))
		r.publish(other(false))
	}
	waitFor(t, "the report", func() bool { return len(r.relay.received()) == 1 })
}

// A QUESTION OR A STOP IS NOT A SUCCESS: a parked run says nothing about the
// conversation's context, so it does not end the row.
func TestAParkedRunDoesNotEndARow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	threshold := entryThreshold(ClassContextExhaustedLoop)
	for range threshold - 1 {
		r.publish(collectedExhausted("c"))
		r.publish(types.ExecutorCollected{ConversationKey: "c", Notes: "stopped to ask: which branch?"})
	}
	r.publish(collectedExhausted("c"))
	waitFor(t, "the report", func() bool { return len(r.relay.received()) == 1 })
}

// ONLY THE ENGINE'S OWN KIND COUNTS: a spent plan or a refused key fails every
// turn of a seat, and no maintainer can mend it.
func TestATurnLoopCountsOnlyTheEnginesOwnKind(t *testing.T) {
	t.Parallel()
	threshold := entryThreshold(ClassFailureLoop)
	turn := func(kind string) types.AgentTurnCompleted {
		return types.AgentTurnCompleted{Agent: "seat", Failed: true, ErrorKind: kind}
	}
	for _, kind := range []string{"rate_limit", "auth", "timeout", "server", "canceled", "fatal",
		"budget_exhausted", "stopped", "sandbox_provider_unavailable", "depth_cap", "stall",
		"coordination_unavailable", "no_data_node", "store_busy", "queue_unavailable", "disk_full",
		types.ErrorKindPanicked} {
		r := newRig(t)
		for range threshold + 1 {
			r.publish(turn(kind))
		}
		r.idle()
		if got := r.relay.received(); len(got) != 0 {
			t.Errorf("kind %s was reported: %s", kind, got)
		}
		r.stop()
	}
	r := newRig(t)
	for range threshold - 1 {
		r.publish(turn(kindError))
	}
	r.publish(turn("rate_limit"))
	r.publish(turn(kindError))
	r.idle()
	if got := r.relay.received(); len(got) != 0 {
		t.Errorf("a row another kind broke was reported: %s", got)
	}
	for range threshold - 1 {
		r.publish(turn(kindError))
	}
	waitFor(t, "the report", func() bool { return len(r.relay.received()) == 1 })
}

// A PANIC NAMES THE FUNCTION THAT PANICKED AND THE FRAMES UNDER IT, so two
// panics are two issues and a maintainer reads who called the one that broke.
func TestAPanicIsReportedAtItsFunctionWithItsFrames(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.report.Panicked(contained(panics.ScopeQueue, callsWritesNilMap))
	r.report.Panicked(contained(panics.ScopeTurn, func() { panic("an explicit panic") }))
	waitFor(t, "both reports", func() bool { return len(r.relay.received()) == 2 })
	r.idle()
	type sentReport struct {
		Site      string            `json:"site"`
		Component string            `json:"component"`
		File      string            `json:"site_file"`
		Frames    []Frame           `json:"frames"`
		Facts     map[string]string `json:"facts"`
	}
	sent := map[string]sentReport{}
	for _, body := range r.relay.received() {
		var report sentReport
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatal(err)
		}
		sent[report.Site] = report
	}
	nilMap, ok := sent["internal/issuereport.writesNilMap"]
	if !ok {
		t.Fatalf("no report at the function that panicked: %v", sent)
	}
	if nilMap.Component != "internal/issuereport" || nilMap.File != "internal/issuereport/issuereport_test.go" {
		t.Errorf("place = %s %s, want the package that broke and the file beside the site", nilMap.Component, nilMap.File)
	}
	if len(nilMap.Frames) < 2 || nilMap.Frames[0].Function != "internal/issuereport.writesNilMap" ||
		nilMap.Frames[1].Function != "internal/issuereport.callsWritesNilMap" || nilMap.Frames[0].Line < 1 ||
		string(nilMap.Frames[0].File) != nilMap.File || len(nilMap.Frames) > maxFrames {
		t.Errorf("frames = %+v, want the site and its caller first, each with its file and line", nilMap.Frames)
	}
	if nilMap.Facts["scope"] != "queue" || nilMap.Facts["panic_kind"] != "nil_map_write" {
		t.Errorf("facts = %v", nilMap.Facts)
	}
	for site, report := range sent {
		if site != "internal/issuereport.writesNilMap" && report.Facts["panic_kind"] != "explicit" {
			t.Errorf("%s: facts = %v, want an explicit panic", site, report.Facts)
		}
	}
}

// ONE THAT NAMES NO FUNCTION OF THE MODULE FALLS BACK TO THE CLASS'S OWN PLACE
// WHOLE, with no file and no frames, since the place is the catalogue's word.
func TestAPanicWithNoPlaceFallsBackToTheClasssOwnPlace(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.report.Panicked(&panics.Panic{Scope: panics.ScopeQueue, Kind: panics.KindOther,
		Frames: []panics.Frame{{Function: "net/http.(*conn).serve", File: "/usr/lib/go/net/http/server.go", Line: 9}}})
	rec := r.settled(panicFingerprint)
	if rec.Status != coord.IssueReportFiled {
		t.Fatalf("record = %+v", rec)
	}
	var sent map[string]any
	if err := json.Unmarshal(r.relay.received()[0], &sent); err != nil {
		t.Fatal(err)
	}
	_, file := sent["site_file"]
	_, frames := sent["frames"]
	if file || frames || sent["component"] != "internal/panics" || sent["site"] != "internal/panics.Recovered" {
		t.Errorf("the fallback sent %v", sent)
	}
}

// AN EXPOSED PANIC SENDS ITS SITE ALONE: one that served input nobody had
// authenticated, or whose site or a frame it would send is in a package that reads
// such input. A frame past the few that are sent exposes nothing.
func TestAnExposedPanicSendsItsSiteAlone(t *testing.T) {
	t.Parallel()
	at := func(pkg, fn string, line int) panics.Frame {
		return panics.Frame{Function: modulePrefix + pkg + "." + fn, File: "/build/" + pkg + "/f.go", Line: line}
	}
	plain := func(extra ...panics.Frame) []panics.Frame {
		return append([]panics.Frame{at("internal/tools", "call", 1), at("internal/tools", "run", 2)}, extra...)
	}
	for name, c := range map[string]struct {
		panic  panics.Panic
		frames bool
	}{
		"nothing exposed":       {panics.Panic{Frames: plain()}, true},
		"before authentication": {panics.Panic{Frames: plain(), Unauthenticated: true}, false},
		"a site in a webhook receiver": {panics.Panic{Frames: []panics.Frame{
			at("internal/api/webhooks", "parse", 1), at("internal/tools", "run", 2)}}, false},
		"a frame in the relay":             {panics.Panic{Frames: plain(at("internal/httprelay", "serve", 3))}, false},
		"a package beneath an exposed one": {panics.Panic{Frames: plain(at("internal/code/host/receive", "pack", 3))}, false},
		"an exposed frame past the sent ones": {panics.Panic{Frames: plain(at("internal/tools", "a", 3),
			at("internal/tools", "b", 4), at("internal/tools", "c", 5), at("internal/httprelay", "serve", 6))}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c.panic.Scope, c.panic.Kind = panics.ScopeAPI, panics.KindOther
			signal := panicSignal(&c.panic)
			if signal.Location.Site == "" {
				t.Fatalf("the site was lost: %+v", signal)
			}
			if got := len(signal.Frames) > 0; got != c.frames {
				t.Errorf("frames sent = %v, want %v: %+v", got, c.frames, signal.Frames)
			}
		})
	}
}

// ---- once per fingerprint per seven days, company-wide -------------------- //

// EVERY NODE DETECTS, ONE SENDS. Two nodes on one fleet store are the company:
// the second is held off by the first's record, and seven days reopen it.
func TestAFingerprintIsSentOncePerWindowAcrossTheFleet(t *testing.T) {
	t.Parallel()
	relay := newFakeRelay(t)
	fleet := memory.NewFleet()
	now := newClock()
	nodeA := newRigOn(t, fleet, relay, now)
	nodeB := newRigOn(t, fleet, relay, now)

	nodeA.panicOnce("seat-a")
	nodeA.settled(panicFingerprint)
	nodeB.panicOnce("seat-b")
	nodeB.idle()
	nodeA.panicOnce("seat-a")
	nodeA.idle()
	if got := len(relay.received()); got != 1 {
		t.Fatalf("%d sent inside the window, want 1", got)
	}

	now.Advance(ResendWindow - time.Minute)
	nodeB.panicOnce("seat-b")
	nodeB.idle()
	if got := len(relay.received()); got != 1 {
		t.Fatalf("%d sent a minute before the window ended, want 1", got)
	}

	now.Advance(time.Minute)
	nodeB.panicOnce("seat-b")
	waitFor(t, "the resend", func() bool { return len(relay.received()) == 2 })
	nodeB.idle()
	rec, _, _ := fleet.IssueReport(context.Background(), string(panicFingerprint))
	if rec.Status != coord.IssueReportDuplicate || rec.Issue != genuineIssue {
		t.Errorf("record = %+v, want the relay's duplicate", rec)
	}
	if !rec.FirstSent.Before(rec.ClaimedAt) {
		t.Errorf("first_sent %s should stay before the latest claim %s", rec.FirstSent, rec.ClaimedAt)
	}
}

// RACING NODES STILL SEND ONCE: the claim is the only thing that decides.
func TestRacingNodesSendOnce(t *testing.T) {
	t.Parallel()
	relay := newFakeRelay(t)
	fleet := memory.NewFleet()
	now := newClock()
	var wg sync.WaitGroup
	rigs := make([]*rig, 6)
	for i := range rigs {
		rigs[i] = newRigOn(t, fleet, relay, now)
	}
	for _, r := range rigs {
		wg.Go(func() { r.panicOnce("seat") })
	}
	wg.Wait()
	waitFor(t, "a send", func() bool { return len(relay.received()) >= 1 })
	for _, r := range rigs {
		r.idle()
	}
	if got := len(relay.received()); got != 1 {
		t.Errorf("%d nodes sent, want 1", got)
	}
}

// failingStore answers every read with an error.
type failingStore struct{ *memory.Fleet }

func (failingStore) IssueReport(context.Context, string) (coord.IssueReport, bool, error) {
	return coord.IssueReport{}, false, errors.New("the store did not answer")
}

// A STORE IT CANNOT READ SENDS NOTHING: the promise is at most once.
func TestAnUnreadableStoreSendsNothing(t *testing.T) {
	t.Parallel()
	relay := newFakeRelay(t)
	reporter := New(Config{Store: failingStore{memory.NewFleet()}, Client: relay.server.Client(),
		URL: relay.server.URL, RetryBase: time.Millisecond, Origin: releaseOrigin})
	reporter.SetEnabled(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.Run(ctx)
	reporter.Observe(ctx, panicSignalFor("s"))
	waitFor(t, "the queue to drain", func() bool {
		reporter.mu.Lock()
		defer reporter.mu.Unlock()
		return len(reporter.queued) == 0
	})
	if got := relay.received(); len(got) != 0 {
		t.Errorf("sent %d with no record to hold it to", len(got))
	}
}

// A DEAD NODE'S CLAIM LAPSES, and the next occurrence takes it over; while it
// stands nobody else sends.
func TestAStuckClaimLapsesOnItsLease(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	now := r.clock.Now()
	if _, _, err := r.fleet.ClaimIssueReport(context.Background(), coord.IssueReport{
		Fingerprint: string(panicFingerprint), Class: string(ClassEnginePanicked),
		Status: coord.IssueReportSending, FirstSent: now, ClaimedAt: now,
	}, func(coord.IssueReport) bool { return true }); err != nil {
		t.Fatal(err)
	}
	r.panicOnce("seat")
	r.idle()
	if got := len(r.relay.received()); got != 0 {
		t.Fatalf("%d sent past another node's live claim", got)
	}
	r.clock.Advance(sendingLease)
	r.panicOnce("seat")
	r.settled(panicFingerprint)
	if got := len(r.relay.received()); got != 1 {
		t.Errorf("%d sent after the lease lapsed, want 1", got)
	}
}

// THE RECORDS ARE BOUNDED: a company past the cap keeps what it has.
func TestRecordsStopAtTheirBound(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	now := r.clock.Now()
	for i := range MaxReports {
		fp := FingerprintOf(ClassEnginePanicked, "internal/x", Site("internal/x.f"+strings.Repeat("a", i)))
		if _, _, err := r.fleet.ClaimIssueReport(context.Background(), coord.IssueReport{
			Fingerprint: string(fp), Class: string(ClassEnginePanicked), Status: coord.IssueReportFiled,
			FirstSent: now, ClaimedAt: now,
		}, func(coord.IssueReport) bool { return true }); err != nil {
			t.Fatal(err)
		}
	}
	r.panicOnce("seat")
	r.idle()
	if got := len(r.relay.received()); got != 0 {
		t.Errorf("%d sent past the bound", got)
	}
}

// ---- retries ------------------------------------------------------------- //

func answering(statuses ...int) func(http.ResponseWriter, int) bool {
	return func(w http.ResponseWriter, attempt int) bool {
		if attempt > len(statuses) {
			return false
		}
		w.WriteHeader(statuses[attempt-1])
		return true
	}
}

// A FAILURE A LATER ATTEMPT COULD MEND IS TRIED AGAIN, and a report that gets
// through is filed like any other.
func TestATransientFailureIsRetried(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusBadGateway} {
		r := newRig(t)
		r.relay.answer = answering(status, status)
		r.panicOnce("seat")
		rec := r.settled(panicFingerprint)
		if rec.Status != coord.IssueReportFiled {
			t.Errorf("status %d: record = %+v, want filed on the third attempt", status, rec)
		}
		if got := len(r.relay.received()); got != 3 {
			t.Errorf("status %d: %d attempts, want 3", status, got)
		}
		r.stop()
	}
}

// A REFUSAL IS NOT TRIED AGAIN: the same bytes would be refused the same way.
func TestARefusalIsNotRetried(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity, http.StatusFound, http.StatusForbidden} {
		r := newRig(t)
		r.relay.answer = answering(status, status, status, status, status, status)
		r.panicOnce("seat")
		rec := r.settled(panicFingerprint)
		if rec.Status != coord.IssueReportFailed {
			t.Errorf("status %d: record = %+v, want failed", status, rec)
		}
		if got := len(r.relay.received()); got != 1 {
			t.Errorf("status %d: %d attempts, want 1", status, got)
		}
		r.stop()
	}
}

// RETRIES HAVE A CAP, and a day later a report nobody took may go again; before
// then it waits, however often the defect recurs.
func TestRetriesStopAtTheirCapAndAFailedSendWaitsADay(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	statuses := make([]int, maxAttempts)
	for i := range statuses {
		statuses[i] = http.StatusServiceUnavailable
	}
	r.relay.answer = answering(statuses...)
	r.panicOnce("seat")
	rec := r.settled(panicFingerprint)
	if rec.Status != coord.IssueReportFailed {
		t.Fatalf("record = %+v, want failed", rec)
	}
	if got := len(r.relay.received()); got != maxAttempts {
		t.Fatalf("%d attempts, want the cap of %d", got, maxAttempts)
	}
	r.idle()
	r.panicOnce("seat")
	r.idle()
	if got := len(r.relay.received()); got != maxAttempts {
		t.Fatalf("%d attempts a moment later, want none more", got)
	}
	r.clock.Advance(failedRetryAfter)
	r.panicOnce("seat")
	waitFor(t, "the retry", func() bool { return len(r.relay.received()) == maxAttempts+1 })
	r.idle()
	if rec, _, _ := r.fleet.IssueReport(context.Background(), string(panicFingerprint)); rec.Status != coord.IssueReportFiled {
		t.Errorf("record = %+v, want filed by the retry", rec)
	}
}

// A RELAY'S WORD IS HELD TO ITS SHAPE: an administrator is offered this address
// as a link, so one naming anywhere else is kept as none.
func TestARelaysAnswerIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		body      string
		status    coord.IssueReportStatus
		wantIssue string
	}{
		"another host":    {`{"status":"filed","issue":"https://evil.example/crewlet/crewlet/issues/1"}`, coord.IssueReportFiled, ""},
		"another repo":    {`{"status":"filed","issue":"https://github.com/evil/crewlet/issues/1"}`, coord.IssueReportFiled, ""},
		"a script":        {`{"status":"filed","issue":"javascript:alert(1)"}`, coord.IssueReportFiled, ""},
		"an unknown word": {`{"status":"maybe","issue":"` + genuineIssue + `"}`, coord.IssueReportFailed, ""},
		"not JSON":        {`<html>`, coord.IssueReportFailed, ""},
		"the real thing":  {`{"status":"filed","issue":"` + genuineIssue + `"}`, coord.IssueReportFiled, genuineIssue},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.relay.answer = func(w http.ResponseWriter, _ int) bool {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, c.body)
				return true
			}
			r.panicOnce("seat")
			rec := r.settled(panicFingerprint)
			if rec.Status != c.status || rec.Issue != c.wantIssue {
				t.Errorf("record = %+v, want status %s issue %q", rec, c.status, c.wantIssue)
			}
		})
	}
}

// THE REAL CLIENT FOLLOWS NO REDIRECT: a report goes to the relay, or nowhere.
func TestTheClientFollowsNoRedirect(t *testing.T) {
	t.Parallel()
	client := NewClient()
	if client.CheckRedirect == nil {
		t.Fatal("the client follows redirects")
	}
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v", err)
	}
	if client.Timeout != requestTimeout {
		t.Errorf("Timeout = %s, want %s", client.Timeout, requestTimeout)
	}
}

// ---- off means off ------------------------------------------------------- //

// OFF IS THE DEFAULT, AND NOTHING HAPPENS: no counting, no queue, no request.
func TestAReporterIsOffUntilTurnedOn(t *testing.T) {
	t.Parallel()
	relay := newFakeRelay(t)
	reporter := New(Config{Store: memory.NewFleet(), Client: relay.server.Client(), URL: relay.server.URL, Origin: releaseOrigin})
	if reporter.Enabled() {
		t.Fatal("a new reporter is on")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.Run(ctx)
	for range 50 {
		reporter.Observe(ctx, panicSignalFor("s"))
		reporter.OnPublish(ctx, "t", events.New(collectedExhausted("c"), events.TraceContext{}))
		reporter.MessageLost(ctx, "s", "", "")
	}
	reporter.mu.Lock()
	tracked, queued := len(reporter.trackers), len(reporter.queued)
	reporter.mu.Unlock()
	if tracked != 0 || queued != 0 || len(reporter.queue) != 0 {
		t.Errorf("off, it kept %d trackers and %d queued", tracked, queued)
	}
	if got := relay.received(); len(got) != 0 {
		t.Errorf("off, it sent %d", len(got))
	}
}

// A REVISION THAT TURNS IT OFF STOPS A SEND UNDER WAY, retries included, and
// what was queued or counted does not come back when it is turned on again.
func TestTurningItOffStopsEverything(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	r.relay.answer = func(w http.ResponseWriter, _ int) bool {
		if attempts.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	r.panicOnce("seat")
	<-entered
	r.report.MessageLost(context.Background(), "queued while the first is under way", "", "")
	threshold := entryThreshold(ClassContextExhaustedLoop)
	for range threshold - 1 {
		r.publish(collectedExhausted("c"))
	}

	r.report.SetEnabled(false)
	rec := r.settled(panicFingerprint)
	if rec.Status != coord.IssueReportFailed {
		t.Errorf("record = %+v, want the cancelled send failed", rec)
	}
	r.idle()
	if got := attempts.Load(); got != 1 {
		t.Errorf("%d attempts after it was turned off, want the one under way", got)
	}

	r.report.SetEnabled(true)
	r.publish(collectedExhausted("c"))
	r.idle()
	if got := len(r.relay.received()); got != 1 {
		t.Errorf("%d requests after turning it on, want the first alone: nothing counted while on before survives off", got)
	}
}

// ---- a turn never waits ---------------------------------------------------- //

// THE QUEUE IS BOUNDED AND NEVER BLOCKS: with no sender running at all, a burst
// of distinct defects returns at once and holds no more than its bound.
func TestABurstNeverBlocksAndStaysBounded(t *testing.T) {
	t.Parallel()
	relay := newFakeRelay(t)
	reporter := New(Config{Store: memory.NewFleet(), Client: relay.server.Client(), URL: relay.server.URL, Origin: releaseOrigin})
	reporter.SetEnabled(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 5000 {
			reporter.Observe(context.Background(), Signal{Class: ClassEnginePanicked, Subject: "s", Facts: panicFacts,
				Location: Location{Site: Site("internal/x.f" + strings.Repeat("a", i%150)), File: "internal/x/f.go"}})
			reporter.Observe(context.Background(), Signal{Class: ClassContextExhaustedLoop,
				Subject: strings.Repeat("c", i%maxTrackers+1), Kind: "k"})
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Observe blocked")
	}
	if got := len(reporter.queue); got > queueDepth {
		t.Errorf("queue holds %d, past its depth %d", got, queueDepth)
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if got := len(reporter.trackers); got > maxTrackers {
		t.Errorf("%d trackers, past the bound %d", got, maxTrackers)
	}
}

// A DEFECT IN THE REPORTER ENDS IN A LOG LINE: a report is never worth a turn.
func TestAPanicInsideTheReporterStaysInside(t *testing.T) {
	t.Parallel()
	reporter := New(Config{Store: memory.NewFleet(), Origin: releaseOrigin, Now: func() time.Time { panic("the clock broke") }})
	reporter.SetEnabled(true)
	reporter.Observe(context.Background(), panicSignalFor("s"))
	reporter.Panicked(contained(panics.ScopeTurn, callsWritesNilMap))
}

// ---- only a released or clean module build reports ------------------------ //

// THE RULE OVER WHAT A BINARY RECORDED: a build whose code anybody can read, an
// unmodified tree and a commit its version agrees with, or it stays inert.
func TestOnlyABuildAnybodyCanReadMayReport(t *testing.T) {
	t.Parallel()
	const hash = "4fa80e3baf3d"
	commit := hash + strings.Repeat("a", 28)
	for _, c := range []struct {
		name, version, revision string
		modified, moduleCache   bool
		want                    InertReason
	}{
		{"a release tag", "v1.2.3", commit, false, false, ""},
		{"a prerelease", "v1.2.3-rc.1", commit, false, false, ""},
		{"the image of main", "main-" + hash, commit, false, false, ""},
		{"a local module pseudo-version", "v0.1.1-0.20261106203610-" + hash, commit, false, false, ""},
		{"a go install of a tag", "v1.2.3", "", false, true, ""},
		{"a go install of a pseudo-version", "v0.1.1-0.20261106203610-" + hash, "", false, true, ""},

		{"dev", "dev", commit, false, false, InertDevelopment},
		{"(devel)", "(devel)", commit, false, false, InertDevelopment},
		{"unknown", "unknown", commit, false, false, InertDevelopment},
		{"a private build's name", "v1.2.3-acme-internal", commit, false, false, InertDevelopment},
		{"a stamp of main on another commit", "main-000000000000", commit, false, false, InertDevelopment},
		{"a pseudo-version of another commit", "v0.1.1-0.20261106203610-000000000000", commit, false, false, InertDevelopment},
		{"a module build of no version", "(devel)", "", false, true, InertDevelopment},

		{"a dirty pseudo-version", "v0.1.1-0.20261106203610-" + hash + "+dirty", commit, true, false, InertModified},
		{"a dirty marker alone", "v1.2.3+dirty", commit, false, false, InertModified},
		{"a stamped tag over a modified tree", "v1.2.3", commit, true, false, InertModified},
		{"a stamp of main over a modified tree", "main-" + hash, commit, true, false, InertModified},

		{"no commit recorded", "v1.2.3", "", false, false, InertNoCommit},
		{"a commit of no shape", "v1.2.3", "acme", false, false, InertNoCommit},
		{"a stamp of main with no commit", "main-" + hash, "", false, false, InertNoCommit},
		{"a stamp of main that says it is a module build", "main-" + hash, "", false, true, InertNoCommit},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := originOf(c.version, c.revision, c.modified, c.moduleCache).InertReason(); got != c.want {
				t.Errorf("InertReason = %q, want %q", got, c.want)
			}
		})
	}
	if got := (Origin{}).InertReason(); got == "" {
		t.Error("a zero origin may report")
	}
}

// A GO INSTALL RECORDS NO COMMIT: its report leaves the field out, and the relay
// resolves its version to one. Every other build states its own.
func TestAModuleBuildSendsNoCommit(t *testing.T) {
	t.Parallel()
	origin := originOf("v1.2.3", "", false, true)
	if origin.InertReason() != "" {
		t.Fatalf("a go install may not report: %q", origin.InertReason())
	}
	relay := newFakeRelay(t)
	reporter := New(Config{Store: memory.NewFleet(), Client: relay.server.Client(), URL: relay.server.URL, Origin: origin,
		Topology: func(context.Context) Topology { return NewTopology(1, "embedded", "local", "direct") }, Now: newClock().Now})
	reporter.SetEnabled(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.Run(ctx)
	reporter.Observe(ctx, panicSignalFor("s"))
	waitFor(t, "the report", func() bool { return len(relay.received()) == 1 })
	var sent struct {
		Engine map[string]any `json:"engine"`
	}
	if err := json.Unmarshal(relay.received()[0], &sent); err != nil {
		t.Fatal(err)
	}
	if _, carried := sent.Engine["commit"]; carried || sent.Engine["version"] != "v1.2.3" {
		t.Errorf("engine = %v", sent.Engine)
	}
}

// AN INERT BUILD DOES NOTHING WITH THE SETTING ON: no counting, no record and no
// request, and the setting is still read back so the dashboard can say why.
func TestAnInertBuildSendsNothingWhateverTheSetting(t *testing.T) {
	t.Parallel()
	relay := newFakeRelay(t)
	fleet := memory.NewFleet()
	reporter := New(Config{Store: fleet, Client: relay.server.Client(), URL: relay.server.URL,
		Origin: originOf("(devel)", strings.Repeat("a", 40), false, false)})
	reporter.SetEnabled(true)
	if !reporter.Enabled() || reporter.Inert() != InertDevelopment {
		t.Fatalf("Enabled = %v, Inert = %q; want the setting on and the reason named", reporter.Enabled(), reporter.Inert())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.Run(ctx)
	for range 20 {
		reporter.Observe(ctx, panicSignalFor("s"))
		reporter.MessageLost(ctx, "s", "", "")
		reporter.OnPublish(ctx, "t", events.New(collectedExhausted("c"), events.TraceContext{}))
	}
	time.Sleep(20 * time.Millisecond)
	reporter.mu.Lock()
	tracked := len(reporter.trackers)
	reporter.mu.Unlock()
	if tracked != 0 || len(reporter.queue) != 0 {
		t.Errorf("inert, it kept %d trackers and %d queued", tracked, len(reporter.queue))
	}
	if got := relay.received(); len(got) != 0 {
		t.Errorf("inert, it sent %d", len(got))
	}
	if all, err := fleet.ListIssueReports(ctx); err != nil || len(all) != 0 {
		t.Errorf("inert, it recorded %+v, %v", all, err)
	}
}

// NO DOCUMENT REACHES THE ORIGIN OR THE RELAY: the company's setting is the one
// switch and holds nothing else, so the test-only override cannot come from it.
func TestNoSettingReachesTheOrigin(t *testing.T) {
	t.Parallel()
	if got := reflect.TypeFor[config.IssueReports]().NumField(); got != 1 {
		t.Errorf("config.IssueReports has %d fields; an address or an override in the document "+
			"would let whoever writes it send anywhere, or from a development build", got)
	}
	// TWO BLOCKS, each its own consent: the defect reports and the operating
	// insights (ADR-0119), whose own gate is internal/insights'.
	if got := reflect.TypeFor[config.Diagnostics]().NumField(); got != 2 {
		t.Errorf("config.Diagnostics has %d fields, want the issue reports and the insights blocks", got)
	}
	wiring, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "internal", "engine", "issuereports.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"issuereport.OriginOf(version.String())"} {
		if !bytes.Contains(wiring, []byte(want)) {
			t.Errorf("the engine's wiring does not read %s", want)
		}
	}
	for _, refused := range []string{"issuereport.Origin{", "URL:", "Client:"} {
		if bytes.Contains(wiring, []byte(refused)) {
			t.Errorf("the engine's wiring sets %s, which only a test may", refused)
		}
	}
}

// A CLASS WITH A HARNESS NEEDS ONE, and a class without never carries one: the
// relay refuses both, so neither is offered to it.
func TestAReportsHarnessFollowsItsClass(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	for range entryThreshold(ClassContextExhaustedLoop) {
		rec := collectedExhausted("c")
		rec.HarnessVersion = "a run that recorded none"
		r.publish(rec)
	}
	r.idle()
	if got := len(r.relay.received()); got != 0 {
		t.Errorf("%d sent with no harness, on a class the relay requires one of", got)
	}
	r.report.Observe(context.Background(), Signal{Class: ClassDeadLettered, Subject: "s",
		Harness: harnessOf("claude-code", "2.1.310"), Facts: map[FactKey]FactValue{FactDeferCause: enumFact(string(queue.DeferSeatMoved))}})
	waitFor(t, "the report", func() bool { return len(r.relay.received()) == 1 })
	var sent map[string]any
	if err := json.Unmarshal(r.relay.received()[0], &sent); err != nil {
		t.Fatal(err)
	}
	if _, carried := sent["harness"]; carried {
		t.Errorf("a class with no harness carried one: %v", sent)
	}
}

// A FIXED SITE IS FIXED: a signal's own site is read only where the class varies,
// so a mistake at a wired site cannot split one defect into two issues.
func TestASignalsSiteCountsOnlyWhereTheClassVaries(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.report.Observe(context.Background(), Signal{Class: ClassDeadLettered, Subject: "s",
		Location: Location{Site: "internal/elsewhere.f", File: "internal/elsewhere/f.go"},
		Facts:    map[FactKey]FactValue{FactDeferCause: enumFact(string(queue.DeferSeatMoved))}})
	entry, _ := entryOf(ClassDeadLettered)
	rec := r.settled(FingerprintOf(entry.ID, entry.Component, entry.Site,
		string(FactDeferCause)+"="+string(queue.DeferSeatMoved)))
	if rec.Status != coord.IssueReportFiled {
		t.Errorf("record = %+v", rec)
	}
}

// ---- every wired path ----------------------------------------------------- //

// wiredSignal is one path a class's report is built from. A class has a row for
// each way its facts or its place can differ, so the export is proved against
// what the wired sites really send: the OpenCode row is how a report the relay
// refused forever would have been found.
type wiredSignal struct {
	name  string
	class Class
	fire  func(r *rig)
}

// wiredSignals is every row. A class with none fails
// [TestEveryWiredSignalBuildsAReportTheExportAccepts].
var wiredSignals = []wiredSignal{
	{"the context loop under Claude Code", ClassContextExhaustedLoop, func(r *rig) {
		for range entryThreshold(ClassContextExhaustedLoop) {
			r.publish(collectedExhausted("c"))
		}
	}},
	{"the context loop under OpenCode, whose window the engine does not set", ClassContextExhaustedLoop, func(r *rig) {
		for range entryThreshold(ClassContextExhaustedLoop) {
			rec := collectedExhausted("c")
			rec.Harness, rec.HarnessVersion = "opencode", "1.0.70"
			r.publish(rec)
		}
	}},
	{"the context loop with no session taken", ClassContextExhaustedLoop, func(r *rig) {
		for range entryThreshold(ClassContextExhaustedLoop) {
			rec := collectedExhausted("c")
			rec.SessionTakenBytes, rec.HarnessSessionContinued = 0, false
			r.publish(rec)
		}
	}},
	{"a turn loop nothing stamped", ClassFailureLoop, func(r *rig) {
		for range entryThreshold(ClassFailureLoop) {
			r.publish(types.AgentTurnCompleted{Agent: "seat", Failed: true, ErrorKind: kindError})
		}
	}},
	{"a turn loop at a stamped place", ClassFailureLoop, func(r *rig) {
		for range entryThreshold(ClassFailureLoop) {
			r.publish(stampedTurn(rebaseFor))
		}
	}},
	{"a panic that named its place and frames", ClassEnginePanicked, func(r *rig) {
		r.report.Panicked(contained(panics.ScopeTurn, callsWritesNilMap))
	}},
	{"a panic that named none of the module's functions", ClassEnginePanicked, func(r *rig) {
		r.report.Panicked(&panics.Panic{Scope: panics.ScopeQueue, Kind: panics.KindOther})
	}},
	{"a panic before authentication", ClassEnginePanicked, func(r *rig) {
		p := contained(panics.ScopeAPI, callsWritesNilMap)
		p.Unauthenticated = true
		r.report.Panicked(p)
	}},
	{"a crash nothing recovered", ClassProcessCrashed, func(r *rig) {
		read, _ := parseCrash(moduleTrace("panic: runtime error: invalid memory address or nil pointer dereference",
			modulePrefix+"internal/store.(*DB).query", modulePrefix+"internal/engine.(*Engine).serve"))
		r.report.Observe(context.Background(), crashSignal(read))
	}},
	{"a native library's fault on a thread of its own", ClassProcessCrashed, func(r *rig) {
		read, _ := parseCrash("[signal SIGSEGV: segmentation violation]\n\ngoroutine 9 [running]:\n")
		r.report.Observe(context.Background(), crashSignal(read))
	}},
	{"a failed migration at its file", ClassMigrationFailed, func(r *rig) {
		r.report.Observe(context.Background(), migrationSignal(t0Migration))
	}},
	{"a failed migration whose file is outside the shape", ClassMigrationFailed, func(r *rig) {
		mark := t0Migration
		mark.Version = "../../etc/passwd"
		r.report.Observe(context.Background(), migrationSignal(mark))
	}},
	{"a run with no parked state", ClassRunStranded, func(r *rig) {
		r.publish(types.ExecutorFailed{Agent: "seat", Reason: types.ExecutorFailureNoExecuteState})
	}},
	{"a suspension nobody recorded", ClassRunStranded, func(r *rig) {
		r.publish(types.ExecutorFailed{Agent: "seat", Reason: types.ExecutorFailureSuspensionUnrecorded})
	}},
	{"a resume that kept failing on the engine's own error", ClassRunStranded, func(r *rig) {
		r.publish(types.ExecutorFailed{Agent: "seat", Reason: types.ExecutorFailureResumeFailing, ErrorKind: kindError})
	}},
	{"a lost message nothing stamped", ClassFailureLoop, func(r *rig) {
		r.report.MessageLost(context.Background(), "seat", "", "")
	}},
	{"a lost message at a stamped place", ClassFailureLoop, func(r *rig) {
		r.report.MessageLost(context.Background(), "seat", rebaseFor, "/srv/build/internal/engine/rebase.go")
	}},
}

// handBackSignals is a dead letter for every hand-back cause, each its own
// identity.
func handBackSignals() []wiredSignal {
	out := make([]wiredSignal, 0, len(queue.DeferCauses))
	for _, cause := range queue.DeferCauses {
		out = append(out, wiredSignal{"a hand-back for " + string(cause), ClassDeadLettered, func(r *rig) {
			r.report.DeadLettered(context.Background(), "seat", cause)
		}})
	}
	return out
}

// EVERY WIRED PATH BUILDS WHAT THE EXPORT ACCEPTS, each sent through the real
// listener and held by the export's own rules. A 400 is not retried, so a mismatch
// here is a class whose reports are lost for good and no error anywhere says so.
func TestEveryWiredSignalBuildsAReportTheExportAccepts(t *testing.T) {
	t.Parallel()
	export, err := CatalogueJSON()
	if err != nil {
		t.Fatal(err)
	}
	covered := map[Class]bool{}
	for _, signal := range append(slices.Clone(wiredSignals), handBackSignals()...) {
		covered[signal.class] = true
		t.Run(signal.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			signal.fire(r)
			waitFor(t, "the report", func() bool { return len(r.relay.received()) == 1 })
			r.idle()
			if got := r.relay.received(); len(got) != 1 {
				t.Fatalf("%d reports", len(got))
			} else if refused := relayRefusal(export, got[0]); refused != nil {
				t.Errorf("the export refuses it: %v\n%s", refused, got[0])
			}
		})
	}
	for _, entry := range catalogue {
		if !covered[entry.ID] {
			t.Errorf("%s has no row in wiredSignals: nothing proves its reports are accepted", entry.ID)
		}
	}
}

// THE RULES THEMSELVES REFUSE: a validator that took anything would prove nothing
// of the paths above. Each case is the good report with one thing wrong.
func TestTheRelaysRulesRefuseWhatTheExportForbids(t *testing.T) {
	t.Parallel()
	export, err := CatalogueJSON()
	if err != nil {
		t.Fatal(err)
	}
	good := func(class Class, at Location, facts map[FactKey]FactValue, harness *Harness) map[string]any {
		entry, _ := entryOf(class)
		now := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
		report, err := build(pending{entry: entry, at: at, facts: facts, harness: harness, count: 1, first: now, last: now},
			releaseOrigin.Build, NewTopology(3, "embedded", "local", "direct"))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := report.Encode()
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	placed := Location{Site: "internal/tools.call", File: "internal/tools/surface.go"}
	context := map[FactKey]FactValue{FactSessionSizeBucket: enumFact("0-64KiB"), FactSessionContinued: boolFact(true)}
	harness := harnessOf("claude-code", "2.1.310")
	check := func(doc map[string]any) error {
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return relayRefusal(export, raw)
	}
	if err := check(good(ClassEnginePanicked, placed, panicFacts, nil)); err != nil {
		t.Fatalf("a good panic report was refused: %v", err)
	}
	withFrames := good(ClassEnginePanicked, placed, panicFacts, nil)
	withFrames["frames"] = []any{map[string]any{"function": placed.Site, "file": placed.File, "line": 1},
		map[string]any{"function": "internal/tools.run", "file": "internal/tools/run.go", "line": 9}}
	if err := check(withFrames); err != nil {
		t.Fatalf("a good panic report with frames was refused: %v", err)
	}
	if err := check(good(ClassContextExhaustedLoop, Location{}, context, harness)); err != nil {
		t.Fatalf("a good context report was refused: %v", err)
	}
	for name, breaks := range map[string]func() map[string]any{
		"an older schema": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			doc["schema"] = 1
			return doc
		},
		"an unknown field": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			doc["note"] = "hello"
			return doc
		},
		"a varying site with no file": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			delete(doc, "site_file")
			return doc
		},
		"a file outside the site's package": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			doc["site_file"] = "internal/other/surface.go"
			return doc
		},
		"a component that is not the site's": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			doc["component"] = "internal/panics"
			doc["fingerprint"] = string(FingerprintOf(ClassEnginePanicked, "internal/panics", placed.Site))
			return doc
		},
		"a fingerprint of something else": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			doc["fingerprint"] = strings.Repeat("0", 64)
			return doc
		},
		"a file on a fixed site": func() map[string]any {
			doc := good(ClassDeadLettered, Location{}, map[FactKey]FactValue{FactDeferCause: enumFact(string(queue.DeferSeatMoved))}, nil)
			doc["site_file"] = "internal/engine/turn.go"
			return doc
		},
		"a site on a fixed class": func() map[string]any {
			doc := good(ClassDeadLettered, Location{}, map[FactKey]FactValue{FactDeferCause: enumFact(string(queue.DeferSeatMoved))}, nil)
			doc["site"] = "internal/engine.other"
			return doc
		},
		"an exact node count": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			doc["topology"].(map[string]any)["nodes"] = 3
			return doc
		},
		"frames on a class that takes none": func() map[string]any {
			doc := good(ClassDeadLettered, Location{}, map[FactKey]FactValue{FactDeferCause: enumFact(string(queue.DeferSeatMoved))}, nil)
			doc["frames"] = []any{map[string]any{"function": doc["site"], "file": "internal/engine/turn.go", "line": 1}}
			return doc
		},
		"frames that do not start at the site": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			doc["frames"] = []any{map[string]any{"function": "internal/tools.run", "file": "internal/tools/run.go", "line": 1}}
			return doc
		},
		"a frame with no line": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			doc["frames"] = []any{map[string]any{"function": placed.Site, "file": placed.File, "line": 0}}
			return doc
		},
		"a scope outside the set": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			doc["facts"].(map[string]any)["scope"] = "payroll"
			return doc
		},
		"a required fact left out": func() map[string]any {
			doc := good(ClassContextExhaustedLoop, Location{}, context, harness)
			delete(doc["facts"].(map[string]any), string(FactSessionContinued))
			return doc
		},
		"an undeclared fact": func() map[string]any {
			doc := good(ClassContextExhaustedLoop, Location{}, context, harness)
			doc["facts"].(map[string]any)["defer_cause"] = string(queue.DeferParked)
			return doc
		},
		"a stamp of main with no commit": func() map[string]any {
			doc := good(ClassEnginePanicked, placed, panicFacts, nil)
			engine := doc["engine"].(map[string]any)
			engine["version"] = "main-4fa80e3baf3d"
			delete(engine, "commit")
			return doc
		},
		"a harness left out": func() map[string]any {
			doc := good(ClassContextExhaustedLoop, Location{}, context, harness)
			delete(doc, "harness")
			return doc
		},
	} {
		if err := check(breaks()); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A CLASS THAT TAKES FRAMES HOLDS THEM TO THE SHAPE, from the place its site was
// read from and bounded, and one that does not drops them, whatever a site sent.
func TestFramesFollowTheirClass(t *testing.T) {
	t.Parallel()
	at := Location{Site: "internal/x.f", File: "internal/x/x.go"}
	frames := make([]Frame, maxFrames+3)
	for i := range frames {
		frames[i] = Frame{Function: at.Site, File: at.File, Line: i + 1}
	}
	fixed, _ := entryOf(ClassDeadLettered)
	if got := fixed.framesOf(at, frames); got != nil {
		t.Errorf("a class that takes none kept %d frames", len(got))
	}
	entry, _ := entryOf(ClassEnginePanicked)
	if got := entry.framesOf(at, frames); len(got) != maxFrames || got[0].Line != 1 {
		t.Errorf("framesOf = %+v, want the innermost %d", got, maxFrames)
	}
	if got := entry.framesOf(at, frames[:0]); got != nil {
		t.Errorf("no frames became %+v", got)
	}
	if got := entry.framesOf(Location{Site: "internal/y.g", File: "internal/y/y.go"}, frames); got != nil {
		t.Errorf("frames that do not start at the site were kept: %+v", got)
	}
}

// ONLY THE ENGINE'S OWN INVARIANTS STRAND A RUN: each of the three is one issue,
// and the other endings are its environment, a person's act or already a panic.
func TestARunIsStrandedOnlyByTheEnginesOwnInvariant(t *testing.T) {
	t.Parallel()
	for _, rec := range []types.ExecutorFailed{
		{Reason: types.ExecutorFailureCollect}, {Reason: types.ExecutorFailureAbandoned},
		{Reason: types.ExecutorFailureClaimStranded}, {Reason: types.ExecutorFailureSeatRemoved},
		{Reason: types.ExecutorFailureResumeBroken}, {Reason: types.ExecutorFailureDeadline},
		{Reason: types.ExecutorFailureResumeFailing, ErrorKind: "rate_limit"},
		{Reason: types.ExecutorFailureResumeFailing, ErrorKind: "server"},
		{Reason: types.ExecutorFailureResumeFailing},
		{Reason: "something a newer build says"},
	} {
		r := newRig(t)
		rec.Agent = "seat"
		r.publish(rec)
		r.idle()
		if got := r.relay.received(); len(got) != 0 {
			t.Errorf("%s (%s) was reported: %s", rec.Reason, rec.ErrorKind, got)
		}
		r.stop()
	}
	r := newRig(t)
	seen := map[string]bool{}
	for _, reason := range []string{types.ExecutorFailureNoExecuteState, types.ExecutorFailureSuspensionUnrecorded, types.ExecutorFailureResumeFailing} {
		r.publish(types.ExecutorFailed{Agent: "seat", Reason: reason, ErrorKind: kindError})
		waitFor(t, "the report", func() bool { return len(r.relay.received()) == len(seen)+1 })
		var sent struct {
			Fingerprint string         `json:"fingerprint"`
			Facts       map[string]any `json:"facts"`
		}
		if err := json.Unmarshal(r.relay.received()[len(seen)], &sent); err != nil {
			t.Fatal(err)
		}
		if sent.Facts["reason"] != reason || seen[sent.Fingerprint] {
			t.Errorf("%s: sent %+v, seen %v", reason, sent, seen)
		}
		seen[sent.Fingerprint] = true
	}
}

// A SUSPENSION NOBODY RECORDED IS THE COORDINATION STORE'S AS OFTEN AS THE ENGINE'S,
// so a standing environment alarm holds it back. The other two reasons are not the
// environment's, and are reported under the same alarm.
//
// Mutation: mark another reason, or none, and the count of reports moves.
func TestOnlyAnUnrecordedSuspensionIsShadowed(t *testing.T) {
	t.Parallel()
	alarms := &fleetAlarms{}
	alarms.set(nil, string(ShadowAlarms[0]))
	r := newShadowRig(t, alarms)
	for _, reason := range []string{types.ExecutorFailureSuspensionUnrecorded, types.ExecutorFailureNoExecuteState,
		types.ExecutorFailureResumeFailing} {
		r.publish(types.ExecutorFailed{Agent: "seat", Reason: reason, ErrorKind: kindError})
	}
	waitFor(t, "the two reports", func() bool { return len(r.relay.received()) == 2 })
	r.idle()
	for _, body := range r.relay.received() {
		if bytes.Contains(body, []byte(types.ExecutorFailureSuspensionUnrecorded)) {
			t.Errorf("an unrecorded suspension was reported under %s: %s", ShadowAlarms[0], body)
		}
	}
}

// ---- the catalogue ------------------------------------------------------- //

// THE EXPORT CARRIES WHAT THE RELAY ENFORCES, read back the way a relay reads it:
// the flag on every class, a site on every class, `site_varies` on the one class
// that does, and patterns that refuse a development build's version.
func TestTheExportCarriesWhatTheRelayEnforces(t *testing.T) {
	t.Parallel()
	raw, err := CatalogueJSON()
	if err != nil {
		t.Fatal(err)
	}
	var export struct {
		Schema   int                 `json:"schema"`
		Enums    map[string][]string `json:"enums"`
		Patterns map[string]string   `json:"patterns"`
		Limits   map[string]int      `json:"limits"`
		Classes  []map[string]any    `json:"classes"`
	}
	if err := json.Unmarshal(raw, &export); err != nil {
		t.Fatal(err)
	}
	if export.Schema != SchemaVersion {
		t.Errorf("the export says schema %d, and a report says %d", export.Schema, SchemaVersion)
	}
	if got, want := export.Enums["topology.nodes"], enumStrings(knownNodeBuckets); !slices.Equal(got, want) {
		t.Errorf("enums.topology.nodes = %v, want %v", got, want)
	}
	if export.Limits["frames.count"] != maxFrames || export.Limits["frame.line"] != maxFrameLine {
		t.Errorf("the frame limits are not exported: %v", export.Limits)
	}
	if file := regexp.MustCompile(export.Patterns["source.file"]); file.MatchString("cmd/crewlet/main.go") ||
		file.MatchString("internal/x/../../etc/passwd.go") || !file.MatchString("internal/agent/turn/panic.go") {
		t.Errorf("patterns.source.file = %s, want a Go file of a package under internal/", file)
	}
	if migration := export.Patterns["site.migration"]; migration == "" ||
		regexp.MustCompile(migration).MatchString("internal/store/schema/node/../../x.sql") ||
		!regexp.MustCompile(migration).MatchString("internal/store/schema/replicated/0042_a_table.sql") {
		t.Errorf("patterns.site.migration = %q, want a numbered schema file of an estate", migration)
	}
	varying := []string{}
	for _, class := range export.Classes {
		id, _ := class["id"].(string)
		if _, ok := class["harness"].(bool); !ok {
			t.Errorf("%s exports no harness flag: %v", id, class["harness"])
		}
		if site, _ := class["site"].(string); site == "" {
			t.Errorf("%s exports no site", id)
		}
		if varies, present := class["site_varies"]; present {
			if varies != true {
				t.Errorf("%s: site_varies = %v, want it true or absent", id, varies)
			}
			varying = append(varying, id)
		}
	}
	if want := []string{string(ClassFailureLoop), string(ClassEnginePanicked), string(ClassProcessCrashed),
		string(ClassMigrationFailed)}; !slices.Equal(varying, want) {
		t.Errorf("classes whose site varies = %v, want %v", varying, want)
	}
	version := regexp.MustCompile(export.Patterns["engine.version"])
	for _, refused := range []string{"dev", "(devel)", "unknown", "v1.2.3+dirty", "main-dev", "main-4fa80e3baf3d+dirty",
		"v0.1.1-0.20261106203610-4fa80e3baf3d+dirty", "v1.2.3-acme"} {
		if version.MatchString(refused) {
			t.Errorf("patterns.engine.version admits %q", refused)
		}
	}
	for _, admitted := range []string{"v1.2.3", "v1.2.3-rc.1", "v0.1.1-0.20261106203610-4fa80e3baf3d", "main-4fa80e3baf3d"} {
		if !version.MatchString(admitted) {
			t.Errorf("patterns.engine.version refuses %q", admitted)
		}
	}
	if site := regexp.MustCompile(export.Patterns["site"]); site.MatchString("cmd/crewlet.main") ||
		!site.MatchString("internal/tools.(*Surface).call.func1") {
		t.Errorf("patterns.site = %s, want a function of a package under internal/", site)
	}
}

// liveSites maps each catalogue site, a stable token, to the function it stands
// for today. Renaming the function edits this table and nothing on the wire: the
// token stays, so its fingerprint and its issue do too.
var liveSites = map[Site]string{
	"internal/sandbox.collectedRecord":               "internal/sandbox.collectedRecord",
	"internal/engine.(*Engine).publishTurnCompleted": "internal/engine.(*Engine).publishTurnCompleted",
	"internal/engine.(*Dispatcher).Dispatch":         "internal/engine.(*Dispatcher).Dispatch",
	"internal/panics.Recovered":                      "internal/panics.Recovered",
	"internal/issuereport.readCrash":                 "internal/issuereport.readCrash",
	"internal/store.(*DB).migrate":                   "internal/store.(*DB).migrate",
	"internal/sandbox.(*Coordinator).settleFailed":   "internal/sandbox.(*Coordinator).settleFailed",
}

// A SITE MAPS TO A REAL FUNCTION: a rename nobody carried over would file every
// report of the class under a place a maintainer cannot find. The token itself
// stays in its package's shape, and its component is that package.
func TestEveryCatalogueSiteMapsToAFunctionInTheSource(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	for _, entry := range catalogue {
		if !siteShape().MatchString(string(entry.Site)) {
			t.Errorf("%s: site %q is not of the site's shape", entry.ID, entry.Site)
		}
		if entry.Site.Package() != entry.Component {
			t.Errorf("%s: site %q is outside its component %q", entry.ID, entry.Site, entry.Component)
		}
		live, mapped := liveSites[entry.Site]
		if !mapped {
			t.Errorf("%s: site %q has no row in liveSites", entry.ID, entry.Site)
			continue
		}
		pkg, fn, ok := strings.Cut(live, ".")
		if !ok {
			t.Errorf("%s: %q names no function", entry.ID, live)
			continue
		}
		if !declared(t, filepath.Join(root, filepath.FromSlash(pkg)), fn) {
			t.Errorf("%s: %s declares no %s", entry.ID, pkg, fn)
		}
	}
	for site := range liveSites {
		if !slices.ContainsFunc(catalogue, func(e Entry) bool { return e.Site == site }) {
			t.Errorf("liveSites names %q, which no class does", site)
		}
	}
}

// ---- a published class does not change ------------------------------------- //

// historyDir is where a reporting tag freezes the export it publishes. A relay holds
// every one of them, so an older release's reports keep validating.
const historyDir = "schema/issue-report-history"

// A PUBLISHED CLASS DOES NOT CHANGE at the schema that published it: an older
// release keeps sending that shape, and the relay holds it to that shape.
// A change is a new class id or a new schema. Holds every frozen file in the tree.
func TestAPublishedClassShapeIsImmutable(t *testing.T) {
	t.Parallel()
	live := liveExport(t)
	dir := filepath.Join(sourcetree.Root(t), filepath.FromSlash(historyDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", historyDir, err)
	}
	for _, entry := range entries {
		if entry.Name() == "README.md" {
			continue
		}
		name, isJSON := strings.CutSuffix(entry.Name(), ".json")
		number, err := strconv.Atoi(name)
		if !isJSON || err != nil {
			t.Errorf("%s/%s is not <schema>.json", historyDir, entry.Name())
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var frozen exportedCatalogue
		if err := json.Unmarshal(raw, &frozen); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if frozen.Schema != number {
			t.Errorf("%s says schema %d", entry.Name(), frozen.Schema)
		}
		if number > SchemaVersion {
			t.Errorf("%s is frozen ahead of this build's schema %d", entry.Name(), SchemaVersion)
		}
		if number == SchemaVersion {
			for _, drift := range shapeDrift(frozen, live) {
				t.Errorf("schema %d is published: %s", number, drift)
			}
		}
	}
}

func liveExport(t *testing.T) exportedCatalogue {
	t.Helper()
	raw, err := CatalogueJSON()
	if err != nil {
		t.Fatal(err)
	}
	var live exportedCatalogue
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatal(err)
	}
	return live
}

// shapeDrift is what live changed of what frozen published. A new class and a
// closed set that grew refuse nothing an older release sends, and every other
// change refuses something. Title, summary and threshold are not shape.
func shapeDrift(frozen, live exportedCatalogue) []string {
	var drift []string
	for _, class := range frozen.Classes {
		i := slices.IndexFunc(live.Classes, func(c exportedClass) bool { return c.ID == class.ID })
		switch {
		case i < 0:
			drift = append(drift, "class "+class.ID+" is gone")
		case !reflect.DeepEqual(class, live.Classes[i]):
			drift = append(drift, "class "+class.ID+" changed shape")
		}
	}
	for name, values := range frozen.Enums {
		for _, value := range values {
			if !slices.Contains(live.Enums[name], value) {
				drift = append(drift, "enum "+name+" lost "+value)
			}
		}
	}
	if !maps.Equal(frozen.Patterns, live.Patterns) {
		drift = append(drift, "the patterns changed")
	}
	if !maps.Equal(frozen.Limits, live.Limits) {
		drift = append(drift, "the limits changed")
	}
	return drift
}

// THE GATE ITSELF: no tag has frozen anything yet, so this is what shows the rule
// above would catch each way a release's reports could start being refused.
func TestAShapeChangeIsCaughtAndAnAdditionIsNot(t *testing.T) {
	t.Parallel()
	mutated := func(change func(c *exportedCatalogue)) exportedCatalogue {
		live := liveExport(t)
		change(&live)
		return live
	}
	frozen := liveExport(t)
	if drift := shapeDrift(frozen, frozen); len(drift) != 0 {
		t.Fatalf("a catalogue drifted from itself: %v", drift)
	}
	for name, change := range map[string]func(c *exportedCatalogue){
		"a class removed":      func(c *exportedCatalogue) { c.Classes = c.Classes[1:] },
		"a site renamed":       func(c *exportedCatalogue) { c.Classes[0].Site = "internal/sandbox.renamed" },
		"a component moved":    func(c *exportedCatalogue) { c.Classes[0].Component = "internal/other" },
		"a fact removed":       func(c *exportedCatalogue) { c.Classes[0].Facts = c.Classes[0].Facts[1:] },
		"a fact retyped":       func(c *exportedCatalogue) { c.Classes[0].Facts[0].Type = "boolean" },
		"a fact made required": func(c *exportedCatalogue) { c.Classes[0].Facts[0].Optional = false },
		"an identity changed":  func(c *exportedCatalogue) { c.Classes[0].Identity = nil },
		"a harness dropped":    func(c *exportedCatalogue) { c.Classes[0].Harness = false },
		"a varying site fixed": func(c *exportedCatalogue) {
			c.Classes[slices.IndexFunc(c.Classes, func(e exportedClass) bool { return e.SiteVaries })].SiteVaries = false
		},
		"an enum narrowed":  func(c *exportedCatalogue) { c.Enums["engine.os"] = []string{"linux"} },
		"a pattern changed": func(c *exportedCatalogue) { c.Patterns["site"] = "^x$" },
		"a limit changed":   func(c *exportedCatalogue) { c.Limits["report.bytes"] = 1 },
	} {
		if drift := shapeDrift(frozen, mutated(change)); len(drift) == 0 {
			t.Errorf("%s was not caught", name)
		}
	}
	for name, change := range map[string]func(c *exportedCatalogue){
		"a new class":   func(c *exportedCatalogue) { c.Classes = append(c.Classes, exportedClass{ID: "x.y"}) },
		"an enum grown": func(c *exportedCatalogue) { c.Enums["engine.os"] = append(c.Enums["engine.os"], "freebsd") },
	} {
		if drift := shapeDrift(frozen, mutated(change)); len(drift) != 0 {
			t.Errorf("%s drifted: %v", name, drift)
		}
	}
}

// declared reports whether a package directory declares fn, spelled as the
// runtime spells it: `name` or `(*Type).name`.
func declared(t *testing.T, dir, fn string) bool {
	t.Helper()
	receiver, name := "", fn
	if rest, ok := strings.CutPrefix(fn, "(*"); ok {
		receiver, name, _ = strings.Cut(rest, ").")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		for _, decl := range file.Decls {
			f, ok := decl.(*ast.FuncDecl)
			if !ok || f.Name.Name != name {
				continue
			}
			if receiver == "" && f.Recv == nil {
				return true
			}
			if receiver != "" && f.Recv != nil {
				if star, ok := f.Recv.List[0].Type.(*ast.StarExpr); ok {
					if id, ok := star.X.(*ast.Ident); ok && id.Name == receiver {
						return true
					}
				}
			}
		}
	}
	return false
}

// EVERY ENTRY IS FIT TO FILE: never a security class, each threshold and example
// real, and the preview of each is a report the allowlist holds.
func TestEveryClassIsFitToFile(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	seen := map[Class]bool{}
	for _, entry := range catalogue {
		if entry.Security {
			t.Errorf("%s: a security defect never goes in a public issue", entry.ID)
		}
		if seen[entry.ID] {
			t.Errorf("%s appears twice", entry.ID)
		}
		seen[entry.ID] = true
		if entry.Title == "" || entry.Summary == "" {
			t.Errorf("%s has no title or summary", entry.ID)
		}
		if entry.Threshold.Count < 1 || entry.Threshold.WindowHours < 1 {
			t.Errorf("%s has no threshold: %+v", entry.ID, entry.Threshold)
		}
		if entry.Frames && !entry.SiteVaries {
			t.Errorf("%s takes frames with a fixed site: a frame is where a varying site was read from", entry.ID)
		}
	}
	previews, err := r.report.Previews(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(previews) != len(catalogue) {
		t.Fatalf("%d previews of %d classes", len(previews), len(catalogue))
	}
	for _, preview := range previews {
		assertAllowlisted(t, preview.Payload)
		if len(preview.Payload) > coord.MaxIssueReportPayloadBytes {
			t.Errorf("%s: preview is %d bytes", preview.Class, len(preview.Payload))
		}
	}
	if got := r.relay.received(); len(got) != 0 {
		t.Errorf("a preview sent %d requests", len(got))
	}
}

// THE LIST IS THE RECORD: what an administrator reads is what was sent.
func TestTheListShowsTheExactBytesSent(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.panicOnce("seat")
	r.settled(panicFingerprint)
	rows, err := r.report.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	row := rows[0]
	if row.Status != "filed" || row.Issue != genuineIssue || row.Fingerprint != string(panicFingerprint) {
		t.Errorf("row = %+v", row)
	}
	if row.Title != catalogue[3].Title {
		t.Errorf("title = %q", row.Title)
	}
	if !bytes.Equal(row.Payload, r.relay.received()[0]) {
		t.Errorf("the row shows\n%s\nthe relay got\n%s", row.Payload, r.relay.received()[0])
	}
}
