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

// tailFailed stands in for a failure its own node logged already: a coding
// run's output that the node owning the run couldn't read.
type tailFailed struct{ error }

func (tailFailed) LoggedAtSource() bool { return true }

// Unwrap keeps the failure's own class readable through the mark, as the
// sandbox's own wrapper does: without it a full file store read as an
// unclassed condition with a hint, which is DEBUG with or without the mark.
func (f tailFailed) Unwrap() error { return f.error }

// A FAILURE ITS OWN NODE LOGGED IS A TRACE HERE. The node that owns a coding
// run writes what it met reading the run's output, with its class and its
// text, and the surface that answers the person writes its own line at DEBUG,
// whether the answer is a 503 no wait clears or a 500: at WARN, one failure was
// two WARN lines on every poll of the live view, the asker's naming nothing
// the owner's didn't.
//
// Mutation: judge the level without asking whether the failure was logged
// where it was met, and both rows go red.
func TestAFailureLoggedWhereItWasMetIsATraceHere(t *testing.T) {
	t.Parallel()
	for name, log := range map[string]func(*slog.Logger, error){
		"unavailable": func(l *slog.Logger, err error) {
			LogUnavailable(t.Context(), l, "api_query_unavailable", err, "what", "executor_tail")
		},
		"failed": func(l *slog.Logger, err error) {
			LogFailed(t.Context(), l, "api_query_failed", err, "what", "executor_tail")
		},
	} {
		var out bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
		log(logger, fmt.Errorf("executor_tail: %w", tailFailed{fmt.Errorf("upload: %w",
			objstore.ErrFull)}))
		if line := out.String(); !strings.Contains(line, "level=DEBUG") {
			t.Errorf("%s: a failure its node logged is written %q, want DEBUG", name, line)
		}
		out.Reset()
		log(logger, fmt.Errorf("upload: %w", objstore.ErrFull))
		if line := out.String(); !strings.Contains(line, "level=WARN") {
			t.Errorf("%s: a failure nobody logged yet is written %q, want WARN", name, line)
		}
	}
}
