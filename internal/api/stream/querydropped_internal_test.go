package stream

import (
	"context"
	"encoding/json"
	"testing"
)

// A QUESTION WHOSE SOCKET CLOSED WHILE IT RAN IS DROPPED, and one whose socket
// is open is answered.
//
// A tab that went away mid-read cancels the socket's context, the store reports
// the read as cancelled, and the default arm logged that as
// `stream_query_failed` at WARN: a fault line for a person closing a tab.
// Decided on the socket's context, never on the error, so a question that
// fails on its own deadline while the socket is open is still answered and
// logged as the failure it is.
//
// Mutation: drop the ctx.Err() check and the first row replies, red; decide on
// errors.Is(err, context.Canceled) and the second row replies nothing, red.
func TestAQuestionWhoseSocketClosedIsDropped(t *testing.T) {
	t.Parallel()
	failing := func(context.Context, string, map[string]any) (any, error) {
		return nil, context.Canceled
	}
	for name, c := range map[string]struct {
		closed bool
		want   int
	}{
		"a socket that closed while it ran": {closed: true, want: 0},
		"a socket still open":               {closed: false, want: 1},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		if c.closed {
			cancel()
		}
		client := NewClient(NewAudience(adminPrincipal, nil))
		runQuery(ctx, client, failing, request{Kind: "query", ID: 7, What: "inbox"})
		cancel()
		if got := len(client.Out()); got != c.want {
			t.Errorf("%s: %d frame(s) queued, want %d", name, got, c.want)
			continue
		}
		if c.want == 1 {
			var env Envelope
			if err := json.Unmarshal((<-client.Out()).Raw(), &env); err != nil {
				t.Fatalf("%s: the frame does not decode: %v", name, err)
			}
			if env.Kind != KindError || env.ID != 7 || env.Error != CodeQueryFailed {
				t.Errorf("%s: answered %+v, want the question's %s", name, env, CodeQueryFailed)
			}
		}
	}
}
