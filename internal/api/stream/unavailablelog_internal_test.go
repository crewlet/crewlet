package stream

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A QUESTION'S CONDITION GOES TO THE ANSWERING SURFACE'S LOG, with its own
// fields, at WARN where no wait clears it and DEBUG where one does: a screen
// asks a question again after every hint for as long as a node is catching up,
// so a line per ask is noise beside the alarm that names it, while a refusal
// no wait clears is the administrator's to act on.
//
// NO WAIT CLEARS IS THE ANSWER'S OWN HINT, so a condition the state log does
// not class (a company's file storage full, a note the running node could not
// verify) is written at WARN like a full log: read through the state log's
// rule alone, both went out with no Retry-After and were written at DEBUG.
//
// Mutation: log through this package's logger, drop the fields, log a node
// behind at INFO, or judge the level by [statelog.ConditionLevel], and a row
// goes red.
func TestAQuestionsConditionIsLoggedByItsSurface(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err   error
		level string
	}{
		"a full log": {fmt.Errorf("read: %w", &statelog.Refused{Code: statelog.RefuseLogFull,
			Level: statelog.ReadLinearizable, Detail: "raise stream.tracker_max_bytes"}),
			"level=WARN"},
		"a full file store": {fmt.Errorf("upload: %w", objstore.ErrFull), "level=WARN"},
		"a note the running node could not verify": {fmt.Errorf("steer: %w",
			steer.ErrUnverified), "level=WARN"},
		"a node behind": {fmt.Errorf("read: %w", &statelog.Refused{Code: statelog.RefuseBehind,
			Level: statelog.ReadLinearizable, Detail: "40 000 records behind",
			RetryAfter: time.Second}), "level=DEBUG"},
	} {
		var out bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})).
			With("component", "api")
		LogUnavailable(t.Context(), logger, "api_query_unavailable", c.err, "what", "work_items")
		line := out.String()
		for _, want := range []string{c.level, "component=api", "msg=api_query_unavailable",
			"what=work_items", "error="} {
			if !strings.Contains(line, want) {
				t.Errorf("%s logged %q, want %s in it", name, line, want)
			}
		}
	}
}
