package statelog

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
)

// TestAnOversizedRecordRefusesAtRoundOne.
//
// nats.ErrMaxPayload is raised CLIENT-SIDE, before the append reaches the
// broker, so it is not an APIError and fell through to the unknown answer —
// which is the one the publisher must retry, because an unanswered append may
// or may not be on the stream. So a record too large for the broker was
// re-decided for all sixteen rounds and then reported as "the rows kept
// changing under this write": a sentence about contention, which sends whoever
// reads it looking for a peer that is not there.
//
// Nothing about the record changes between rounds. It is too big now and it
// will be too big in a millisecond.
func TestAnOversizedRecordRefusesAtRoundOne(t *testing.T) {
	t.Parallel()
	f, detail := classify(fmt.Errorf("append: %w", nats.ErrMaxPayload))
	if f != faultFull {
		t.Fatalf("classify(ErrMaxPayload) = %v, want faultFull — the unknown answer "+
			"is retried, and no retry can make a record smaller", f)
	}
	if !strings.Contains(detail, "maximum payload") {
		t.Errorf("the detail does not say what is wrong: %q", detail)
	}

	// CONTROL: an error nobody answered is still the unknown answer, or
	// this arm would have swallowed the case the retry exists for.
	if f, _ := classify(errors.New("no responders available for request")); f != faultUnknown {
		t.Errorf("an unanswered append classified as %v, want faultUnknown — it may "+
			"be on the stream and may not, and only the ordered classification "+
			"can say which", f)
	}
	if f, _ := classify(nil); f != faultNone {
		t.Errorf("classify(nil) = %v, want faultNone", f)
	}
}
