package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
)

// A HANDLER'S PANIC NAKS WITH THE PANIC IN ITS CHAIN, so whatever classifies the
// failure reads it as a panic, already reported, and not as a fault of its own.
func TestAHandlersPanicNaksAsAPanic(t *testing.T) {
	t.Parallel()
	res := runHandler(context.Background(), func(context.Context) queue.Result { panic("a nil map") })
	var contained *panics.Failure
	if res.Outcome != queue.OutcomeNak || !errors.As(res.Err, &contained) || contained.Panic.Scope != panics.ScopeQueue {
		t.Errorf("result = %+v, want a Nak carrying the contained panic", res)
	}
}
