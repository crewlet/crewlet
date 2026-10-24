package artifactsapi

import (
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE CHECK, over HTTP: a page's text files judged as a publish would judge
// them, and nothing written ([artifactupload.Service.Check]). A person writing
// a page sees the linter's findings as they write, rather than only when a
// publish is refused.

// maxCheckBodyBytes bounds a check's body: the text a check reads
// ([artifactupload.MaxCheckBytes]) as JSON strings, which escaping can grow by
// up to six times for control characters and twice for markup's quotes, plus
// the paths. A body past it is refused before it is read whole.
const maxCheckBodyBytes = 3 * artifactupload.MaxCheckBytes

// The check's budget, per node: a principal's own bucket and the node's.
// A check is asked for after a pause in typing, so a person earns two a second
// with a burst of ten, and the node gives everybody together fifty.
const (
	checksPerPrincipalPerSecond = 2
	checksPrincipalBurst        = 10
	checksPerNodePerSecond      = 50
	checksNodeBurst             = 100
)

// checkScope is the one scope every check is charged to: the node's own.
const checkScope = "checks"

// newCheckLimiter builds the node's check buckets on a clock.
func newCheckLimiter(now func() time.Time) *costlimit.Limiter {
	return costlimit.New(costlimit.Rates{
		PrincipalRate: checksPerPrincipalPerSecond, PrincipalBurst: checksPrincipalBurst,
		ScopeRate: checksPerNodePerSecond, ScopeBurst: checksNodeBurst,
	}, now)
}

// checkBody is one check: the kind the files would be published as, the
// entry page, and every text file by path.
type checkBody struct {
	Kind  artifacts.Kind `json:"kind"`
	Entry string         `json:"entry,omitempty"`
	Files []struct {
		Path string `json:"path"`
		Text string `json:"text"`
	} `json:"files"`
}

// check answers what a publish of the files would say (`POST
// /artifacts/checks`), decided by `create_artifact`: a check is about making
// or publishing a page, and reads no artifact. Answered 200 with every refusal
// and every warning; a refused page is an answer here, not an error.
func (s *Service) check(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionArtifactCreate)
	if !ok {
		return
	}
	var body checkBody
	if !decodeWithin(w, r, &body, false, maxCheckBodyBytes) {
		return
	}
	if refused := s.checks.Take(checkScope, iam.RecordOwner(p), 1); refused != nil {
		scope := "principal"
		if refused.Scope == costlimit.ScopeShared {
			scope = "node"
		}
		httpjson.ThrottledWith(w, httpjson.CodeCheckBudget, refused.RetryAfter, httpjson.Detail{
			"scope": scope})
		return
	}
	pipe, ok := s.pipeline(w, r)
	if !ok {
		return
	}
	files := make([]artifactupload.CheckFile, len(body.Files))
	for i, f := range body.Files {
		files[i] = artifactupload.CheckFile{Path: f.Path, Text: []byte(f.Text)}
	}
	report, err := pipe.Check(r.Context(), s.creatorCall(p, "", ""), body.Kind, body.Entry, files)
	if err != nil {
		writeFailed(w, r, "", "check the page", err)
		return
	}
	httpjson.Write(w, http.StatusOK, report)
}
