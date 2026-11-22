package searchconsole

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
)

// AN INDEXING API NOTIFICATION IS SENT ONLY FOR A PAGE GOOGLE'S TERMS ALLOW,
// read now: an update needs JobPosting, or BroadcastEvent nested in
// VideoObject, in JSON-LD, microdata or RDFa; a removal needs a page that is
// gone or carries noindex. Every other page is refused, saying why.
//
// The page itself is judged, never where it redirects; items nest as the
// page's tree holds them, in schema.org's vocabulary alone.
//
// Mutation: accept a BroadcastEvent outside a VideoObject, a removal of a
// page that still answers 200, a followed redirect, a sibling VideoObject or
// another vocabulary's JobPosting, and a case that must be refused passes.
func TestAnIndexingNotificationIsSentOnlyForAPageGooglesTermsAllow(t *testing.T) {
	t.Parallel()
	pages := map[string]struct {
		status int
		header string
		body   string
	}{
		"/job-ld":      {200, "", `<html><head><script type="application/ld+json">{"@context":"https://schema.org","@graph":[{"@type":"Organization"},{"@type":["Thing","JobPosting"]}]}</script></head></html>`},
		"/live-ld":     {200, "", `<script type="application/ld+json">{"@type":"VideoObject","publication":[{"@type":"BroadcastEvent","isLiveBroadcast":true}]}</script>`},
		"/broadcast":   {200, "", `<script type="application/ld+json">{"@type":"BroadcastEvent"}</script>`},
		"/job-micro":   {200, "", `<div itemscope itemtype="https://schema.org/JobPosting"><span itemprop="title">Cook</span></div>`},
		"/live-rdfa":   {200, "", `<div vocab="https://schema.org/" typeof="VideoObject"><div property="publication" typeof="BroadcastEvent"></div></div>`},
		"/plain":       {200, "", `<html><body><h1>About us</h1><script type="application/ld+json">{"@type":"Article"}</script></body></html>`},
		"/gone":        {410, "", ``},
		"/noindex":     {200, "", `<meta name="robots" content="noindex, nofollow">`},
		"/noindex-hdr": {200, "googlebot: noindex", ``},
		"/other-bot":   {200, "otherbot: noindex", ``},
		"/broken":      {500, "", ``},
		"/moved":       {301, "", ``},
		"/siblings":    {200, "", `<div itemscope itemtype="https://schema.org/VideoObject"></div><div itemscope itemtype="https://schema.org/BroadcastEvent"></div>`},
		"/other-vocab": {200, "", `<div itemscope itemtype="https://vocab.example/JobPosting"></div><script type="application/ld+json">{"@type":"https://vocab.example/JobPosting"}</script>`},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := pages[r.URL.Path]
		if p.header != "" {
			w.Header().Set("X-Robots-Tag", p.header)
		}
		if p.status == http.StatusMovedPermanently {
			w.Header().Set("Location", "/job-ld")
		}
		w.WriteHeader(p.status)
		_, _ = w.Write([]byte(p.body))
	}))
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		page, notification string
		allowed            bool
	}{
		{"/job-ld", notifyUpdated, true},
		{"/live-ld", notifyUpdated, true},
		{"/broadcast", notifyUpdated, false},
		{"/job-micro", notifyUpdated, true},
		{"/live-rdfa", notifyUpdated, true},
		{"/plain", notifyUpdated, false},
		{"/broken", notifyUpdated, false},
		{"/gone", notifyDeleted, true},
		{"/noindex", notifyDeleted, true},
		{"/noindex-hdr", notifyDeleted, true},
		{"/other-bot", notifyDeleted, false},
		{"/plain", notifyDeleted, false},
		{"/moved", notifyUpdated, false},
		{"/moved", notifyDeleted, false},
		{"/siblings", notifyUpdated, false},
		{"/other-vocab", notifyUpdated, false},
	} {
		err := pageAllows(t.Context(), srv.Client(), srv.URL+tc.page, tc.notification)
		var r *growth.Refusal
		switch {
		case tc.allowed && err != nil:
			t.Errorf("%s %s was refused: %v", tc.notification, tc.page, err)
		case !tc.allowed && (err == nil || !errors.As(err, &r) || r.Kind != growth.RefusedForbidden):
			t.Errorf("%s %s answered %v, want a refusal", tc.notification, tc.page, err)
		}
	}
}
