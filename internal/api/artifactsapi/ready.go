package artifactsapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objectives"
)

// THE READY REPORT: the viewer's browser saying how long one frame took from
// its ticket to the page's bridge hello, for the artifact-ready service
// objective (internal/objectives). The one indicator that sees an outage
// keeping requests from reaching the engine at all, so it is reported from the
// dashboard, through the API origin, about a frame the API minted.

// maxReadyBodyBytes bounds a ready report's body: a frame URL (an origin, a
// signed ticket of at most a few hundred bytes and the entry's path) and two
// short fields fit in a tenth of it, and nothing a report carries needs more.
const maxReadyBodyBytes = 8 << 10

// readyBody is one ready report.
type readyBody struct {
	// FrameURL is the frame's URL exactly as `POST /artifacts/{id}/tickets`
	// answered it: the frame the report is about, and the ticket that binds
	// it to the viewer and the credential it was minted for.
	FrameURL string `json:"frame_url"`

	// Outcome is `ready` (the page said hello) or `failed` (the frame
	// failed to load, or the dashboard gave up waiting for hello).
	Outcome objectives.ReadyOutcome `json:"outcome"`

	// ElapsedMS is the milliseconds from the frame being handed to the
	// dashboard to its outcome, by the browser's own clock.
	ElapsedMS int64 `json:"elapsed_ms"`
}

// reportReady counts one frame's ready time (`POST /artifacts/ready`), decided
// by `artifacts.ready` (everybody's: it carries no artifact content, and any
// reader of an artifact sends one). Answered 204 once counted.
//
// BOUND TO A FRAME THIS DEPLOYMENT MINTED FOR THIS CREDENTIAL: the URL's
// ticket is verified as the artifact origin verifies it and must name the
// caller's record and the credential the caller acts through, so no caller
// reports about a frame somebody else opened. Each frame reports once, never a
// time longer than the frame has existed, and a credential is held to a
// sustained report a second ([objectives.Beacon]).
func (s *Service) reportReady(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionArtifactReady)
	if !ok {
		return
	}
	var body readyBody
	if !decodeWithin(w, r, &body, false, maxReadyBodyBytes) {
		return
	}
	switch {
	case !body.Outcome.Valid():
		invalid(w, "outcome is ready or failed")
		return
	case body.ElapsedMS < 0 || body.ElapsedMS > artifacts.TicketLifetime.Milliseconds():
		invalid(w, "elapsed_ms is the milliseconds from the frame to its outcome, at most "+
			"the frame's own lifetime")
		return
	case s.frames == nil:
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeArtifactOriginUnset, map[string]string{
			"detail": "api.artifacts.external_url is unset on this deployment, so it opens no " +
				"frame to report on"})
		return
	}
	claims, err := s.frames.Claims(body.FrameURL)
	if err != nil {
		invalid(w, "frame_url is not a live frame this deployment minted")
		return
	}
	lineage, err := auth.LineageOf(p)
	if err != nil || claims.Principal != iam.RecordOwner(p) || claims.Lineage != lineage {
		// THE SAME ANSWER as a URL that is no frame at all: whether a frame
		// minted for somebody else exists is not this caller's to learn.
		invalid(w, "frame_url is not a live frame this deployment minted")
		return
	}
	if claims.Purpose == artifacts.TicketPurposePreview {
		// A PREVIEW IS NOT AN OPEN: a gallery card drawing a page small is
		// not a reader waiting on it, and counting it would move the ready
		// objective by what scrolled past.
		invalid(w, "frame_url is a preview's frame, which reports no ready time")
		return
	}
	err = s.ready.Accept(objectives.ReadyReport{
		Viewer: claims.Lineage, Frame: claims.Nonce,
		// THE EARLIEST INSTANT THE FRAME CAN HAVE BEEN MINTED: a ticket
		// lives at most [artifacts.TicketLifetime] (less when its credential
		// ends first), so its expiry less that lifetime is never after its
		// mint, and no honest report is longer than the time since.
		Minted: claims.Expires.Add(-artifacts.TicketLifetime), Expires: claims.Expires,
		Outcome: body.Outcome, Elapsed: time.Duration(body.ElapsedMS) * time.Millisecond,
	})
	var throttled *objectives.Throttled
	switch {
	case errors.As(err, &throttled):
		httpjson.Throttled(w, throttled.RetryAfter)
	case errors.Is(err, objectives.ErrReported):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, map[string]string{
			"detail": "this frame's ready time was already reported; each frame reports once"})
	case errors.Is(err, objectives.ErrImplausible):
		invalid(w, say(err))
	case err != nil:
		log.ErrorContext(r.Context(), "artifacts_ready_uncounted", "error", err.Error())
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
