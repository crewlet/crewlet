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
// they were written, so a fresh memory — after any boot or apply — meeting
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
// spent sends no further call, and the first call of a pass is always sent so
// an input larger than a whole pass still gets one.
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
