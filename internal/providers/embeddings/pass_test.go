package embeddings_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// passInputs is n short inputs in one scope, the one at poison (or none, for a
// negative index) carrying the fake's refusal marker.
func passInputs(n, poison int) []embeddings.PassInput {
	inputs := make([]embeddings.PassInput, n)
	for i := range inputs {
		text := fmt.Sprintf("durable fact number %d about the release train", i)
		if i == poison {
			text = "a poison note " + text
		}
		inputs[i] = embeddings.PassInput{Scope: "diary/a", ID: fmt.Sprintf("n%03d", i), Text: text}
	}
	return inputs
}

// kept collects what a pass hands its store.
type kept struct {
	ids   []string
	calls int
}

func (k *kept) store(inputs []embeddings.PassInput, vectors [][]float32) error {
	k.calls++
	for i, in := range inputs {
		if vectors[i] != nil {
			k.ids = append(k.ids, in.ID)
		}
	}
	return nil
}

var passAt = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// A REFUSED INPUT IS SPLIT OUT, NOT RETRIED ONE BY ONE: one poison among a full
// call costs at most 1 + 2·log₂128 = 15 requests, plus the one canary that
// judges a refusal met before anything was accepted, every neighbour is
// embedded, and the poison is named once, alone — where retrying each input
// alone was 1 + 128 requests a pass.
func TestAPassIsolatesARefusedInputInFifteenRequestsAndKeepsItsNeighbours(t *testing.T) {
	t.Parallel()
	fake := embeddings.NewFake(16)
	fake.Refuse("poison")
	memory := embeddings.NewRefusals()
	pass := embeddings.NewPass(memory, passAt, 32, 1<<20)
	var k kept
	if err := pass.Embed(t.Context(), fake, passInputs(embeddings.PassBatch, 77), k.store); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if pass.Err() != nil {
		t.Fatalf("a refused input ended the pass: %v", pass.Err())
	}
	if pass.Canaries != 1 || pass.Requests-pass.Canaries > 15 {
		t.Errorf("isolating one input took %d requests and %d canaries, past "+
			"1 + 2·log₂128 = 15 and the one canary its first refusal is judged by",
			pass.Requests-pass.Canaries, pass.Canaries)
	}
	if len(k.ids) != embeddings.PassBatch-1 || pass.Accepted != embeddings.PassBatch-1 {
		t.Errorf("embedded %d inputs (%d accepted), want every neighbour of the poison", len(k.ids), pass.Accepted)
	}
	if len(pass.RefusedAlone) != 1 || pass.RefusedAlone[0].Input.ID != "n077" || pass.RefusedAlone[0].Retry {
		t.Fatalf("refused alone: %+v, want the poison once, fresh", pass.RefusedAlone)
	}
	if pass.Concluded {
		t.Error("one refused input was taken for the configuration's refusal")
	}
}

// AN INPUT REFUSED ALONE COSTS NO REQUEST until its retry is due, and is then
// offered again by itself; accepted at last, it is forgotten.
func TestAnInputRefusedAloneIsHeldBackAndThenOfferedAgainAlone(t *testing.T) {
	t.Parallel()
	fake := embeddings.NewFake(16)
	fake.Refuse("poison")
	memory := embeddings.NewRefusals()
	inputs := passInputs(8, 3)
	first := embeddings.NewPass(memory, passAt, 32, 1<<20)
	var k kept
	if err := first.Embed(t.Context(), fake, inputs, k.store); err != nil {
		t.Fatal(err)
	}

	// The next pass, inside the hour, sends nothing for it.
	before := len(fake.Requests())
	next := embeddings.NewPass(memory, passAt.Add(time.Minute), 32, 1<<20)
	if err := next.Embed(t.Context(), fake, inputs[3:4], k.store); err != nil {
		t.Fatal(err)
	}
	if sent := len(fake.Requests()) - before; sent != 0 || next.Held != 1 {
		t.Fatalf("a held input cost %d requests (held %d) inside its retry", sent, next.Held)
	}

	// Due, it is sent alone — with neighbours it would poison them again —
	// and refused again it is a retry, not a fresh refusal.
	due := embeddings.NewPass(memory, passAt.Add(embeddings.RefusalRetry), 32, 1<<20)
	others := passInputs(4, -1)
	if err := due.Embed(t.Context(), fake, append(others, inputs[3]), k.store); err != nil {
		t.Fatal(err)
	}
	requests := fake.Requests()[before:]
	if len(requests[0]) != 1 || !strings.Contains(requests[0][0], "poison") {
		t.Fatalf("the due retry went as %q, want the poison first and alone", requests[0])
	}
	if len(due.RefusedAlone) != 1 || !due.RefusedAlone[0].Retry {
		t.Fatalf("refused alone: %+v, want the one due retry", due.RefusedAlone)
	}

	// A provider that stops refusing it embeds it on its next retry, and the
	// memory lets it go.
	fixed := embeddings.NewFake(16)
	later := embeddings.NewPass(memory, passAt.Add(2*embeddings.RefusalRetry), 32, 1<<20)
	var fixedKept kept
	if err := later.Embed(t.Context(), fixed, inputs[3:4], fixedKept.store); err != nil {
		t.Fatal(err)
	}
	if len(fixedKept.ids) != 1 || memory.Len() != 0 {
		t.Fatalf("the accepted retry left %v kept and %d remembered", fixedKept.ids, memory.Len())
	}
}

