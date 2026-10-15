package host

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// EVERY WAY THIS NODE ENDS A GIT PROCESS BEFORE ITS FIRST BYTE is answered as
// that — 503, BusyRetryAfter, a sentence naming why — and logged as the
// REQUEST'S failure, a push's whatever process its sync or its receive ran
// and a fetch's likewise: a deadline, a stall, the process's own input or
// output failing here. None of them is the code log failing, which is what
// each read as before it had a case of its own.
func TestEveryWayTheNodeEndsGitIsAnsweredAsThat(t *testing.T) {
	requests := map[string]struct {
		method, target, line string
	}{
		"a push's advertisement": {http.MethodGet, "/git/acme/info/refs?service=git-receive-pack", "code_push_failed"},
		"a push":                 {http.MethodPost, "/git/acme/git-receive-pack", "code_push_failed"},
		"a fetch's advertisement": {http.MethodGet, "/git/acme/info/refs?service=git-upload-pack",
			"code_fetch_failed"},
		"a fetch": {http.MethodPost, "/git/acme/git-upload-pack", "code_fetch_failed"},
	}
	for _, end := range []struct {
		err         error
		reason, why string
	}{
		{gitproc.ErrDeadline, "deadline", "past its deadline"},
		{gitproc.ErrStalled, "stall", "stalled"},
		{gitproc.ErrInput, "input", "when its input failed"},
		{gitproc.ErrOutput, "output", "when its output failed"},
	} {
		for name, req := range requests {
			t.Run(end.reason+"/"+name, func(t *testing.T) {
				var log lockedBuffer
				s := &Server{log: slog.New(slog.NewJSONHandler(&log, nil))}
				s.life, s.end = context.WithCancel(context.Background())
				defer s.end()
				w := httptest.NewRecorder()
				err := fmt.Errorf("gitproc: git upload-pack was ended: %w", end.err)
				s.syncFailed(w, httptest.NewRequest(req.method, req.target, nil), "acme", err)
				body := w.Body.String()
				if w.Code != http.StatusServiceUnavailable ||
					w.Header().Get("Retry-After") != strconv.Itoa(int(BusyRetryAfter/time.Second)) ||
					!strings.Contains(body, "git on this node was stopped before it answered, "+end.why+" (") ||
					!strings.Contains(body, "retry in "+seconds(BusyRetryAfter)+", or against another node") {
					t.Errorf("answered %d (Retry-After %q) %q", w.Code, w.Header().Get("Retry-After"), body)
				}
				logged := log.String()
				if !strings.Contains(logged, `"msg":"`+req.line+`"`) ||
					!strings.Contains(logged, `"reason":"`+end.reason+`"`) ||
					strings.Contains(logged, "code_git_unavailable") {
					t.Errorf("logged %q, want %s with reason %s and nothing else", logged, req.line, end.reason)
				}
			})
		}
	}
}

// A FAILURE NO RETRY ENDS PROMISES NO RETRY: a pack whose verified bytes are
// not the bundle its row describes, and rows this build cannot install into a
// cache, are each answered 500 with no Retry-After and a sentence that says
// retrying does not end it and names the line an operator acts on. Promised
// "retry in 10 s" and a Retry-After of 10, a client or a balancer obeying
// either started a sync every ten seconds, and for a corrupt pack each one
// downloaded the whole bundle again — up to code.MaxPackBytes — to fail the
// same way.
func TestAFailureNoRetryEndsPromisesNone(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
		err              error
	}{
		{name: "a corrupt pack", line: "code_pack_checksum_mismatch",
			err: &cache.ErrCorruptPack{Repo: "acme", Pack: code.PackRow{PackID: "p1"},
				Err: fmt.Errorf("%w: index_bytes", code.ErrBadBundle)},
			want: "only the node that received the push, which holds its own copy, can serve it"},
		{name: "rows this build cannot install", line: "code_bad_snapshot",
			err:  fmt.Errorf("%w: page 1, row 3: a ref the grammar refuses", cache.ErrBadSnapshot),
			want: "a node running a newer build may serve it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log lockedBuffer
			s := &Server{log: slog.New(slog.NewJSONHandler(&log, nil))}
			s.life, s.end = context.WithCancel(context.Background())
			defer s.end()
			w := httptest.NewRecorder()
			s.syncFailed(w, httptest.NewRequest(http.MethodPost, "/git/acme/git-upload-pack", nil), "acme", tc.err)
			body := w.Body.String()
			if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") != "" {
				t.Errorf("answered %d with Retry-After %q, want 500 and none", w.Code, w.Header().Get("Retry-After"))
			}
			if strings.Contains(body, "retry in") || !strings.Contains(body, "retrying does not end it") ||
				!strings.Contains(body, "an operator must act on this node's "+tc.line+" line") ||
				!strings.Contains(body, tc.want) {
				t.Errorf("answered %q", body)
			}
			if !strings.Contains(log.String(), `"level":"ERROR","msg":"`+tc.line+`"`) {
				t.Errorf("logged %q, want %s at ERROR", log.String(), tc.line)
			}
		})
	}
}
