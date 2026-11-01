package harness

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeStatus plays a status command: FAKE_STATUS_STDOUT on stdout,
// FAKE_STATUS_STDERR on stderr, exit FAKE_EXIT, after FAKE_SLEEP_MS (which the
// fake's dispatch sleeps before anything). With FAKE_STATUS_TEXT set it prints
// that instead when argv carries `--text`: the profile's status_text_args.
func fakeStatus() {
	text := os.Getenv("FAKE_STATUS_TEXT")
	asked := false
	for _, a := range os.Args {
		asked = asked || a == "--text"
	}
	if text != "" && asked {
		fmt.Print(text)
	} else {
		fmt.Print(os.Getenv("FAKE_STATUS_STDOUT"))
	}
	fmt.Fprint(os.Stderr, os.Getenv("FAKE_STATUS_STDERR"))
	code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
	os.Exit(code)
}

// statusProvider is a provider whose status command is [fakeStatus], reading
// Claude Code's fields out of its JSON.
func statusProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	merged := map[string]string{"FAKE_STORE": "1"}
	for k, v := range env {
		merged[k] = v
	}
	run := "-test.run=TestFakeHarness"
	return fakeProvider(t, merged, map[string]any{
		"credential_paths": []any{".fake/creds.json"},
		"status_args":      []any{run, "store-status"},
		"status_text_args": []any{run, "store-status", "--text"},
		"status_fields": map[string]any{
			"method":  []any{[]any{"authMethod"}},
			"account": []any{[]any{"email"}, []any{"orgName"}},
		},
	})
}

// THE STATUS COMMAND'S EXIT IS THE ANSWER, and its JSON says how and as whom,
// in the shapes measured on `claude auth status`: signed in exits 0 with
// `authMethod` and `email`; signed out exits 1; anything else is unknown, with
// the CLI's first line on stderr as why. An account is bounded, and a status
// that runs past its budget is unknown rather than a wedged check.
//
// Mutation: read exit 1 as unknown, drop the fields, or drop the account's
// bound, and its case is red.
func TestALoginStatusIsTheHarnessExitAndItsFields(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 300) + "@example.com"
	for name, c := range map[string]struct {
		env     map[string]string
		want    LoginStatus
		account int
	}{
		"signed in": {
			env: map[string]string{"FAKE_STATUS_STDOUT": `{"loggedIn": true, "authMethod": "claude.ai", ` +
				`"apiProvider": "firstParty", "email": "ops@example.com", "orgName": "Acme"}`},
			want: LoginStatus{State: LoginSignedIn, Method: "claude.ai", Account: "ops@example.com"},
		},
		"signed in to an organisation": {
			env:  map[string]string{"FAKE_STATUS_STDOUT": `{"loggedIn": true, "authMethod": "console", "orgName": "Acme"}`},
			want: LoginStatus{State: LoginSignedIn, Method: "console", Account: "Acme"},
		},
		"signed in, as a long account": {
			env:     map[string]string{"FAKE_STATUS_STDOUT": `{"authMethod": "claude.ai", "email": "` + long + `"}`},
			want:    LoginStatus{State: LoginSignedIn, Method: "claude.ai", Account: long[:maxStatusAccount]},
			account: maxStatusAccount,
		},
		"signed out": {
			env: map[string]string{"FAKE_EXIT": "1",
				"FAKE_STATUS_STDOUT": `{"loggedIn": false, "authMethod": "none", "apiProvider": "firstParty"}`},
			want: LoginStatus{State: LoginSignedOut},
		},
		"unknown": {
			env: map[string]string{"FAKE_EXIT": "2",
				"FAKE_STATUS_STDERR": "\n\x1b[31mError: credentials file is corrupt\x1b[0m\nat line 3\n"},
			want: LoginStatus{State: LoginUnknown,
				Detail: "the status command exited 2: Error: credentials file is corrupt"},
		},
		"a text status": {
			env:  map[string]string{"FAKE_STATUS_STDOUT": "Logged in using ChatGPT\n"},
			want: LoginStatus{State: LoginSignedIn},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := statusProvider(t, c.env)
			before := time.Now().Add(-time.Second)
			got, err := p.LoginStatus(t.Context())
			if err != nil {
				t.Fatalf("LoginStatus: %v", err)
			}
			if !got.State.Valid() || got.CheckedAt.Before(before) {
				t.Errorf("the status is %q checked at %s, want a valid state checked now", got.State, got.CheckedAt)
			}
			got.CheckedAt = time.Time{}
			if got != c.want {
				t.Errorf("LoginStatus = %+v, want %+v", got, c.want)
			}
		})
	}
}

// A STATUS IS NEVER A WEDGED CHECK: one that runs past its budget is unknown
// and says so, its CLI stopped.
//
// Mutation: drop the timeout from the status command's run, and this case
// waits out the fake's sleep and is red.
func TestALoginStatusPastItsBudgetIsUnknown(t *testing.T) {
	t.Parallel()
	p := statusProvider(t, map[string]string{"FAKE_SLEEP_MS": "20000"})
	budget := 300 * time.Millisecond
	started := time.Now()
	got, err := p.loginStatus(t.Context(), budget)
	if err != nil {
		t.Fatalf("LoginStatus: %v", err)
	}
	if got.State != LoginUnknown || !strings.Contains(got.Detail, "did not answer within") {
		t.Errorf("a status past its budget = %+v, want unknown, naming the budget", got)
	}
	if took := time.Since(started); took > 15*time.Second {
		t.Errorf("the status took %s, past its budget of %s", took, budget)
	}
}

// A STATUS THAT COULD NOT BE ASKED IS AN ERROR, not an answer: a CLI with no
// status command says so rather than reading as signed out.
func TestALoginStatusWithNoCommandIsAnError(t *testing.T) {
	t.Parallel()
	p := fileProvider(t, nil)
	p.profile.StatusArgs = nil
	if _, err := p.LoginStatus(t.Context()); !errors.Is(err, ErrNoLoginCommand) {
		t.Errorf("LoginStatus with no status command = %v, want ErrNoLoginCommand", err)
	}
}

// A PERSON READS THE STATUS IN WORDS: `crewlet llm status` runs the profile's
// status_text_args where its status_args print JSON for the engine, and
// status_args themselves where there are no others.
//
// Mutation: run status_args alone in Status, and the JSON is what a person is
// shown.
func TestTheStatusAPersonReadsIsTheTextForm(t *testing.T) {
	t.Parallel()
	p := statusProvider(t, map[string]string{"FAKE_STATUS_STDOUT": `{"authMethod": "claude.ai"}`,
		"FAKE_STATUS_TEXT": "Login method: Claude Max account\n"})
	var out bytes.Buffer
	if err := p.Status(t.Context(), &out, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if out.String() != "Login method: Claude Max account\n" {
		t.Errorf("Status printed %q, want the text form", out.String())
	}
	p.profile.StatusTextArgs = nil
	out.Reset()
	if err := p.Status(t.Context(), &out, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if out.String() != `{"authMethod": "claude.ai"}` {
		t.Errorf("Status with no text form printed %q, want status_args' own output", out.String())
	}
}