// AN ISOLATION A PASS COULD NOT FINISH IS RESUMED BY THE NEXT ONE, at the size
// it reached, rather than started again at the full call.
func TestAnUnfinishedIsolationResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	fake := embeddings.NewFake(16)
	fake.Refuse("poison")
	memory := embeddings.NewRefusals()
	inputs := passInputs(embeddings.PassBatch, 0)
	var k kept
	first := embeddings.NewPass(memory, passAt, 3, 1<<20)
	if err := first.Embed(t.Context(), fake, inputs, k.store); err != nil {
		t.Fatal(err)
	}
	if first.Requests != 3 || len(first.RefusedAlone) != 0 {
		t.Fatalf("first pass: %d requests, %d refused alone", first.Requests, len(first.RefusedAlone))
	}
	// The next pass is offered what is still unembedded, as a caller's
	// selection would offer it.
	stored := map[string]bool{}
	for _, id := range k.ids {
		stored[id] = true
	}
	var left []embeddings.PassInput
	for _, in := range inputs {
		if !stored[in.ID] {
			left = append(left, in)
		}
	}
	before := len(fake.Requests())
	next := embeddings.NewPass(memory, passAt.Add(time.Minute), 32, 1<<20)
	if err := next.Embed(t.Context(), fake, left, k.store); err != nil {
		t.Fatal(err)
	}
	resumed := fake.Requests()[before]
	if len(resumed) > embeddings.PassBatch/4 {
		t.Fatalf("the next pass began with a request of %d inputs, re-sending halves "+
			"the last one already had refused", len(resumed))
	}
	if len(next.RefusedAlone) != 1 || next.RefusedAlone[0].Input.ID != "n000" {
		t.Fatalf("the resumed isolation refused %+v alone, want the poison", next.RefusedAlone)
	}
	if len(k.ids) != embeddings.PassBatch-1 {
		t.Errorf("%d neighbours embedded across the two passes, want all of them", len(k.ids))
	}
}

// ANY FAILURE THAT IS NOT A REFUSAL ENDS THE PASS after the one request that
// met it: a throttled account, a revoked key or a missing model answers every
// request after it the same way.
func TestAPassEndsAtAFailureThatIsNotAboutAnInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fail  func(*embeddings.Fake)
		class error
	}{
		{"rate limited", func(f *embeddings.Fake) { f.FailTransiently("durable", 100) }, embeddings.ErrTransient},
		{"key revoked", func(f *embeddings.Fake) { f.FailConfiguration("durable", 100) }, embeddings.ErrConfiguration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := embeddings.NewFake(16)
			tc.fail(fake)
			pass := embeddings.NewPass(embeddings.NewRefusals(), passAt, 32, 1<<20)
			var k kept
			for range 3 {
				if err := pass.Embed(t.Context(), fake, passInputs(embeddings.PassBatch, -1), k.store); err != nil {
					t.Fatal(err)
				}
			}
			if !errors.Is(pass.Err(), tc.class) {
				t.Fatalf("the pass ended with %v, want %v", pass.Err(), tc.class)
			}
			if sent := len(fake.Requests()); sent != 1 || k.calls != 0 {
				t.Fatalf("the pass sent %d requests and stored %d calls after the "+
					"failure, want the one request that met it", sent, k.calls)
			}
		})
	}
}

// A REFUSED CONFIGURATION IS CONCLUDED BY THE CANARY, at once: the first
// refusal a pass meets before anything was accepted is judged by one plain word
// sent alone, and a provider refusing that too is refusing its configuration —
// two requests, nothing held against the inputs whose refusal was not theirs,
// every pass held back for the hour, and judged again after it: still refused
// it costs two requests more, fixed it embeds everything with no canary at all.
func TestARefusedConfigurationIsConcludedByTheCanary(t *testing.T) {
	t.Parallel()
	fake := embeddings.NewFake(16)
	fake.Refuse("") // every input carries the empty marker: a refused setting
	memory := embeddings.NewRefusals()
	var k kept
	pass := embeddings.NewPass(memory, passAt, 16, 1<<20)
	if err := pass.Embed(t.Context(), fake, passInputs(embeddings.PassBatch, -1), k.store); err != nil {
		t.Fatal(err)
	}
	if !pass.Concluded || !errors.Is(pass.Err(), embeddings.ErrRefusedWhole) {
		t.Fatalf("a provider refusing the canary too was not concluded refused: "+
			"concluded %v, %v", pass.Concluded, pass.Err())
	}
	if pass.Requests != 2 || pass.Canaries != 1 {
		t.Fatalf("concluding took %d requests (%d canaries), want the refused call and its canary",
			pass.Requests, pass.Canaries)
	}
	if canary := fake.Requests()[1]; len(canary) != 1 || strings.Contains(canary[0], "durable") {
		t.Fatalf("the judging request was %q, want one plain word and none of the inputs", canary)
	}
	if memory.Len() != 0 || len(pass.RefusedAlone) != 0 {
		t.Fatalf("a refusal that was the configuration's was held against the inputs: "+
			"%d remembered, %d refused alone", memory.Len(), len(pass.RefusedAlone))
	}

	before := len(fake.Requests())
	held := embeddings.NewPass(memory, passAt.Add(time.Minute), 16, 1<<20)
	if err := held.Embed(t.Context(), fake, passInputs(embeddings.PassBatch, -1), k.store); err != nil {
		t.Fatal(err)
	}
	if !held.Paused || len(fake.Requests()) != before {
		t.Fatalf("the pass after the conclusion was not held back: paused %v, %d requests",
			held.Paused, len(fake.Requests())-before)
	}

	again := embeddings.NewPass(memory, passAt.Add(embeddings.RefusalRetry), 16, 1<<20)
	if err := again.Embed(t.Context(), fake, passInputs(embeddings.PassBatch, -1), k.store); err != nil {
		t.Fatal(err)
	}
	if again.Paused || !again.Concluded || again.Requests != 2 {
		t.Fatalf("the pass after the pause: paused %v, concluded %v, %d requests — want "+
			"it judged again, in two", again.Paused, again.Concluded, again.Requests)
	}

	fixed := embeddings.NewFake(16)
	healed := embeddings.NewPass(memory, passAt.Add(2*embeddings.RefusalRetry), 16, 1<<20)
	if err := healed.Embed(t.Context(), fixed, passInputs(embeddings.PassBatch, -1), k.store); err != nil {
		t.Fatal(err)
	}
	if healed.Err() != nil || healed.Accepted != embeddings.PassBatch || healed.Canaries != 0 {
		t.Fatalf("a fixed configuration after the pause: %v, %d accepted, %d canaries",
			healed.Err(), healed.Accepted, healed.Canaries)
	}
}

