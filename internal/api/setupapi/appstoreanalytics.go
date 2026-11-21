package setupapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/plainjson"
)

// AppStoreAnalytics turns App Store analytics on with an Admin key a person
// pastes once ([appstoreconnect.Product.EnableAnalytics]).
type AppStoreAnalytics interface {
	EnableAnalytics(ctx context.Context, in appstoreconnect.AnalyticsEnable) ([]appstoreconnect.AnalyticsApp, error)
}

// analyticsRequest is what Turn on App Store analytics sends: the connection,
// and the Admin key's ID and .p8, which no answer, log or record repeats.
type analyticsRequest struct {
	Connection string `json:"connection"`
	KeyID      string `json:"key_id"`
	PrivateKey string `json:"private_key"`
}

// maxAnalyticsBody bounds the request: a .p8 key is about 250 bytes, and a
// key ID and a connection id a few more.
const maxAnalyticsBody = 4 << 10

// analyticsRevoke is what the answer tells the person to do next.
const analyticsRevoke = "Revoke this key in App Store Connect now: Crewlet used it once and kept no copy."

// appStoreAnalytics serves POST /setup/integrations/app_store_connect/analytics.
func (s *Service) appStoreAnalytics(w http.ResponseWriter, r *http.Request) {
	if s.analytics == nil {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds, httpjson.Detail{
			"detail": "this node does not reach App Store Connect; ask through a node that serves the dashboard"})
		return
	}
	p, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	if p.Seat == "" {
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeForbidden, map[string]string{
			"detail": "a person holding a human seat turns App Store analytics on, so the access log can say who"})
		return
	}
	var req analyticsRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAnalyticsBody))
	if err == nil {
		err = decode(body, &req)
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		httpjson.FailWith(w, http.StatusRequestEntityTooLarge, httpjson.CodeInvalid, map[string]string{
			"detail": "the request is larger than a key ever is"})
		return
	case err != nil:
		// NOT ECHOED: plainjson names where the JSON broke, never its values.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
			"detail": plainjson.Unreadable("the request", err)})
		return
	}
	apps, err := s.analytics.EnableAnalytics(r.Context(), appstoreconnect.AnalyticsEnable{
		Connection: strings.TrimSpace(req.Connection), KeyID: req.KeyID, PrivateKey: req.PrivateKey, By: p.Seat})
	var refusal *growth.Refusal
	switch {
	case errors.As(err, &refusal) && refusal.Kind == growth.RefusedUnavailable:
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
			httpjson.Detail{"detail": refusal.Said})
		return
	case errors.As(err, &refusal):
		status, code := http.StatusBadRequest, httpjson.CodeInvalid
		switch refusal.Kind {
		case growth.RefusedNotFound:
			status, code = http.StatusNotFound, httpjson.CodeNotFound
		case growth.RefusedForbidden:
			status, code = http.StatusForbidden, httpjson.CodeForbidden
		case growth.RefusedConflict:
			status, code = http.StatusConflict, httpjson.CodeConflict
		}
		httpjson.FailWith(w, status, code, map[string]string{"detail": refusal.Said})
		return
	case err != nil:
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds, httpjson.Detail{
			"detail": "App Store Connect could not be reached, so nothing was turned on; try again"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"connection": req.Connection, "apps": apps,
		"revoke": analyticsRevoke})
}
