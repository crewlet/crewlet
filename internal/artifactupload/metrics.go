package artifactupload

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

var log = logging.Get("artifactupload")

// Refusals are every reason a publish or an upload is refused, the closed set
// the refusals counter's `reason` attribute takes: one per answer a surface
// gives, so a dashboard can tell a client sending the wrong bytes from a
// company out of quota.
var Refusals = []string{
	"invalid", "media_type", "content", "credential", "capability_unknown", "contract_unknown",
	"edit_refused", "policy_refused", "upload_missing", "upload_mismatch", "upload_not_found",
	"upload_expired", "upload_consumed", "uploads_outstanding", "turn_quota", "quota_exceeded",
	"version_moved", "carried_object_gone", "label_taken", "labels_full", "source_taken",
	"exists", "trashed", "not_found", "conflict",
	"folder_not_found", "brand_contrast", "brand_incomplete", "other",
}

// reasonOf is a refusal's reason, from [Refusals].
func reasonOf(err error) string {
	var (
		mismatch *UploadMismatch
		missing  *UploadMissing
		pending  *UploadsOutstanding
		turn     *TurnQuota
		content  *ContentRefused
		quota    *artifactsdomain.QuotaExceeded
		moved    *artifactsdomain.VersionMoved
		gone     *artifactsdomain.CarriedObjectGone
		edit     *edits.Refused
		policy   *lint.Refusal
	)
	switch {
	case errors.As(err, &mismatch):
		return "upload_mismatch"
	case errors.As(err, &missing):
		return "upload_missing"
	case errors.As(err, &pending):
		return "uploads_outstanding"
	case errors.As(err, &turn):
		return "turn_quota"
	case errors.As(err, &content):
		return "content"
	case errors.As(err, &quota):
		return "quota_exceeded"
	case errors.As(err, &moved):
		return "version_moved"
	case errors.As(err, &gone):
		return "carried_object_gone"
	case errors.As(err, &edit):
		return "edit_refused"
	case errors.As(err, &policy):
		return "policy_refused"
	case errors.Is(err, ErrMediaType):
		return "media_type"
	case errors.Is(err, ErrCredential):
		return "credential"
	case errors.Is(err, ErrCapabilityUnknown):
		return "capability_unknown"
	case errors.Is(err, ErrContractUnknown):
		return "contract_unknown"
	case errors.Is(err, ErrUploadNotFound):
		return "upload_not_found"
	case errors.Is(err, ErrUploadExpired):
		return "upload_expired"
	case errors.Is(err, ErrUploadConsumed):
		return "upload_consumed"
	case errors.Is(err, artifactsdomain.ErrLabelTaken):
		return "label_taken"
	case errors.Is(err, artifactsdomain.ErrLabelsFull):
		return "labels_full"
	case errors.Is(err, artifactsdomain.ErrSourceTaken):
		return "source_taken"
	case errors.Is(err, artifactsdomain.ErrExists):
		return "exists"
	case errors.Is(err, artifactsdomain.ErrBrandContrast):
		return "brand_contrast"
	case errors.Is(err, artifactsdomain.ErrBrandIncomplete):
		return "brand_incomplete"
	case errors.Is(err, artifactsdomain.ErrTrashed), errors.Is(err, artifactsdomain.ErrPurged):
		return "trashed"
	case errors.Is(err, artifactsdomain.ErrNotFound):
		return "not_found"
	case errors.Is(err, artifactsdomain.ErrFolderNotFound):
		// THE FOLDER A CREATE FILES INTO is gone or not visible: a
		// definite refusal, so what the create staged is undone rather
		// than left for a retry that would be refused the same way.
		return "folder_not_found"
	case errors.Is(err, artifactsdomain.ErrConflict):
		return "conflict"
	case errors.Is(err, ErrInvalid), errors.Is(err, artifactsdomain.ErrInvalid),
		errors.Is(err, artifactsdomain.ErrUploadStale),
		errors.Is(err, artifactsdomain.ErrAgentCompanyShare), errors.Is(err, artifactsdomain.ErrAboveOwnRole):
		return "invalid"
	}
	return "other"
}

// refused reports whether err is a definite refusal: the request was judged
// and turned down, as opposed to an outcome nobody could establish (a store
// that did not answer, a context that ended). Only a refusal undoes what a
// publish staged; an unknown outcome leaves it for the retry to continue.
func refused(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return reasonOf(err) != "other"
}

// observePublish records one publish's duration and, for a refusal, its
// reason.
func (s *Service) observePublish(start time.Time, err error) {
	if s.meter == nil {
		return
	}
	outcome := "published"
	switch {
	case err == nil:
	case refused(err):
		outcome = "refused"
	default:
		outcome = "failed"
	}
	s.meter.Observe(metrics.ArtifactsPublishDuration, s.now().Sub(start), metrics.Attrs{"outcome": outcome})
	if outcome == "refused" {
		s.countRefusal(err)
	}
}

// judgePublish counts one create or publish, from its request to its answer,
// as a [objectives.PublishSuccess] event: answered when it published or was
// refused for the client's own reason (a definite refusal: a moved head, a
// quota, a policy, a bad request), and not when nobody could establish its
// outcome.
func (s *Service) judgePublish(start time.Time, err error) {
	if s.judged != nil {
		s.judged.Record(objectives.PublishSuccess, s.now().Sub(start), err == nil || refused(err))
	}
}

// judgeApplied counts one bytes-free publish ([bytesFree]), from the domain's
// decision to its record applied, as a [objectives.PublishApplied] event. A
// refusal is no latency event: it was answered, and never applied.
func (s *Service) judgeApplied(decided time.Time, err error) {
	if s.judged != nil && !refused(err) {
		s.judged.Record(objectives.PublishApplied, s.now().Sub(decided), err == nil)
	}
}

// bytesFree reports a publish that moves no new bytes into the store: every
// file it names is carried, restored or removed, or an edit of the base. Its
// latency is the log's and the applier's alone, never its content's, which is
// what the publish latency objective is held to.
func bytesFree(req PublishRequest) bool {
	if req.Upload != "" {
		return false
	}
	for _, f := range req.Files {
		if f.Upload || f.Inline != nil {
			return false
		}
	}
	return true
}

// countRefusal records one refusal by its reason.
func (s *Service) countRefusal(err error) {
	if s.meter != nil {
		s.meter.Add(metrics.ArtifactsPublishRefusals, 1, metrics.Attrs{"reason": reasonOf(err)})
	}
}

// countBytes records bytes that reached the store by one transport.
func (s *Service) countBytes(n int64, transport string) {
	if s.meter != nil && n > 0 {
		s.meter.Add(metrics.ArtifactsUploadBytes, uint64(n), metrics.Attrs{"transport": transport})
	}
}

// gauge writes one gauge.
func (s *Service) gauge(name string, v float64) {
	if s.meter != nil {
		s.meter.Set(name, v, nil)
	}
}
