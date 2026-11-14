package cdp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// bounded is the case's context under a liveness bound: what a case waits on
// arrives at once when the claim holds, so the bound only turns a hang into a
// failure that says what never came.
func bounded(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A PIPE MESSAGE IS ONE NUL-TERMINATED FRAME, each way, as Chromium's
// --remote-debugging-pipe frames them: a message split across reads waits for
// the rest, two in one read are two, and each command the client writes is
// one JSON object ended by exactly one NUL. The messages hold a newline
// between their tokens, as JSON may.
//
// Mutation: split the reader on '\n' rather than NUL, and the first message
// is read as two that are no CDP message, which ends the connection — red.
func TestAPipeMessageIsOneNULTerminatedFrame(t *testing.T) {
	t.Parallel()
	clientReads, peerWrites := io.Pipe()
	peerReads, clientWrites := io.Pipe()
	c := NewPipe(clientReads, clientWrites, Options{})
	t.Cleanup(func() {
		_ = c.Close()
		_ = peerWrites.Close()
		_ = peerReads.Close()
	})
	ctx := bounded(t)
	l := c.Browser().Listen(testEvent)

	writes := make(chan error, 1)
	go func() {
		for _, chunk := range []string{
			// One message, split across two reads.
			"{\"method\":\"Test.event\",\n\"params\":{\"n\":1",
			"}}\x00",
			// Two messages in one read.
			"{\"method\":\"Test.event\",\n\"params\":{\"n\":2}}\x00{\"method\":\"Test.event\",\"params\":{\"n\":3}}\x00",
		} {
			if _, err := peerWrites.Write([]byte(chunk)); err != nil {
				writes <- err
				return
			}
		}
		writes <- nil
	}()
	for want := 1; want <= 3; want++ {
		m, err := l.Next(ctx)
		if err != nil {
			t.Fatalf("event %d: %v (the connection ended with %v)", want, err, c.Err())
		}
		e, ok, err := testEvent.Of(m)
		if !ok || err != nil || e.N != want {
			t.Fatalf("event %d read as %+v (%v, %v), from %s", want, e, ok, err, m.Params)
		}
	}
	if err := <-writes; err != nil {
		t.Fatal(err)
	}

	// Each command is one frame, ended by exactly one NUL.
	raw := bufio.NewReader(peerReads)
	for i := range 2 {
		calls := make(chan error, 1)
		go func() {
			_, err := Call(ctx, c.Browser(), testCall, testParams{Text: "a\nb"})
			calls <- err
		}()
		frame, err := raw.ReadBytes(0)
		if err != nil {
			t.Fatal(err)
		}
		body := frame[:len(frame)-1]
		var r request
		if bytes.IndexByte(body, 0) >= 0 || json.Unmarshal(body, &r) != nil || r.Method != testCall.name {
			t.Fatalf("command %d framed as %q, want one JSON object of %s and one NUL", i, frame, testCall.name)
		}
		answer, _ := json.Marshal(map[string]any{"id": r.ID, "result": map[string]any{}})
		if _, err := peerWrites.Write(append(answer, 0)); err != nil {
			t.Fatal(err)
		}
		if err := <-calls; err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
	}
}
