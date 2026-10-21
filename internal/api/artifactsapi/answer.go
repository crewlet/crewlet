package artifactsapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/artifactupload/vendor"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// HOW THE SURFACE ANSWERS: an admission refused, a write's three outcomes, and
// every refusal the domain and the pipeline can give, each under its own code,
// so a client branches on a field rather than a sentence.

// admit decides action on the path's artifact, rendering a refusal itself. It
// reports whether the handler goes on.
func (s *Service) admit(w http.ResponseWriter, r *http.Request, action authz.Action) (Admission, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return Admission{}, false
	}
	p, ok := caller(w, r, action)
	if !ok {
		return Admission{}, false
	}
	a, err := s.gate.Admit(r.Context(), p, action, id)
	if err != nil {
		refuseAdmission(w, r, action, err)
		return Admission{}, false
	}
	return a, true
}

// admitContent is [Service.admit] for a read of the path's artifact's content
// at version (zero for the head): [Gate.Content], which refuses an artifact in
// the trash and records the admin path's override first.
func (s *Service) admitContent(w http.ResponseWriter, r *http.Request, version int) (Admission, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return Admission{}, false
	}
	p, ok := caller(w, r, authz.ActionArtifactRead)
	if !ok {
		return Admission{}, false
	}
	a, err := s.gate.Content(r.Context(), p, authz.ActionArtifactRead, id, version)
	if err != nil {
		refuseAdmission(w, r, authz.ActionArtifactRead, err)
		return Admission{}, false
	}
	return a, true
}

// caller is the request's resolved principal, or the refusal of a request
// nobody could resolve, rendered.
func caller(w http.ResponseWriter, r *http.Request, action authz.Action) (iam.Principal, bool) {
	p, how := iam.From(r.Context())
	if how != iam.Resolved {
		operator.RefuseDecision(w, r, action, authz.Decision{Err: iam.Reason(r.Context())})
		return iam.Principal{}, false
	}
	return p, true
}

// refuseAdmission renders an admission that did not admit.
func refuseAdmission(w http.ResponseWriter, r *http.Request, action authz.Action, err error) {
	var refused *Refused
	switch {
	case errors.Is(err, ErrNoSuchArtifact):
		notFound(w)
	case errors.As(err, &refused):
		operator.RefuseDecision(w, r, action, refused.Decision)
	case errors.Is(err, artifactsdomain.ErrTrashed):
		trashed(w)
	case errors.Is(err, ErrRecordUnwritten):
		log.WarnContext(r.Context(), "artifacts_elevated_read_unrecorded", "error", err.Error())
		unavailable(w, err, "this read is admitted only as an administrator's, and the "+
			"record of it could not be written, so it was not answered")
	default:
		// THE CHART OR THE ESTATE COULD NOT TELL: unknown, never a refusal.
		unavailable(w, err, "this node could not decide who may act on this artifact")
	}
}

// notFound is the answer for an artifact this caller is answered as absent:
// byte for byte the authority table's answer for a caller with no relation
// ([authz.EnvelopeRefusal]), so the two cannot be told apart.
func notFound(w http.ResponseWriter) {
	httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
}

// gone is a version, an asset or a file the artifact does not hold, for a
// caller the artifact itself was disclosed to.
func gone(w http.ResponseWriter, detail string) {
	httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{"detail": detail})
}

// invalid is a request the route refuses on its own rules.
func invalid(w http.ResponseWriter, detail string) {
	httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid,
		map[string]string{"detail": detail})
}

// unavailable is a 503 whose cause says whether, and when, to come back.
func unavailable(w http.ResponseWriter, cause error, detail string) {
	httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
		httpjson.RetrySeconds(statelog.RetryAfter(cause, authz.RetryUndecidedSeconds*time.Second)),
		httpjson.Detail{"detail": detail})
}

// trashed refuses a gesture on an artifact in the trash.
func trashed(w http.ResponseWriter) {
	httpjson.FailWith(w, http.StatusConflict, httpjson.CodeArtifactTrashed, map[string]string{
		"detail": "the artifact is in the trash; its owner restores it first"})
}

// prefixes are the package names a refusal's text begins with, which the
// person reading the detail has no use for.
var prefixes = []string{"artifactsdomain: ", "artifactupload: ", "artifactsapi: ", "artifactserve: ",
	"artifacts: ", "objectives: "}

// say is err's text as a detail a person reads.
func say(err error) string {
	text := err.Error()
	for _, p := range prefixes {
		text = strings.TrimPrefix(text, p)
	}
	return text
}

