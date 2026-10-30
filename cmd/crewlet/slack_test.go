package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/slack"
)

// A SLACK REPORT SAYS IT LEFT A SEAT ALONE ONLY WHEN THE RUN LEFT ITS APP AND
// ITS TOKEN.
//
// The line says the seat's credential still works and that minting a fresh
// one would revoke what the running engine is using. A run that installed the
// seat's app — with -reinstall, or after a -no-install run — had just minted
// that credential, and its report printed the line too: an operator who then
// minted afresh revoked the token the run had just recorded. Each case runs
// twice against one ledger and one env file, as two invocations would.
func TestASlackReportLeavesAloneOnlyASeatTheRunDidNotTouch(t *testing.T) {
	t.Parallel()
	install := func(context.Context, string, string) (string, error) {
		return "the-temporary-code", nil
	}
	cases := []struct {
		name string
		// first is the run before the one whose report is read, then.
		first, then func(*slack.Options)
		leftAlone   bool
	}{
		{
			name:      "a plain re-run of an installed app",
			first:     func(o *slack.Options) { o.Install = install },
			then:      func(o *slack.Options) { o.Install = install },
			leftAlone: true,
		},
		{
			name:  "a -reinstall re-run of an installed app",
			first: func(o *slack.Options) { o.Install = install },
			then:  func(o *slack.Options) { o.Install, o.Reinstall = install, true },
		},
		{
			name:  "the install run after a -no-install run",
			first: func(o *slack.Options) { o.Install = nil },
			then:  func(o *slack.Options) { o.Install = install },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			api := fakeSlackAPI(t)
			dir := t.TempDir()
			ledgerPath := filepath.Join(dir, slack.LedgerName)
			envPath := filepath.Join(dir, "slack.env")
			reconcile := func(mutate func(*slack.Options)) (*slack.Result, provision.TokenSink) {
				t.Helper()
				ledger, err := slack.LoadLedger(ledgerPath)
				if err != nil {
					t.Fatal(err)
				}
				sink, err := provision.NewEnvFileSink(envPath)
				if err != nil {
					t.Fatal(err)
				}
				opts := slack.Options{
					Admin: slack.NewAdmin(api),
					Seats: []slack.SeatPlan{{
						Handle: "swe", RoleName: "Engineer",
						TokenVar: "SLACK_BOT_TOKEN_SWE", SigningVar: "SLACK_SIGNING_SECRET_SWE",
					}},
					Ledger: ledger, LedgerPath: ledgerPath, Sink: sink,
					BaseURL: "https://engine.example.com", ConfigRefreshToken: "xoxe-1-refresh",
				}
				mutate(&opts)
				res, err := slack.Reconcile(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				return res, sink
			}
			reconcile(c.first)
			res, sink := reconcile(c.then)

			var report bytes.Buffer
			printSlackResult(&report, res, ledgerPath, sink)
			got := report.String()
			if !c.leftAlone && !strings.Contains(got, "Installed 1 app(s): swe") {
				t.Fatalf("the run under test installed nothing, so this case proves "+
					"nothing:\n%s", got)
			}
			if said := strings.Contains(got, "seat(s) alone"); said != c.leftAlone {
				t.Errorf("the report says it left the seat alone: %v, want %v:\n%s",
					said, c.leftAlone, got)
			}
		})
	}
}

// fakeSlackAPI is a client whose every request reaches a fake of the Web API
// methods a provisioning run calls, answering as the one app it creates. A
// method it does not know fails the case, so a run reaching further than
// these is seen rather than answered.
func fakeSlackAPI(t *testing.T) *http.Client {
	t.Helper()
	replies := map[string]string{
		"tooling.tokens.rotate": `{"ok":true,"token":"xoxe.xoxp-config",` +
			`"refresh_token":"xoxe-1-next","exp":4102444800}`,
		"apps.manifest.validate": `{"ok":true}`,
		"apps.manifest.create": `{"ok":true,"app_id":"A0SWE","credentials":` +
			`{"client_id":"1.2","client_secret":"cs","signing_secret":"ss"}}`,
		"oauth.v2.access": `{"ok":true,"access_token":"xoxb-minted",` +
			`"bot_user_id":"U0BOTSWE","app_id":"A0SWE","team":{"id":"T0ACME"}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		reply, known := replies[method]
		if !known {
			t.Errorf("the run called %s, which this fake does not answer", method)
			reply = `{"ok":false,"error":"unknown_method"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	return &http.Client{Timeout: 5 * time.Second, Transport: httpxtest.Rewrite(t, server)}
}
