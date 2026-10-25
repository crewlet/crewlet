package slack_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
	"github.com/crewlet/crewlet/internal/slack"
)

// slackAnswering is a Slack whose auth.test answers body, and whose bots.info
// names the app.
func slackAnswering(authTest string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth.test":
			_, _ = w.Write([]byte(authTest))
		case "/api/bots.info":
			_, _ = w.Write([]byte(`{"ok":true,"bot":{"app_id":"A0PM"}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"unknown_method"}`))
		}
	}
}

const botIdentity = `{"ok":true,"user_id":"U0PM","team_id":"T0ACME","bot_id":"B0PM",
	"user":"crewlet-pm","team":"FairSplit"}`

func slackVendor(t *testing.T) setuptest.Vendor {
	return setuptest.Vendor{
		App:          "Slack",
		Requirements: slack.Requirements("pm", nil, nil),
		Probe: func(srv *httptest.Server) setup.Probe {
			return slack.Probe(&http.Client{Transport: httpxtest.Rewrite(t, srv)})
		},
		Values: func(*httptest.Server) map[string]string { return nil },
		Accept: slackAnswering(botIdentity),
		// SLACK REFUSES INSIDE A 200, with a code, which is the shape the
		// shared status rule has nothing to read in.
		Refuse: slackAnswering(`{"ok":false,"error":"invalid_auth"}`),
		Outage: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("<html><title>Slack is down</title></html>"))
		},
	}
}

func TestTheSlackProbePassesTheSuite(t *testing.T) {
	t.Parallel()
	setuptest.Run(t, slackVendor(t))
}

func slackCheck(t *testing.T, h http.HandlerFunc) setup.Verdict {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	got, err := slack.Probe(&http.Client{Transport: httpxtest.Rewrite(t, srv)})(
		context.Background(), setup.Check{
			Field: "bot_token", Value: func(string) string { return "xoxb-typed" },
		})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A WORKING BOT TOKEN SAYS WHO AND WHERE, which is how an operator with
// several agents' apps open knows they pasted this agent's.
func TestASlackCheckNamesTheBotAndTheWorkspace(t *testing.T) {
	t.Parallel()
	got := slackCheck(t, slackAnswering(botIdentity))
	if got.Outcome != setup.VerifyWorks ||
		got.Detail != "Works. Slack accepted it as @crewlet-pm in the FairSplit workspace." {
		t.Errorf("answered %+v", got)
	}
}

// A USER TOKEN AUTHENTICATES, and an agent posting with one posts as the
// person who installed the app, so it is refused with what to copy instead.
func TestASlackUserTokenIsRefused(t *testing.T) {
	t.Parallel()
	got := slackCheck(t, slackAnswering(`{"ok":true,"user_id":"U0JANE","team_id":"T0ACME"}`))
	if got.Outcome != setup.VerifyRefused {
		t.Errorf("a user token answered %+v", got)
	}
}

// AN ERROR CODE THAT IS NOT A REFUSAL is Slack failing, and it is not sent to
// regenerate a token that works.
func TestASlackOutageCodeIsNotARefusal(t *testing.T) {
	t.Parallel()
	got := slackCheck(t, slackAnswering(`{"ok":false,"error":"internal_error"}`))
	if got.Outcome != setup.VerifyUnreachable {
		t.Errorf("internal_error answered %+v", got)
	}
	limited := slackCheck(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if limited.Outcome != setup.VerifyUnreachable {
		t.Errorf("a rate limit answered %+v", limited)
	}
}