// POISON INPUTS ARE NEVER THE CONFIGURATION, however few and however alone:
// the rows a fill meets in steady state are the ones whose embed failed when
// they were written, so a fresh memory — after any boot, or any change to the
// provider's configuration — meeting
// nothing but two poisons is the ordinary case, and the rule that concluded
// from two inputs refused alone with nothing accepted blamed the provider
// configuration for them and stopped the node's whole fill for an hour.
func TestPoisonInputsAloneAreNeverTakenForARefusedConfiguration(t *testing.T) {
	t.Parallel()
	fake := embeddings.NewFake(16)
	fake.Refuse("poison")
	memory := embeddings.NewRefusals()
	poisons := []embeddings.PassInput{
		{Scope: "diary/a", ID: "p1", Text: "a poison note one"},
		{Scope: "episodes/b", ID: "p2", Text: "a poison note two"},
	}
	var k kept
	for i, at := range []time.Time{passAt, passAt.Add(embeddings.RefusalRetry),
		passAt.Add(2 * embeddings.RefusalRetry)} {
		pass := embeddings.NewPass(memory, at, 16, 1<<20)
		inputs := poisons
		if i == 2 {
			// A NEW poison after the retries: fresh, and still an input's.
			inputs = append(inputs, embeddings.PassInput{Scope: "diary/c", ID: "p3",
				Text: "a poison note three"})
		}
		if err := pass.Embed(t.Context(), fake, inputs, k.store); err != nil {
			t.Fatal(err)
		}
		if pass.Concluded || pass.Paused || pass.Err() != nil {
			t.Fatalf("pass %d took poison inputs for the configuration's refusal: "+
				"concluded %v, paused %v, %v", i, pass.Concluded, pass.Paused, pass.Err())
		}
		if len(pass.RefusedAlone) != len(inputs) || pass.Canaries != 1 {
			t.Fatalf("pass %d refused %d inputs alone with %d canaries, want each "+
				"poison isolated and the first refusal judged once", i,
				len(pass.RefusedAlone), pass.Canaries)
		}
	}
	if len(k.ids) != 0 {
		t.Fatalf("stored %v from inputs the provider refused", k.ids)
	}
}

// A PASS THAT HAS HAD ANYTHING ACCEPTED SPENDS NO CANARY: the acceptance is the
// evidence, so a working provider costs no request beyond the isolation.
func TestAPassWithAnAcceptanceSendsNoCanary(t *testing.T) {
	t.Parallel()
	fake := embeddings.NewFake(16)
	fake.Refuse("poison")
	pass := embeddings.NewPass(embeddings.NewRefusals(), passAt, 16, 1<<20)
	var k kept
	if err := pass.Embed(t.Context(), fake, passInputs(4, -1), k.store); err != nil {
		t.Fatal(err)
	}
	if err := pass.Embed(t.Context(), fake, passInputs(1, 0), k.store); err != nil {
		t.Fatal(err)
	}
	if pass.Canaries != 0 || pass.Requests != 2 || len(pass.RefusedAlone) != 1 {
		t.Fatalf("after an acceptance a refusal cost %d canaries over %d requests "+
			"(%d refused alone), want none beyond the two calls", pass.Canaries,
			pass.Requests, len(pass.RefusedAlone))
	}
}

// AN ACCEPTANCE PROVES ONE PASS, NOT THE MEMORY: a setting the endpoint stops
// honouring after the memory's provider worked is concluded at the first pass
// that meets it, where an acceptance remembered for the memory's life held
// every input back one by one instead, each a request of its own an hour.
func TestAConfigurationRefusedAfterItWorkedIsConcludedAtOnce(t *testing.T) {
	t.Parallel()
	memory := embeddings.NewRefusals()
	var k kept
	working := embeddings.NewFake(16)
	first := embeddings.NewPass(memory, passAt, 16, 1<<20)
	if err := first.Embed(t.Context(), working, passInputs(4, -1), k.store); err != nil {
		t.Fatal(err)
	}
	refusing := embeddings.NewFake(16)
	refusing.Refuse("")
	later := embeddings.NewPass(memory, passAt.Add(time.Minute), 16, 1<<20)
	if err := later.Embed(t.Context(), refusing, passInputs(embeddings.PassBatch, -1), k.store); err != nil {
		t.Fatal(err)
	}
	if !later.Concluded || later.Requests != 2 {
		t.Fatalf("a configuration refused after it worked: concluded %v after %d requests",
			later.Concluded, later.Requests)
	}
}

