package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// EVERY READER OF THIS FAKE PAIRS ITS SLICES BY INDEX, so a call must append
// to all of them or to none.
//
// # The failure this protects against
//
// [executorPrompt] finds "execute" in calls and reads systems at the SAME
// position. That holds only if one request appends to both without another
// getting in between — and it did not: serve appended the prompt, the offered
// tools and the body before its switch (it must, since the phase is not known
// until the body is parsed), released the lock, and saw appended the phase
// after. Two requests in flight — two seats, or a seat and one of the
// prefetch's auxiliary passes, which is the ordinary case — interleave between
// those sections, and from then on calls[i] describes one call while
// systems[i] describes another.
//
// It cost an intermittent end-to-end failure in which the skills case read a
// prompt with no "## Tool skills" section and failed on a section that was
// present all along, having been handed an auxiliary pass's prompt where it
// asked for the executor's. Invisible under -race, because both halves were
// correctly locked: it is a pairing bug, not a data race.
//
// The invariant is checked under CONCURRENCY because that is the only
// condition it ever broke under, and with each request carrying a marker only
// its own prompt has, so a misalignment is detected rather than inferred from
// lengths that happen to match.
func TestEveryModelCallRecordsOneAlignedEntry(t *testing.T) {
	t.Parallel()
	m := &scriptedModel{}
	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)

	// Two shapes that take DIFFERENT branches of serve's switch and are
	// each identifiable from their own system prompt: an auxiliary
	// memory-filter pass, and the default text round.
	const calls = 60
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var system string
			if i%2 == 0 {
				system = fmt.Sprintf("you are a memory-relevance filter #%d", i)
			} else {
				system = fmt.Sprintf("ordinary round #%d", i)
			}
			post(t, srv.URL, system)
		}()
	}
	wg.Wait()

	phases, systems := m.seen(), m.systemPrompts()
	if len(phases) != calls || len(systems) != calls {
		t.Fatalf("recorded %d phases and %d prompts for %d calls: a call that "+
			"appends to one slice and not the other misaligns every index "+
			"after it", len(phases), len(systems), calls)
	}
	for i, phase := range phases {
		aux := strings.Contains(systems[i], "memory-relevance filter")
		if want := "aux:memory"; aux != (phase == want) {
			t.Fatalf("entry %d says phase %q and carries the prompt %q — the "+
				"phase and the prompt at one index describe different calls",
				i, phase, tail(systems[i]))
		}
	}
}

// post sends one request carrying the given system prompt and no tools, which
// is the shape both branches under test take.
func post(t *testing.T, url, system string) {
	t.Helper()
	body := fmt.Sprintf(`{"system":%q,"messages":[]}`, system)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Errorf("post: %v", err)
		return
	}
	_ = resp.Body.Close()
}
