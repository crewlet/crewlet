package webhooks_test

import (
	"errors"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
)

// THE TWO LANDING PAGES RUN ONLY THEIR OWN INLINE BLOCKS.
//
// Both are unauthenticated, both render values an arriving browser supplies,
// and both share an origin with the dashboard, whose operator token sits in
// localStorage. Each is served under a policy that allows its own style and
// script by hash and nothing else, and nobody may frame it.
//
// What is checked is a REAL response against its REAL header: every inline
// block a browser receives on every arrival has its hash in the policy, and the
// policy holds no hash that no arrival renders. That is what fails when a block
// changes without its hash, when a block starts depending on the request, or
// when a policy is computed from something other than what is served.
func TestTheLandingPagesAllowExactlyTheirOwnInlineBlocks(t *testing.T) {
	t.Parallel()
	for _, page := range []struct {
		name     string
		arrivals map[string]*httptest.ResponseRecorder
	}{
		{"github-app", map[string]*httptest.ResponseRecorder{
			"a created app with an install link": landing(t, &stubFlow{
				seat: "sre-lead", install: "https://github.com/apps/acme-sre-lead/installations/new",
			}, "code=abc&state=xyz"),
			"the install arrival":       landing(t, &stubFlow{}, "installed=sre-lead&installation_id=42"),
			"a refused creation":        landing(t, &stubFlow{seat: "sre-lead", err: errors.New("refused")}, "code=abc&state=xyz"),
			"GitHub's own refusal":      landing(t, &stubFlow{}, "error=access_denied&error_description=No"),
			"a creation with no code":   landing(t, &stubFlow{}, "state=xyz"),
			"an engine with no flow":    landingOn(t, newEdge(t), "code=abc&state=xyz"),
			"hostile markup in a query": landing(t, &stubFlow{}, "installed=<script>alert(1)</script>"),
		}},
		{"slack-oauth", map[string]*httptest.ResponseRecorder{
			"the CLI flow":      getLanding(t, newEdge(t), "code=abc&state=ceo"),
			"the manual flow":   getLanding(t, newEdge(t), "code=abc"),
			"Slack's refusal":   getLanding(t, newEdge(t), "error=access_denied"),
			"a direct visit":    getLanding(t, newEdge(t), ""),
			"hostile markup":    getLanding(t, newEdge(t), "code=<style>*{}</style>"),
			"hostile in state":  getLanding(t, newEdge(t), "code=ok&state=<script>x()</script>"),
			"hostile in error":  getLanding(t, newEdge(t), "error=</code><script>x()</script>"),
			"an empty code key": getLanding(t, newEdge(t), "code="),
		}},
	} {
		t.Run(page.name, func(t *testing.T) {
			t.Parallel()
			var policy string
			rendered := map[string][]string{"style-src": nil, "script-src": nil}
			for name, res := range page.arrivals {
				header := res.Header()
				for key, want := range map[string]string{
					"X-Frame-Options":        "DENY",
					"X-Content-Type-Options": "nosniff",
					"Referrer-Policy":        "no-referrer",
				} {
					if got := header.Get(key); got != want {
						t.Errorf("%s: %s = %q, want %q", name, key, got, want)
					}
				}
				got := header.Get("Content-Security-Policy")
				if policy == "" {
					policy = got
				} else if got != policy {
					t.Errorf("%s: the policy differs between arrivals:\n%s\n%s", name, got, policy)
				}
				styles, scripts := pagepolicy.InlineBlocks(res.Body.Bytes())
				for _, block := range styles {
					rendered["style-src"] = append(rendered["style-src"], pagepolicy.Hash(block))
				}
				for _, block := range scripts {
					rendered["script-src"] = append(rendered["script-src"], pagepolicy.Hash(block))
				}
			}

			directives := parsePolicy(policy)
			for key, want := range map[string]string{
				"default-src":     "'none'",
				"img-src":         "'self'",
				"base-uri":        "'none'",
				"form-action":     "'none'",
				"frame-ancestors": "'none'",
			} {
				if got := strings.Join(directives[key], " "); got != want {
					t.Errorf("%s = %q, want %q (policy: %s)", key, got, want, policy)
				}
			}
			for _, key := range []string{"style-src", "script-src"} {
				want := slices.Sorted(maps.Keys(set(rendered[key])))
				if len(want) == 0 {
					want = []string{"'none'"}
				}
				got := slices.Sorted(slices.Values(directives[key]))
				if !slices.Equal(got, want) {
					t.Errorf("%s allows %v, but the page renders %v", key, got, want)
				}
			}
		})
	}
}

// parsePolicy splits a Content-Security-Policy into its directives.
func parsePolicy(policy string) map[string][]string {
	out := map[string][]string{}
	for _, part := range strings.Split(policy, ";") {
		fields := strings.Fields(part)
		if len(fields) > 0 {
			out[fields[0]] = fields[1:]
		}
	}
	return out
}

func set(values []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		out[v] = true
	}
	return out
}