// A CANARY THAT CANNOT ANSWER JUDGES NOTHING AND ENDS THE PASS: its failure is
// a fact about the provider like any other, the refusal it was sent to judge is
// taken as the inputs' — which costs least whichever it was — and the next
// pass judges its own first refusal again.
func TestACanaryThatCannotAnswerEndsThePassAndConcludesNothing(t *testing.T) {
	t.Parallel()
	fake := embeddings.NewFake(16)
	fake.Refuse("poison")
	fake.FailTransiently("note", 1) // refused first, so only the canary meets it
	memory := embeddings.NewRefusals()
	var k kept
	pass := embeddings.NewPass(memory, passAt, 16, 1<<20)
	if err := pass.Embed(t.Context(), fake, passInputs(2, 0), k.store); err != nil {
		t.Fatal(err)
	}
	if pass.Concluded || !errors.Is(pass.Err(), embeddings.ErrTransient) || pass.Requests != 2 {
		t.Fatalf("a canary that failed: concluded %v, %v, after %d requests — want the "+
			"pass ended by its failure and nothing concluded", pass.Concluded, pass.Err(), pass.Requests)
	}
	next := embeddings.NewPass(memory, passAt.Add(time.Minute), 16, 1<<20)
	if err := next.Embed(t.Context(), fake, passInputs(2, 0), k.store); err != nil {
		t.Fatal(err)
	}
	if next.Err() != nil || next.Concluded || next.Canaries != 1 || len(next.RefusedAlone) != 1 {
		t.Fatalf("the next pass: %v, concluded %v, %d canaries, %d refused alone — want the "+
			"refusal judged again and the poison isolated", next.Err(), next.Concluded,
			next.Canaries, len(next.RefusedAlone))
	}
}

