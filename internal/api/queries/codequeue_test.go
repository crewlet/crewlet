package queries_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// A QUEUE ASKS FOR TRAINS ONLY WHERE ITS CALLER NAMES THEM, `trains=true`, and a spelling
// that is neither `true` nor `false` is refused, naming both, before the reader is asked.
// Mutation: read `trains` as nothing, read it whatever the caller named, or read an unknown
// spelling as false.
func TestAQueueAsksForTrainsOnlyWhereItsCallerNamesThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args map[string]any
		want bool
	}{
		{"nothing", map[string]any{}, false},
		{"trains", map[string]any{"trains": "true"}, true},
		{"no trains, spelled out", map[string]any{"trains": "false"}, false},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_queue", tc.args); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := stub.last(); got.Kind != gitlet.ReadQueue || got.Trains != tc.want {
			t.Errorf("code_queue naming %s asked %s with trains %t, want a queue read with trains %t", tc.name,
				got.Kind, got.Trains, tc.want)
		}
	}
	stub := &stubCode{}
	_, err := askNative(t, queries.Sources{Code: stub}, "code_queue", map[string]any{"trains": "yes"})
	if !errors.Is(err, queries.ErrBadParams) || !strings.Contains(err.Error(), "true") ||
		!strings.Contains(err.Error(), "false") || len(stub.asked) != 0 {
		t.Errorf("trains=yes: answered %v having asked the reader %d times, want bad_params naming true and "+
			"false before it", err, len(stub.asked))
	}
}
