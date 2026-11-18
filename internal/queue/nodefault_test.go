package queue_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
)

// A FAILURE THE QUEUE TELLS AS ITS NODE'S IS ONE TEST, asked by every handler
// that keeps such a failure out of its group's poison: a verb the broker did not
// answer, one a queue no longer live refused, and any failure under a context
// that has ended, a stop cancelling the handler. Anything else, a payload too
// large among them, which no node's retry changes, is the message's, and so is
// no failure at all.
//
// Mutation: drop any one of the three, and a handler that asks it keeps that
// node's failure with the poison; tell an unmarked failure as the node's, and a
// message that fails as itself is never kept as poison.
func TestAFailureTheQueueTellsAsItsNodesIsOneTest(t *testing.T) {
	t.Parallel()
	ended, end := context.WithCancel(t.Context())
	end()
	for name, tc := range map[string]struct {
		ctx  context.Context
		err  error
		node bool
	}{
		"a broker that did not answer": {ctx: t.Context(),
			err:  fmt.Errorf("publishing: %w: nats: no responders available for request", queue.ErrUnavailable),
			node: true},
		"a queue no longer live":                {ctx: t.Context(), err: fmt.Errorf("publishing: %w", queue.ErrNotLive), node: true},
		"a handler a stop cancelled":            {ctx: ended, err: fmt.Errorf("collecting: %w", context.Canceled), node: true},
		"an event too large to carry":           {ctx: t.Context(), err: fmt.Errorf("publishing: %w", queue.ErrTooLarge)},
		"a failure of its own":                  {ctx: t.Context(), err: errors.New("the model provider did not answer")},
		"no failure, its handler stopped after": {ctx: ended},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := queue.NodeFault(tc.ctx, tc.err); got != tc.node {
				t.Fatalf("NodeFault(%v) = %v, want %v", tc.err, got, tc.node)
			}
		})
	}
}