// written answers a write that needs no more than its outcome: applied (200),
// durable and not yet applied on this node (202), or unknown (503 carrying
// the operation to retry under). body is what the answer says beside it.
func written(w http.ResponseWriter, key string, res statelog.Result, body httpjson.Detail) {
	if body == nil {
		body = httpjson.Detail{}
	}
	body["op_id"], body["outcome"] = key, string(res.Outcome)
	switch res.Outcome {
	case statelog.OutcomeUnknown:
		operator.UnknownOutcome(w, key, res.Unvouched, body)
	case statelog.OutcomePending:
		body["position"] = res.Position
		httpjson.Write(w, http.StatusAccepted, body)
	default:
		body["position"] = res.Position
		httpjson.Write(w, http.StatusOK, body)
	}
}

// writeFailed renders a write the domain or the pipeline refused, or one whose
// outcome nobody can establish.
func writeFailed(w http.ResponseWriter, r *http.Request, key, verb string, err error) {
	var (
		gone       *artifactsdomain.CarriedObjectGone
		quota      *artifactsdomain.QuotaExceeded
		mismatch   *artifactupload.UploadMismatch
		missing    *artifactupload.UploadMissing
		pending    *artifactupload.UploadsOutstanding
		content    *artifactupload.ContentRefused
		edit       *edits.Refused
		policy     *lint.Refusal
		detail     = httpjson.Detail{"detail": say(err), "op_id": key}
		fail       = func(status int, code httpjson.Code) { httpjson.FailWithFields(w, status, code, detail) }
		moved      *artifactsdomain.VersionMoved
		ambiguous  *artifactsdomain.FolderAmbiguous
		failClient = func(code httpjson.Code) { fail(http.StatusUnprocessableEntity, code) }
	)
	if key == "" {
		// A CHECK IS NO OPERATION: it writes nothing, so there is no
		// operation a retry would reuse, and an empty id would only be noise.
		delete(detail, "op_id")
	}
	switch {
	case errors.As(err, &moved):
		// A MOVED HEAD IS ANSWERED BY THE PUBLISH ROUTES with the entry's
		// text to merge onto, `412` because they take their base as the
		// If-Match header. A gesture reaching here stated no such header,
		// so it is the class's own conflict, as a tool is told it: a 412
		// says a precondition the request stated was false, and this one
		// stated none.
		detail["head"] = moved.Head.HeadNumber
		fail(http.StatusConflict, httpjson.CodeStaleVersion)
	case errors.As(err, &gone):
		detail["paths"] = gone.Paths
		fail(http.StatusConflict, httpjson.CodeCarriedObjectGone)
	case errors.As(err, &quota):
		// 409, NOT 413: what the write collided with is how much the
		// company holds, which a smaller request does not change — the
		// tools' `quota_exceeded` class, answered in its status.
		detail["limit"], detail["usage"], detail["key"] = quota.Limit, quota.Usage, quota.Key
		fail(http.StatusConflict, httpjson.CodeQuotaExceeded)
	case errors.As(err, &mismatch):
		detail["path"] = mismatch.Path
		failClient(httpjson.CodeUploadMismatch)
	case errors.As(err, &missing):
		detail["paths"] = missing.Paths
		fail(http.StatusConflict, httpjson.CodeUploadMissing)
	case errors.As(err, &pending):
		detail["scope"], detail["limit"], detail["held"] = pending.Scope, pending.Limit, pending.Held
		detail["unit"] = "bytes"
		if pending.Uploads {
			detail["unit"] = "uploads"
		}
		detail["until"] = pending.Until.UTC()
		// WHEN THE EARLIEST HOLD LAPSES ON ITS OWN is when a retry can
		// land without anything else finishing.
		httpjson.ThrottledWith(w, httpjson.CodeUploadsOutstanding, time.Until(pending.Until), detail)
	case errors.As(err, &content):
		detail["path"] = content.Path
		failClient(httpjson.CodeContentRefused)
	case errors.As(err, &edit):
		detail["index"], detail["path"], detail["matches"] = edit.Index, edit.Path, edit.Matches
		failClient(httpjson.CodeEditRefused)
	case errors.As(err, &policy):
		detail["findings"] = policy.Findings
		failClient(httpjson.CodePolicyRefused)
	case errors.Is(err, vendor.ErrRefused):
		failClient(httpjson.CodePolicyRefused)
	case errors.Is(err, artifactupload.ErrMediaType):
		fail(http.StatusUnsupportedMediaType, httpjson.CodeMediaTypeRefused)
	case errors.Is(err, artifactupload.ErrCredential):
		failClient(httpjson.CodeCredentialRefused)
	case errors.Is(err, artifactupload.ErrCapabilityUnknown):
		detail["capabilities"] = artifacts.Capabilities
		failClient(httpjson.CodeCapabilityUnknown)
	case errors.Is(err, artifactupload.ErrContractUnknown):
		failClient(httpjson.CodeContractUnknown)
	case errors.Is(err, artifactupload.ErrUploadNotFound):
		fail(http.StatusNotFound, httpjson.CodeUploadNotFound)
	case errors.Is(err, artifactupload.ErrUploadExpired):
		fail(http.StatusGone, httpjson.CodeUploadExpired)
	case errors.Is(err, artifactsdomain.ErrUploadStale):
		fail(http.StatusConflict, httpjson.CodeUploadExpired)
	case errors.Is(err, artifactupload.ErrUploadConsumed):
		fail(http.StatusConflict, httpjson.CodeUploadConsumed)
	case errors.Is(err, artifactsdomain.ErrLabelTaken):
		fail(http.StatusConflict, httpjson.CodeLabelTaken)
	case errors.Is(err, artifactsdomain.ErrLabelsFull):
		fail(http.StatusConflict, httpjson.CodeLabelsFull)
	case errors.Is(err, artifactsdomain.ErrSourceTaken):
		fail(http.StatusConflict, httpjson.CodeSourceTaken)
	case errors.Is(err, artifactsdomain.ErrExists):
		fail(http.StatusConflict, httpjson.CodeExists)
	case errors.Is(err, artifactsdomain.ErrTrashed):
		fail(http.StatusConflict, httpjson.CodeArtifactTrashed)
	case errors.Is(err, artifactsdomain.ErrPurging):
		fail(http.StatusConflict, httpjson.CodeArtifactPurging)
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		fail(http.StatusNotFound, httpjson.CodeNotFound)
	case errors.Is(err, artifactsdomain.ErrFolderNotFound):
		fail(http.StatusNotFound, httpjson.CodeFolderNotFound)
	case errors.Is(err, artifactsdomain.ErrFolderNameTaken):
		fail(http.StatusConflict, httpjson.CodeFolderNameTaken)
	case errors.Is(err, artifactsdomain.ErrFolderCycle):
		fail(http.StatusConflict, httpjson.CodeFolderCycle)
	case errors.Is(err, artifactsdomain.ErrFolderDepth):
		fail(http.StatusConflict, httpjson.CodeFolderDepth)
	case errors.Is(err, artifactsdomain.ErrFolderLimit):
		fail(http.StatusConflict, httpjson.CodeFolderLimit)
	case errors.As(err, &ambiguous):
		candidates := make([]string, len(ambiguous.Candidates))
		for i, c := range ambiguous.Candidates {
			candidates[i] = c.ID
		}
		detail["candidates"] = candidates
		fail(http.StatusConflict, httpjson.CodeFolderAmbiguous)
	case errors.Is(err, artifactsdomain.ErrAgentCompanyShare),
		errors.Is(err, artifactsdomain.ErrAboveOwnRole):
		fail(http.StatusForbidden, httpjson.CodeForbidden)
	case errors.Is(err, artifactsdomain.ErrConflict):
		fail(http.StatusConflict, httpjson.CodeConflict)
	case errors.Is(err, artifactsdomain.ErrInvalid), errors.Is(err, artifactupload.ErrInvalid):
		failClient(httpjson.CodeInvalid)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		operator.Interrupted(w, r, key, verb, err)
	default:
		if words, ok := builtin.Condition(err); ok {
			log.WarnContext(r.Context(), "artifacts_write_unavailable", "verb", verb,
				"op_id", key, "error", err.Error())
			unavailable(w, err, "this node could not "+verb+" right now: "+words)
			return
		}
		// A FAILURE THE DOMAIN DID NOT NAME is this node's own fault, and
		// its words are the log's.
		log.ErrorContext(r.Context(), "artifacts_write_failed", "verb", verb, "op_id", key,
			"error", err.Error())
		httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeInternalError,
			httpjson.Detail{"op_id": key})
	}
}

// readFailed renders a read of the estate that could not be answered.
func readFailed(w http.ResponseWriter, r *http.Request, err error, what string) {
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		gone(w, what+" is not one this artifact keeps")
	case errors.Is(err, artifactsdomain.ErrInvalid), errors.Is(err, artifactsdomain.ErrBadCursor):
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": say(err)})
	default:
		log.WarnContext(r.Context(), "artifacts_read_unavailable", "what", what, "error", err.Error())
		unavailable(w, err, "this node could not read "+what+" right now")
	}
}