// EACH INPUT WHOLE, IN ONE REQUEST WHERE THEY FIT: a short input is the vector
// EmbedBatch gives it, a long one is the pool EmbedWhole gives it — and both,
// with every piece of the long one, went in one request rather than one round
// trip an input. An input with nothing to embed is never sent.
func TestAPassEmbedsEachInputWholeAsEmbedWholeWould(t *testing.T) {
	t.Parallel()
	limits := embeddings.Limits{InputBytes: 32, BatchInputs: 64, BatchBytes: 4096}
	f := embeddings.NewFake(64)
	f.SetLimits(limits)
	short := "the release train is thursdays"
	long := strings.Repeat("deploy failing staging runbook rollback ", 6)
	inputs := []embeddings.PassInput{
		{Scope: "s", ID: "short", Text: short},
		{Scope: "s", ID: "empty", Text: " \n "},
		{Scope: "s", ID: "long", Text: long},
	}
	got := map[string][]float32{}
	pass := embeddings.NewPass(embeddings.NewRefusals(), passAt, 16, 1<<20)
	if err := pass.Embed(t.Context(), f, inputs,
		func(in []embeddings.PassInput, vectors [][]float32) error {
			for i, input := range in {
				got[input.ID] = vectors[i]
			}
			return nil
		}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, sent := got["empty"]; sent {
		t.Error("an input with nothing to embed was sent")
	}

	reference := embeddings.NewFake(64)
	reference.SetLimits(limits)
	alone, err := reference.EmbedBatch(t.Context(), []string{short})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if !slices.Equal(got["short"], alone[0]) {
		t.Error("a short input is not the vector EmbedBatch gives it")
	}
	whole, err := embeddings.EmbedWhole(t.Context(), reference, long)
	if err != nil {
		t.Fatalf("EmbedWhole: %v", err)
	}
	if !slices.Equal(got["long"], whole) {
		t.Error("a long input is not the pool EmbedWhole gives it")
	}
	pieces := 1 + len(embeddings.Chunks(long, limits.InputBytes))
	if requests := f.Requests(); len(requests) != 1 || len(requests[0]) != pieces {
		t.Fatalf("requests = %d (%v), want ONE carrying all %d pieces",
			len(requests), requests, pieces)
	}

	nothing := embeddings.NewFake(16)
	empty := embeddings.NewPass(embeddings.NewRefusals(), passAt, 16, 1<<20)
	if err := empty.Embed(t.Context(), nothing, inputs[1:2], func([]embeddings.PassInput, [][]float32) error {
		t.Error("a pass of nothing to embed stored something")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(nothing.Requests()) != 0 {
		t.Fatalf("nothing to embed sent %d requests", len(nothing.Requests()))
	}
}

// A PASS IS BOUNDED BY WHAT IT SENDS, refusals included: a pass whose bytes are
// spent sends no further call, and the first call of a pass always carries one
// piece, so an input larger than a whole pass still moves.
func TestAPassIsBoundedByWhatItSends(t *testing.T) {
	t.Parallel()
	fake := embeddings.NewFake(16)
	inputs := passInputs(embeddings.PassBatch, -1)
	one := len(embeddings.Prepare(inputs[0].Text))
	pass := embeddings.NewPass(embeddings.NewRefusals(), passAt, 32, 10*one)
	var k kept
	if err := pass.Embed(t.Context(), fake, inputs, k.store); err != nil {
		t.Fatal(err)
	}
	if pass.Bytes > 10*one+one || len(k.ids) > 11 {
		t.Fatalf("a pass of %d bytes sent %d bytes and embedded %d inputs", 10*one, pass.Bytes, len(k.ids))
	}
	if pass.Open() {
		t.Error("a pass that could not afford its next call is still open")
	}

	big := embeddings.NewPass(embeddings.NewRefusals(), passAt, 32, 1)
	var bigKept kept
	if err := big.Embed(t.Context(), fake, inputs[:1], bigKept.store); err != nil {
		t.Fatal(err)
	}
	if len(bigKept.ids) != 1 {
		t.Fatal("an input larger than the whole pass was never sent")
	}

	refused := embeddings.NewFake(16)
	refused.Refuse("durable")
	spent := embeddings.NewPass(embeddings.NewRefusals(), passAt, 2, 1<<20)
	if err := spent.Embed(t.Context(), refused, inputs, k.store); err != nil {
		t.Fatal(err)
	}
	if spent.Requests != 2 || spent.Open() {
		t.Fatalf("refused requests were not charged: %d sent, open %v", spent.Requests, spent.Open())
	}
}

// longLimits is a small model's limits — four pieces of at most 64 bytes a
// request — so a text of a few kilobytes needs many requests.
var longLimits = embeddings.Limits{InputBytes: 64, BatchInputs: 4, BatchBytes: 4096}

// longInput is an input of distinct words — so every piece embeds to its own
// vector and a word names the piece it is in — and its pieces under
// longLimits.
func longInput(t *testing.T, words int) (embeddings.PassInput, []string) {
	t.Helper()
	parts := make([]string, words)
	for i := range parts {
		parts[i] = fmt.Sprintf("w%04dz", i)
	}
	text := strings.Join(parts, " ")
	pieces := embeddings.Chunks(text, longLimits.InputBytes)
	if len(pieces) < 4*longLimits.BatchInputs {
		t.Fatalf("the fixture is %d pieces, want enough for several requests", len(pieces))
	}
	return embeddings.PassInput{Scope: "episodes/a", ID: "long", Text: text}, pieces
}

// longFake is a fake at longLimits.
func longFake() *embeddings.Fake {
	f := embeddings.NewFake(16)
	f.SetLimits(longLimits)
	return f
}

// wholeOf is the vector EmbedWhole gives text under longLimits.
func wholeOf(t *testing.T, text string) []float32 {
	t.Helper()
	whole, err := embeddings.EmbedWhole(t.Context(), longFake(), text)
	if err != nil {
		t.Fatalf("EmbedWhole: %v", err)
	}
	return whole
}

// vectorsOf collects what a pass hands its store, by input.
type vectorsOf map[string][]float32

func (v vectorsOf) store(inputs []embeddings.PassInput, vectors [][]float32) error {
	for i, in := range inputs {
		v[in.ID] = vectors[i]
	}
	return nil
}

// AN INPUT TOO LONG FOR ONE REQUEST IS SENT A REQUEST AT A TIME, AND POOLED
// EXACTLY: each call is one request of its next pieces, no request is sent
// twice, and the vector is the pool EmbedWhole gives the text, to the bit —
// where the whole input went as one call of every request at once, which a
// pass could not bound.
func TestALongInputIsSentARequestAtATimeAndPooledExactly(t *testing.T) {
	t.Parallel()
	input, pieces := longInput(t, 400)
	fake := longFake()
	got := vectorsOf{}
	pass := embeddings.NewPass(embeddings.NewRefusals(), passAt, 64, 1<<20)
	if err := pass.Embed(t.Context(), fake, []embeddings.PassInput{input}, got.store); err != nil {
		t.Fatal(err)
	}
	want := (len(pieces) + longLimits.BatchInputs - 1) / longLimits.BatchInputs
	if pass.Requests != want || len(fake.Requests()) != want {
		t.Fatalf("the input went in %d requests (%d recorded), want the %d its %d pieces need",
			pass.Requests, len(fake.Requests()), want, len(pieces))
	}
	var sent []string
	for _, request := range fake.Requests() {
		sent = append(sent, request...)
	}
	if !slices.Equal(sent, pieces) {
		t.Fatalf("the requests carried %d pieces, want each of the %d once and in order",
			len(sent), len(pieces))
	}
	if !slices.Equal(got["long"], wholeOf(t, input.Text)) || pass.Accepted != 1 {
		t.Fatalf("the vector is not the pool EmbedWhole gives the text (%d accepted)", pass.Accepted)
	}
	if len(pass.Unfinished) != 0 {
		t.Fatalf("a finished input was reported unfinished: %+v", pass.Unfinished)
	}
}

// AN INPUT LONGER THAN A PASS IS FINISHED BY THE PASSES AFTER IT, WHERE THE
// LAST STOPPED: a pass never sends more requests than it was given, what it
// embedded is kept, and the next pass begins at the next piece — so a text
// longer than any one pass is still embedded, whole, and pays for each piece
// once. Sent as one call it broke the pass's bound by however long it was.
func TestAnInputLongerThanAPassIsFinishedByThePassesAfterIt(t *testing.T) {
	t.Parallel()
	input, pieces := longInput(t, 400)
	fake := longFake()
	memory := embeddings.NewRefusals()
	got := vectorsOf{}
	const perPass = 3
	embedded := 0
	for i := 0; ; i++ {
		if i > len(pieces) {
			t.Fatal("the passes never finished the input")
		}
		before := len(fake.Requests())
		pass := embeddings.NewPass(memory, passAt.Add(time.Duration(i)*time.Minute), perPass, 1<<20)
		if err := pass.Embed(t.Context(), fake, []embeddings.PassInput{input}, got.store); err != nil {
			t.Fatal(err)
		}
		if pass.Requests > perPass {
			t.Fatalf("pass %d sent %d requests, past its %d", i, pass.Requests, perPass)
		}
		if first := fake.Requests()[before]; first[0] != pieces[embedded] {
			t.Fatalf("pass %d began at %q, want the piece the last stopped before (%q)",
				i, first[0], pieces[embedded])
		}
		if _, done := got["long"]; done {
			break
		}
		if len(pass.Unfinished) != 1 || pass.Unfinished[0].Input.ID != "long" ||
			pass.Unfinished[0].Pieces != len(pieces) ||
			pass.Unfinished[0].Embedded != embedded+perPass*longLimits.BatchInputs {
			t.Fatalf("pass %d reported %+v unfinished, want the input with %d of %d pieces",
				i, pass.Unfinished, embedded+perPass*longLimits.BatchInputs, len(pieces))
		}
		embedded = pass.Unfinished[0].Embedded
	}
	var sent []string
	for _, request := range fake.Requests() {
		sent = append(sent, request...)
	}
	if !slices.Equal(sent, pieces) {
		t.Fatalf("the passes sent %d pieces between them, want each of the %d once",
			len(sent), len(pieces))
	}
	if !slices.Equal(got["long"], wholeOf(t, input.Text)) {
		t.Fatal("the input finished across passes is not the pool EmbedWhole gives it")
	}
	if memory.Len() != 0 {
		t.Fatalf("the memory still holds %d inputs after the input was finished", memory.Len())
	}
}

// A FAILURE PART WAY THROUGH A LONG INPUT COSTS THE REQUEST IT MET, NOT THE
// INPUT: the pass ends, as any failure that is not about an input ends it, and
// the next pass sends that request again and goes on from there. Sent as one
// call, a failure part way threw away every request already answered, and the
// next pass began with the same input — so one that always ran out of time or
// met a rate limit stopped the walk for good.
func TestAFailurePartWayThroughALongInputCostsOnlyTheRequestItMet(t *testing.T) {
	t.Parallel()
	input, pieces := longInput(t, 400)
	fake := longFake()
	stop := 5 * longLimits.BatchInputs // the piece the sixth request begins with
	fake.FailTransiently(strings.Fields(pieces[stop])[0], 1)
	memory := embeddings.NewRefusals()
	got := vectorsOf{}
	first := embeddings.NewPass(memory, passAt, 64, 1<<20)
	if err := first.Embed(t.Context(), fake, []embeddings.PassInput{input}, got.store); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(first.Err(), embeddings.ErrTransient) || first.Requests != 6 {
		t.Fatalf("the first pass ended with %v after %d requests, want the failure the "+
			"sixth met", first.Err(), first.Requests)
	}
	if len(first.Unfinished) != 1 || first.Unfinished[0].Embedded != stop {
		t.Fatalf("the first pass left %+v unfinished, want the %d pieces before the failure",
			first.Unfinished, stop)
	}
	before := len(fake.Requests())
	next := embeddings.NewPass(memory, passAt.Add(time.Minute), 64, 1<<20)
	if err := next.Embed(t.Context(), fake, []embeddings.PassInput{input}, got.store); err != nil {
		t.Fatal(err)
	}
	if resumed := fake.Requests()[before]; resumed[0] != pieces[stop] {
		t.Fatalf("the next pass began at %q, want the request the failure met (%q)",
			resumed[0], pieces[stop])
	}
	if !slices.Equal(got["long"], wholeOf(t, input.Text)) {
		t.Fatal("the input finished after a failure is not the pool EmbedWhole gives it")
	}
}

// A PASS'S BYTES BOUND A LONG INPUT TOO: a pass sends what its bytes cover and
// no more — by one piece at most, on its first call, where its whole allowance
// is smaller than one — so the text of one input cannot take more of an
// account's minute than the pass was given.
func TestAPassSendsALongInputOnlyAsFarAsItsBytes(t *testing.T) {
	t.Parallel()
	input, pieces := longInput(t, 400)
	piece := len(pieces[0])
	for _, tc := range []struct {
		name         string
		bytes, sends int
	}{
		{"two pieces' worth", 2*piece + 1, 2},
		{"less than a piece", piece / 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := longFake()
			pass := embeddings.NewPass(embeddings.NewRefusals(), passAt, 64, tc.bytes)
			if err := pass.Embed(t.Context(), fake, []embeddings.PassInput{input},
				vectorsOf{}.store); err != nil {
				t.Fatal(err)
			}
			if pass.Requests != 1 || len(fake.Requests()[0]) != tc.sends {
				t.Fatalf("a pass of %d bytes sent %d requests, the first of %d pieces — want "+
					"one of %d", tc.bytes, pass.Requests, len(fake.Requests()[0]), tc.sends)
			}
			if over := pass.Bytes - tc.bytes; over > 0 && over > piece {
				t.Fatalf("a pass of %d bytes sent %d, more than one piece past them",
					tc.bytes, pass.Bytes)
			}
			if pass.Open() {
				t.Error("a pass whose bytes are spent is still open")
			}
		})
	}
}

// A LONG INPUT REFUSED PART WAY IS HELD BACK, AND ITS RETRY BEGINS WHERE THE
// REFUSAL WAS: the pieces the provider took are kept, so the due retry costs
// the refused request rather than the input's every request again.
func TestALongInputRefusedPartWayRetriesFromTheRefusal(t *testing.T) {
	t.Parallel()
	input, pieces := longInput(t, 400)
	refusing := longFake()
	stop := 3 * longLimits.BatchInputs
	refusing.Refuse(strings.Fields(pieces[stop])[0])
	memory := embeddings.NewRefusals()
	got := vectorsOf{}
	first := embeddings.NewPass(memory, passAt, 64, 1<<20)
	if err := first.Embed(t.Context(), refusing, []embeddings.PassInput{input}, got.store); err != nil {
		t.Fatal(err)
	}
	whole := 0
	for _, piece := range pieces {
		whole += len(piece)
	}
	if len(first.RefusedAlone) != 1 || first.RefusedAlone[0].Input.ID != "long" ||
		first.RefusedAlone[0].Bytes != whole {
		t.Fatalf("refused alone: %d input(s), want the input, named with what its %d "+
			"pieces carry (%d bytes)", len(first.RefusedAlone), len(pieces), whole)
	}
	held := embeddings.NewPass(memory, passAt.Add(time.Minute), 64, 1<<20)
	if err := held.Embed(t.Context(), refusing, []embeddings.PassInput{input}, got.store); err != nil {
		t.Fatal(err)
	}
	if held.Held != 1 || held.Requests != 0 {
		t.Fatalf("inside its retry the input was held %d time(s) and cost %d request(s)",
			held.Held, held.Requests)
	}
	fixed := longFake()
	due := embeddings.NewPass(memory, passAt.Add(embeddings.RefusalRetry), 64, 1<<20)
	if err := due.Embed(t.Context(), fixed, []embeddings.PassInput{input}, got.store); err != nil {
		t.Fatal(err)
	}
	if retried := fixed.Requests()[0]; retried[0] != pieces[stop] {
		t.Fatalf("the due retry began at %q, want the refused request (%q)", retried[0], pieces[stop])
	}
	if !slices.Equal(got["long"], wholeOf(t, input.Text)) {
		t.Fatal("the input finished on its retry is not the pool EmbedWhole gives it")
	}
}

// A LONG INPUT ANSWERED WITH NO DIRECTION PART WAY IS HELD BACK, AND ITS RETRY
// BEGINS AT THAT PIECE. The provider accepted the request, so what it embedded
// before the piece is still the input's: kept, the passes after it neither
// begin the input again at its first piece nor send it at all inside the hour,
// and the retry costs the request that piece is in. Forgotten, as it was, the
// input began again at piece 0 on the next pass and met the same piece on the
// one after — a text longer than a pass paid its every request before that
// piece again each time round, for ever, and took each pass from everything
// behind it.
func TestALongInputAnsweredWithNoDirectionIsHeldAndResumedAtThatPiece(t *testing.T) {
	t.Parallel()
	input, pieces := longInput(t, 400)
	fake := longFake()
	stop := 4 * longLimits.BatchInputs // the piece the fifth request begins with
	fake.AnswerZero(strings.Fields(pieces[stop])[0])
	memory := embeddings.NewRefusals()
	got := vectorsOf{}
	const perPass = 3
	pass := func(at time.Time, provider *embeddings.Fake) *embeddings.Pass {
		t.Helper()
		p := embeddings.NewPass(memory, at, perPass, 1<<20)
		if err := p.Embed(t.Context(), provider, []embeddings.PassInput{input}, got.store); err != nil {
			t.Fatal(err)
		}
		if p.Err() != nil {
			t.Fatalf("an answer with no direction ended the pass: %v", p.Err())
		}
		return p
	}
	first := pass(passAt, fake)
	if len(first.Unfinished) != 1 || first.Unfinished[0].Embedded != perPass*longLimits.BatchInputs {
		t.Fatalf("the first pass left %+v, want the input with its first %d pieces in",
			first.Unfinished, perPass*longLimits.BatchInputs)
	}
	met := passAt.Add(time.Minute)
	second := pass(met, fake)
	whole := 0
	for _, piece := range pieces {
		whole += len(piece)
	}
	if len(second.Unusable) != 1 || second.Unusable[0].Input.ID != "long" ||
		second.Unusable[0].Bytes != whole || second.Unusable[0].Retry ||
		second.Requests != 2 || len(second.Unfinished) != 0 {
		t.Fatalf("the second pass sent %d requests and reported unusable %+v, unfinished "+
			"%+v — want the two that reach the piece, and the input named once, fresh, "+
			"with its %d bytes", second.Requests, second.Unusable, second.Unfinished, whole)
	}
	if v, handed := got["long"]; !handed || v != nil {
		t.Fatalf("the input was handed over as %v, want it settled with no vector", v)
	}
	delete(got, "long")

	// INSIDE THE HOUR it costs nothing, however often a pass meets it.
	for minute := 2; minute < 10; minute++ {
		before := len(fake.Requests())
		held := pass(passAt.Add(time.Duration(minute)*time.Minute), fake)
		if sent := len(fake.Requests()) - before; sent != 0 || held.Held != 1 {
			t.Fatalf("minute %d: the held input cost %d requests (held %d)", minute, sent, held.Held)
		}
	}

	// DUE, IT RESUMES AT THE PIECE THAT HAD NONE, and answered so again it is
	// a retry held for another hour.
	before := len(fake.Requests())
	due := pass(met.Add(embeddings.RefusalRetry), fake)
	if resumed := fake.Requests()[before]; resumed[0] != pieces[stop] {
		t.Fatalf("the due retry began at %q, want the piece that had no direction (%q)",
			resumed[0], pieces[stop])
	}
	if len(due.Unusable) != 1 || !due.Unusable[0].Retry || due.Requests != 1 {
		t.Fatalf("the due retry sent %d requests and reported %+v, want one request and "+
			"the input named as a retry", due.Requests, due.Unusable)
	}
	for _, request := range fake.Requests()[perPass:] {
		if request[0] == pieces[0] {
			t.Fatal("a pass after the first began the input again at its first piece")
		}
	}

	// A PROVIDER THAT ANSWERS IT finishes it from there, as the vector of the
	// whole text, and the memory lets it go.
	fixed := longFake()
	for minute := time.Duration(0); got["long"] == nil; minute++ {
		if minute > time.Duration(len(pieces)) {
			t.Fatal("the fixed provider never finished the input")
		}
		pass(met.Add(2*embeddings.RefusalRetry+minute*time.Minute), fixed)
	}
	if first := fixed.Requests()[0]; first[0] != pieces[stop] {
		t.Fatalf("the retry that was answered began at %q, want %q", first[0], pieces[stop])
	}
	if !slices.Equal(got["long"], wholeOf(t, input.Text)) {
		t.Fatal("the input finished on its retry is not the pool EmbedWhole gives it")
	}
	if memory.Len() != 0 {
		t.Fatalf("the memory still holds %d inputs once the input was embedded", memory.Len())
	}
}

// A SHORT INPUT ANSWERED WITH NO DIRECTION COSTS ONLY ITSELF, AND IS HELD BACK.
// Its neighbours in the request keep their vectors, it is handed over with
// none — a vector of zeros is one no cosine can compare, and stored it would
// read as a row that has its vector — and the passes inside the hour pass over
// it. Due, it is offered again WITH its neighbours: the request was accepted,
// so nothing about it endangers them as a refused input's would.
func TestAShortInputAnsweredWithNoDirectionIsHeldAndItsNeighboursKept(t *testing.T) {
	t.Parallel()
	fake := embeddings.NewFake(16)
	fake.AnswerZero("hollow")
	memory := embeddings.NewRefusals()
	inputs := passInputs(6, -1)
	inputs[2].Text = "a hollow note " + inputs[2].Text
	got := vectorsOf{}
	first := embeddings.NewPass(memory, passAt, 16, 1<<20)
	if err := first.Embed(t.Context(), fake, inputs, got.store); err != nil {
		t.Fatal(err)
	}
	if first.Err() != nil || first.Requests != 1 || first.Accepted != 5 ||
		len(first.Unusable) != 1 || first.Unusable[0].Input.ID != "n002" {
		t.Fatalf("the pass ended %v after %d requests, %d accepted, unusable %+v — want "+
			"one request, every neighbour embedded and the hollow note named",
			first.Err(), first.Requests, first.Accepted, first.Unusable)
	}
	if v, handed := got["n002"]; !handed || v != nil {
		t.Fatalf("the hollow note was handed over as %v, want no vector", v)
	}
	if want := len(embeddings.Prepare(inputs[2].Text)); first.Unusable[0].Bytes != want {
		t.Fatalf("the hollow note was named with %d bytes, want its %d",
			first.Unusable[0].Bytes, want)
	}

	before := len(fake.Requests())
	held := embeddings.NewPass(memory, passAt.Add(time.Minute), 16, 1<<20)
	if err := held.Embed(t.Context(), fake, inputs[2:3], got.store); err != nil {
		t.Fatal(err)
	}
	if sent := len(fake.Requests()) - before; sent != 0 || held.Held != 1 {
		t.Fatalf("inside its retry the hollow note cost %d requests (held %d)", sent, held.Held)
	}

	others := passInputs(3, -1)
	for i := range others {
		others[i].ID = fmt.Sprintf("m%03d", i)
	}
	due := embeddings.NewPass(memory, passAt.Add(embeddings.RefusalRetry), 16, 1<<20)
	if err := due.Embed(t.Context(), fake, append(others, inputs[2]), got.store); err != nil {
		t.Fatal(err)
	}
	requests := fake.Requests()[before:]
	if len(requests) != 1 || len(requests[0]) != 4 {
		t.Fatalf("the due retry went as %q, want one request with its neighbours", requests)
	}
	if len(due.Unusable) != 1 || !due.Unusable[0].Retry {
		t.Fatalf("the due retry reported %+v, want the hollow note again, as a retry", due.Unusable)
	}
}

// AN ANSWER OF ANOTHER WIDTH ENDS THE PASS AND HOLDS NOTHING BACK. A provider
// answering a width the store was not sized for answers every input so — the
// configuration's fault, never an input's — so the pass ends with it as with
// any failure that is not about an input. Judged one input at a time, a pool
// refusing the piece would have held every input back for an hour.
func TestAnAnswerOfAnotherWidthEndsThePassAndHoldsNothing(t *testing.T) {
	t.Parallel()
	long, _ := longInput(t, 400)
	for name, inputs := range map[string][]embeddings.PassInput{
		"short inputs": passInputs(4, -1),
		"a long input": {long},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			memory := embeddings.NewRefusals()
			pass := embeddings.NewPass(memory, passAt, 16, 1<<20)
			if err := pass.Embed(t.Context(), wider{longFake()}, inputs,
				func([]embeddings.PassInput, [][]float32) error {
					t.Error("an answer of another width was handed to the store")
					return nil
				}); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(pass.Err(), embeddings.ErrConfiguration) || pass.Requests != 1 {
				t.Fatalf("the pass ended with %v after %d requests, want the configuration's "+
					"fault at the first", pass.Err(), pass.Requests)
			}
			if memory.Len() != 0 || len(pass.Unusable) != 0 {
				t.Fatalf("the memory holds %d inputs and the pass named %d unusable, want "+
					"nothing held against an input", memory.Len(), len(pass.Unusable))
			}
		})
	}
}

// wider is a provider that reports one width more than it answers.
type wider struct{ *embeddings.Fake }

func (w wider) Width() int { return w.Fake.Width() + 1 }
