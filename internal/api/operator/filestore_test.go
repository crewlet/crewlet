package operator_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// refusingObjects is a file store whose every put fails with err, as the
// object store classifies a failure ([objstore.Store]).
type refusingObjects struct{ err error }

func (o refusingObjects) Put(context.Context, io.Reader, int64, objstore.PutMeta) (objstore.Object, error) {
	return objstore.Object{}, o.err
}

func (refusingObjects) ReadAt(context.Context, objstore.Object, int64, int64) ([]byte, error) {
	return nil, errors.New("not read")
}

// noProjectFiles is a project that holds no file yet, and a writer the call
// never reaches, since the bytes are refused first.
type noProjectFiles struct{}

func (noProjectFiles) Files(context.Context, worklet.FileQuery) (worklet.FileListing, error) {
	return worklet.FileListing{}, nil
}

func (noProjectFiles) File(context.Context, string, string, statelog.Freshness) (worklet.FileDetail, error) {
	return worklet.FileDetail{}, worklet.ErrNoFile
}

func (noProjectFiles) PutFile(context.Context, string, worklet.FilePut) (worklet.WriteResult, error) {
	return worklet.WriteResult{}, errors.New("not reached")
}

func (noProjectFiles) RemoveFile(context.Context, string, string, string, uint64) (worklet.WriteResult, error) {
	return worklet.WriteResult{}, errors.New("not reached")
}

// A FILE STORE'S CONDITION IS ANSWERED ON THE ACT ROUTE AS ON THE UPLOAD ROUTE.
//
// write_project_file is served on this surface, and a full store it met went
// out as `503` with a Retry-After of two seconds beside the tool's "Don't try
// again", because the state log's rule did not know the store's sentinel —
// while the project file's own upload answered the same refusal with none.
// Both read one classification now ([builtin.RetryAfter]): a full store is
// `503` with no Retry-After, under a message that says waiting won't help; a
// store that did not answer is `503` under an election's wait. NEITHER CARRIES
// A SENTENCE OF ITS OWN: the act route answers a person, so a condition nobody
// has worded for one is told in its code's own message, and the tool's
// sentence, written for a model, travels with neither (ADR-0060). Mutation:
// drop the full store from the builtin classification and the first row's
// Retry-After and message fail; drop the unanswered one and the second row's
// do.
func TestAFileStoresConditionIsOneAnswerOnTheActRoute(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err          error
		retry        string
		messageWaits bool
	}{
		"a full store": {fmt.Errorf("objstore: store the object: natsobj: put x: %w",
			objstore.ErrFull), "", false},
		"a store that did not answer": {fmt.Errorf("objstore: store the object: %w: %w",
			objstore.ErrUnanswered, errors.New("nats: timeout")), "4", true},
	} {
		files := noProjectFiles{}
		h := guarded(newSurface(t, operator.Options{
			Halves: fixed(operator.Halves{Work: builtin.WorkDeps{
				Reader: stubWorkReader{}, Writer: (&recordingWork{}).writer,
				Files: files, FileWriter: func(builtin.Actor) builtin.FileWriter { return files },
				Objects: refusingObjects{c.err},
			}}),
			Org: company,
		}))
		status, answer, header := actAnswer(t, h, worklet.WriteProjectFileTool,
			`{"args":{"project":"ENG","path":"a.md","content":"x"}}`)
		if status != http.StatusServiceUnavailable ||
			answer["error"] != string(crewletmcp.RefusalUnavailable) {
			t.Errorf("%s answered %d %v, want 503 unavailable", name, status, answer)
		}
		if detail, has := answer["detail"]; has {
			t.Errorf("%s: carried the detail %q, want the code's own message alone", name,
				detail)
		}
		if got := header.Get("Retry-After"); got != c.retry {
			t.Errorf("%s: Retry-After %q, want %q", name, got, c.retry)
		}
		message, _ := answer["message"].(string)
		if waits := strings.Contains(message, "in a moment"); waits != c.messageWaits {
			t.Errorf("%s: the message %q invites a wait: %v, want %v", name, message, waits,
				c.messageWaits)
		}
		for key, value := range answer {
			if key != "tool" && strings.Contains(fmt.Sprint(value), worklet.WriteProjectFileTool) {
				t.Errorf("%s: %s carries the tool's own sentence: %v", name, key, value)
			}
		}
		if text := fmt.Sprint(answer); strings.Contains(text, "nats") ||
			strings.Contains(text, "reservation") {
			t.Errorf("%s: carried the store's own words: %v", name, answer)
		}
	}
}

// AND A PUBLISH THE FULL STORE REFUSED IS THE SAME ANSWER: the artifact tools
// class it unavailable over the store's own sentinel, as the pipeline wraps it,
// and the one mapping reads that sentinel, through the function every surface
// answers a failed tool call with ([operator.FailTool]). Mutation: drop the full
// store from the builtin classification and this fails.
func TestAPublishTheFullStoreRefusedIsTheSameAnswer(t *testing.T) {
	t.Parallel()
	cause := tools.Classify(tools.RefusalUnavailable, fmt.Errorf("artifactupload: store %q: %w",
		"index.html", objstore.ErrFull))
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/operator/act/"+builtin.PublishArtifactTool, nil)
	operator.FailTool(rec, req, tools.Result{Failed: true, Cause: cause,
		Output: "publish_artifact: nothing was saved: the company's file storage is full."},
		"op-1", operator.About(builtin.PublishArtifactTool, nil))
	answer := decode(t, rec)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "" {
		t.Errorf("a publish refused by a full store answered %d %v (Retry-After %q), want 503 "+
			"with none", rec.Code, answer, rec.Header().Get("Retry-After"))
	}
	if detail, has := answer["detail"]; has {
		t.Errorf("a publish refused by a full store carried the detail %q, want the code's "+
			"own message alone", detail)
	}
}
