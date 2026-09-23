package iamapi

import (
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// PostBootstrapCode is `POST /iam/bootstrap-code`.
//
// # It answers the FILE PATH and never the value
//
// The code is written 0600 beside the store, which is the one path a running
// node always has, and its SHA-256 is published so any ingress node validates
// any node's. A route that returned the value would put a superuser claim in
// a response body, a proxy log and a shell history — so the answer names
// where to read it, and reading it needs shell on the host, which is the
// point.
//
// # It names the NODE as well as the path
//
// The file is written on whichever node served this request, and on a fleet
// behind a load balancer that is not something the caller chose — so a path
// with no host is a file nobody can find.
//
// # Refused wherever the redemption would be
//
// A live code is a way to become the first person, so it exists only while
// there is no first person. The seam asks the redemption's OWN gate — nobody
// enrolled, `api.auth.bootstrap` open — and this answers `409
// bootstrap_closed` when it is shut. It used to ask its own question here,
// whether an ACTIVE, CREDENTIALLED ADMINISTRATOR existed, which is a different
// one: a company whose only person was suspended was handed a code the
// redemption refused for as long as it lived. The way back in there is an
// administrator, or a Tier A token holding people:manage — the path that
// leaves a trail naming who did it.
func (s *Service) PostBootstrapCode(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil {
		// ABSENT RATHER THAN REFUSING, which is what a route that this
		// deployment does not serve should look like: 404 says this
		// engine does not bootstrap that way, and 503 would say it does
		// and is broken.
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	}
	minted, err := s.bootstrap.MintCode(r.Context())
	switch {
	case errors.Is(err, iamdomain.ErrBootstrapClosed):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeBootstrapClosed,
			map[string]string{"detail": "the first-person route is closed " +
				"here — somebody is already enrolled, or api.auth.bootstrap " +
				"is closed — so no code would ever be honoured. An " +
				"administrator, or a Tier A token holding people:manage, " +
				"invites or enrols the next person instead"})
		return
	case errors.Is(err, statelog.ErrUnavailable):
		// THE ESTATE COULD NOT BE READ, OR A RECORD COULD NOT BE LANDED
		// OR CONFIRMED: waiting clears it, so a 503 with the hint every
		// identity 503 carries. It was a bare 503, which a client cannot
		// tell from a node that is gone for good.
		log.WarnContext(r.Context(), "api_iam_bootstrap_mint_unavailable",
			"error", err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
			auth.RetryIdentitySeconds, httpjson.Detail{"detail": err.Error()})
		return
	case errors.Is(err, statelog.ErrConflict):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
		return
	case err != nil:
		// A FAULT WAITING DOES NOT CLEAR — the file beside the store
		// could not be written, this node's randomness failed — so a 500
		// rather than a 503 telling a client to retry for ever.
		log.ErrorContext(r.Context(), "api_iam_bootstrap_mint_failed",
			"error", err)
		httpjson.FailWith(w, http.StatusInternalServerError,
			httpjson.CodeInternalError, map[string]string{"detail": err.Error()})
		return
	}
	log.WarnContext(r.Context(), "iam_bootstrap_code_minted",
		"path", minted.Path, "node", minted.Node,
		"detail", "a one-time founder code is live on this host")
	httpjson.Write(w, http.StatusCreated, map[string]any{
		"path": minted.Path,
		"node": minted.Node,
		"detail": "the code is in that file, mode 0600, on that node's host, " +
			"and lasts 24 hours. It is never returned over HTTP and never " +
			"logged. Every code outstanding before this one was withdrawn, " +
			"so exactly one is live.",
	})
}
