package iamapi

import (
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
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
// # Refused once somebody can administer this company
//
// A live code is a way to become the first person, so it exists only while
// there is no first person. Once anybody active holds a credential and
// carries people:manage, this answers `409 bootstrap_closed` and an
// administrator invites instead — which is the path that leaves a trail
// naming who did it.
func (s *Service) PostBootstrapCode(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil {
		// ABSENT RATHER THAN REFUSING, which is what a route that this
		// deployment does not serve should look like: 404 says this
		// engine does not bootstrap that way, and 503 would say it does
		// and is broken.
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	}
	held, err := s.anyAdministrator(r)
	if err != nil {
		s.unavailable(w, r, "look for an administrator", err)
		return
	}
	if held {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeBootstrapClosed,
			map[string]string{"detail": "this company already has somebody " +
				"who can administer it; invite the next person instead, " +
				"which leaves a trail naming who did it"})
		return
	}
	path, err := s.bootstrap.MintCode(r.Context())
	if err != nil {
		log.ErrorContext(r.Context(), "api_iam_bootstrap_mint_failed",
			"error", err)
		httpjson.FailWith(w, http.StatusServiceUnavailable,
			httpjson.CodeUnavailable, map[string]string{"detail": err.Error()})
		return
	}
	log.WarnContext(r.Context(), "iam_bootstrap_code_minted", "path", path,
		"detail", "a one-time founder code is live on this host")
	httpjson.Write(w, http.StatusCreated, map[string]any{
		"path": path,
		"detail": "the code is in that file, mode 0600, on this node's host. " +
			"It is never returned over HTTP and never logged. Every code " +
			"outstanding before this one was spent, so exactly one is live.",
	})
}

// anyAdministrator reports whether somebody can already administer this
// company.
//
// ACTIVE, CREDENTIALLED AND CARRYING people:manage — all three, because any
// two of them describe a person who cannot actually do it: a suspended
// administrator may not act, and one who has never enrolled a credential
// cannot sign in to try.
func (s *Service) anyAdministrator(r *http.Request) (bool, error) {
	after := ""
	for {
		page, err := s.directory.People(r.Context(), iamdomain.PeopleQuery{
			After: after, Limit: iamdomain.MaxPageSize,
			Stage: iam.StageActive,
		})
		if err != nil {
			return false, err
		}
		for _, row := range page.People {
			if !slices.Contains(row.Grants, iam.GrantPeopleManage) {
				continue
			}
			if s.hasCredential(r, row) {
				return true, nil
			}
		}
		if page.Next == "" {
			return false, nil
		}
		after = page.Next
	}
}
