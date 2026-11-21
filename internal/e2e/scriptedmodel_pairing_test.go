package e2e

import (
	"bytes"
	"encoding/json"
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
// [executorPrompt] finds "execute" in calls and reads systems at the SAME
// position, so one request must append to both without another between. serve
// appends the prompt, tools and body before its switch (the phase is unknown
// until read) and the phase after; concurrent requests interleave there. It is
// a pairing bug, not a data race, so -race cannot see it. Checked under
// CONCURRENCY, with a marker in each request's own prompt.
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
			post(t, srv, system, fmt.Sprintf("skill #%d", i))
		}()
	}
	wg.Wait()

	phases, systems, listed := m.seen(), m.systemPrompts(), m.skillsListed()
	if len(phases) != calls || len(systems) != calls || len(listed) != calls {
		t.Fatalf("recorded %d phases, %d prompts and %d skill lists for %d calls: a "+
			"call that appends to one slice and not the other misaligns every index "+
			"after it", len(phases), len(systems), len(listed), calls)
	}
	for i := range systems {
		_, number, _ := strings.Cut(systems[i], "#")
		if len(listed[i]) != 1 || listed[i][0] != "skill #"+number {
			t.Fatalf("entry %d carries the prompt %q and the skills %q — the prompt "+
				"and the skills at one index describe different calls", i, systems[i], listed[i])
		}
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

// post sends one request the way the scripted harness does, carrying the given
// system prompt and no tools — a text function's request, which is the shape
// both branches under test take.
// IT TAKES THE SERVER, not its URL: sixty goroutines post concurrently here
// and the pool must be this server's own, never the process-global one every
// httptest cleanup in the binary sweeps — see
// [github.com/crewlet/crewlet/internal/httpx/httpxtest].
func post(t *testing.T, srv *httptest.Server, system, skill string) {
	t.Helper()
	body, err := json.Marshal(modelRequest{System: system, Prompt: "## user\n\nHow did the week go?\n",
		Skills: []string{skill}})
	if err != nil {
		t.Errorf("encode the call: %v", err)
		return
	}
	resp, err := srv.Client().Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Errorf("post: %v", err)
		return
	}
	_ = resp.Body.Close()
}
