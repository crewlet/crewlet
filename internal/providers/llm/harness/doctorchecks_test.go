package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// EACH CHECK SAYS WHAT KIND OF FAILURE IT MET, and the problems are the
// checks'. The API answers a test with these lines, and a client acts on the
// kind: a refused sign-in is a credential to replace, a configuration fault a
// remedy to apply. A probe whose call the vendor refused is the call's kind,
// a CLI that ran and fell short is configuration, and a missing binary is a
// call the engine cannot make at all; a check with no problem has no kind.
//
// Mutation: give a failed probe call the configuration kind, or let a passing
// check keep a kind, and this is red.
func TestEachCheckSaysWhatKindOfFailureItMet(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{
		"FAKE_STDERR": "Error: OAuth token has expired\n", "FAKE_EXIT": "1",
	}, map[string]any{
		"auth_markers": []any{map[string]any{"sentinel": "OAuth token has expired"}},
		"version_args": []any{"-test.run=NoSuchTest"},
	})
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})

	want := map[CheckName]struct {
		ok   bool
		kind string
	}{
		CheckBinary:  {ok: true},
		CheckVersion: {kind: CheckKindConfig},
		CheckSignIn:  {kind: CheckKindConfig},
		CheckUsage:   {kind: CheckKindConfig},
		CheckSmoke:   {kind: CheckKindConfig},
		CheckShell:   {ok: true},
		CheckWeb:     {kind: llm.KindAuth.String()},
	}
	var problems []string
	for _, check := range d.Checks {
		if !check.Name.Valid() {
			t.Errorf("check %q is not a name Valid accepts", check.Name)
		}
		w, ok := want[check.Name]
		if !ok {
			t.Errorf("an unexpected check %q", check.Name)
			continue
		}
		delete(want, check.Name)
		if check.OK != w.ok || check.Kind != w.kind {
			t.Errorf("%s: ok %v kind %q, want ok %v kind %q (detail %q)",
				check.Name, check.OK, check.Kind, w.ok, w.kind, check.Detail)
		}
		if check.OK != (check.Remedy == "") {
			t.Errorf("%s: ok %v with remedy %q", check.Name, check.OK, check.Remedy)
		}
		problems = append(problems, check.problems...)
	}
	for name := range want {
		t.Errorf("no %s check", name)
	}
	if strings.Join(problems, "\n") != strings.Join(d.Problems, "\n") {
		t.Errorf("Problems are not the checks' problems in check order:\n%q\nwant\n%q", d.Problems, problems)
	}
	if d.Healthy() {
		t.Error("a diagnosis with failed checks is healthy")
	}
	if CheckName("doctor").Valid() {
		t.Error("a check the doctor never makes is Valid")
	}
}

// candidateEntry is an entry on a CLI that keeps a login in .fake/creds.json
// and reads a headless token from FAKE_TOKEN, whose CLI is the fake answering
// with env.
func candidateEntry(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	return fakeProvider(t, env, map[string]any{
		"credential_paths": []any{".fake/creds.json"},
		"token_env":        "FAKE_TOKEN",
		"api_key_env":      "FAKE_KEY",
		"version_args":     []any{"-test.run=NoSuchTest"},
	})
}

// said is what the CLI answered the web probe with, as the report quotes it:
// the probe is the last call of a diagnosis, and a fake that prints what it
// was handed answers it with that.
func said(t *testing.T, d Diagnosis) string {
	t.Helper()
	web, _ := d.Check(CheckWeb)
	_, quoted, ok := strings.Cut(web.Remedy, "(it said: ")
	if !ok {
		t.Fatalf("the web check quotes no answer: %+v", web)
	}
	return quoted
}

// A CANDIDATE VALUE REACHES THE CLI, FOR THAT DIAGNOSIS ALONE. The credential
// route tests a value before it stores it, by running the doctor with it in
// place of what the entry resolved for its variable: the CLI has to be handed
// the candidate, in a home of the diagnosis's own, and the entry's next call
// has to be handed the entry's own value as before.
//
// Mutation: drop the overlay, and the CLI is handed the entry's token; build
// the candidate on the entry's own environment map, and the entry's next call
// carries the candidate.
func TestACandidateValueReachesTheHarnessForThatDiagnosisAlone(t *testing.T) {
	t.Parallel()
	p := candidateEntry(t, map[string]string{"FAKE_DUMP_ENV": "1", "GATEWAY_TOKEN": "entry-gateway"})
	p.auth.Token = "entry-token"
	home := t.TempDir()
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true, Home: home, Values: map[string]string{
		"FAKE_TOKEN":    "candidate-token",
		"GATEWAY_TOKEN": "candidate-gateway",
	}})
	handed := said(t, d)
	for _, want := range []string{
		`FAKE_TOKEN=candidate-token`, `GATEWAY_TOKEN=candidate-gateway`, `HOME=` + home,
	} {
		if !strings.Contains(handed, want) {
			t.Errorf("the CLI was not handed %s:\n%s", want, handed)
		}
	}
	if strings.Contains(handed, "entry-token") || strings.Contains(handed, "entry-gateway") {
		t.Errorf("the CLI was handed the entry's own value beside the candidate:\n%s", handed)
	}
	if d.StateDir != home {
		t.Errorf("state dir = %q, want the diagnosis's home %q", d.StateDir, home)
	}

	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if envValue(comp.Content, "FAKE_TOKEN") != "entry-token" ||
		envValue(comp.Content, "GATEWAY_TOKEN") != "entry-gateway" {
		t.Errorf("the entry's own call after the diagnosis was not handed its own values:\n%s", comp.Content)
	}
}

// A CANDIDATE THE ENTRY WOULD NOT HAND ITS CLI IS A SIGN-IN PROBLEM, never a
// value dropped in silence. A key given to a subscription entry is removed
// from the CLI's environment by the mode, and a name the entry's env does not
// set is no variable of the entry's: a test that went on without either
// would report on the entry's current sign-in as if it were the candidate's.
//
// Mutation: skip the check of what the child is handed, and the key passes
// unreported.
func TestACandidateValueThatReachesNothingIsASignInProblem(t *testing.T) {
	t.Parallel()
	p := candidateEntry(t, map[string]string{"FAKE_STDOUT": "ok"})
	d := p.Diagnose(t.Context(), DiagnoseOptions{Home: t.TempDir(), Values: map[string]string{
		"FAKE_KEY":      "sk-candidate",
		"NOT_IN_ENV":    "x",
		"FAKE_TOKEN":    "a-token",
		"FAKE_UNUSED_2": "",
	}})
	signIn, _ := d.Check(CheckSignIn)
	if signIn.OK || signIn.Kind != CheckKindConfig {
		t.Fatalf("sign-in = %+v, want a configuration failure", signIn)
	}
	for _, name := range []string{"FAKE_KEY", "NOT_IN_ENV", "FAKE_UNUSED_2"} {
		if !strings.Contains(signIn.Remedy, "candidate value for "+name+" reaches nothing") {
			t.Errorf("the sign-in check does not name %s:\n%s", name, signIn.Remedy)
		}
	}
	if strings.Contains(signIn.Remedy, "FAKE_TOKEN reaches nothing") {
		t.Errorf("the token a subscription entry reads was refused:\n%s", signIn.Remedy)
	}
}

// THE ENTRY'S REAL LOGIN IS UNTOUCHED BY A DIAGNOSIS IN A HOME OF ITS OWN: its
// calls are seeded from no login but the home's, and what the CLI writes into
// its seat comes back to the home, never to the entry's shared credentials.
// Each half has its control, the same entry diagnosed in place, which is
// seeded with the login and writes back to it, so the isolation is the home's
// and not an accident of the fake.
//
// Mutation: run the candidate in the entry's workspace, and the CLI is handed
// the real login and its write lands on it.
func TestADiagnosisInAHomeOfItsOwnLeavesTheRealLoginAlone(t *testing.T) {
	t.Parallel()
	const login = `{"token":"the-real-login"}`
	withLogin := func(t *testing.T, env map[string]string) (*Provider, string) {
		p := candidateEntry(t, env)
		path := filepath.Join(p.Workspace().CredentialsDir(), "creds.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(login), 0o600); err != nil {
			t.Fatal(err)
		}
		return p, path
	}
	read := func(t *testing.T, path string) string {
		body, err := os.ReadFile(path) //nolint:gosec // the test's own state dir
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	t.Run("seeds nothing from it", func(t *testing.T) {
		t.Parallel()
		p, _ := withLogin(t, map[string]string{"FAKE_KEY_SESSION": ".fake/creds.json"})
		d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true, Home: t.TempDir()})
		if got := said(t, d); strings.Contains(got, "the-real-login") || !strings.Contains(got, `"nothing"`) {
			t.Errorf("the CLI in the diagnosis's home held a login: %s", got)
		}
		if d.Credentials != "none on disk" {
			t.Errorf("credentials = %q, want the home's, which holds none", d.Credentials)
		}
		d = p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
		if got := said(t, d); !strings.Contains(got, "the-real-login") {
			t.Errorf("in place, the CLI was not seeded with the entry's login, so the case proves nothing: %s", got)
		}
	})

	t.Run("writes nothing back to it", func(t *testing.T) {
		t.Parallel()
		p, path := withLogin(t, map[string]string{
			"FAKE_KEY_SESSION": ".fake/creds.json", "FAKE_VENDOR_API_KEY": "refreshed",
		})
		p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true, Home: t.TempDir()})
		if got := read(t, path); got != login {
			t.Errorf("a diagnosis in another home wrote the real login: %s", got)
		}
		p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
		if got := read(t, path); got == login {
			t.Error("in place, what the CLI wrote never reached the login, so the case proves nothing")
		}
	})
}

// A DIAGNOSIS OF THE ENTRY AS IT STANDS IS RECORDED ON ITS SIGN-IN, AND ONE OF
// A CANDIDATE IS NOT. The doctor's calls are real calls, so one that the
// entry's own sign-in answered tells the entry's record the sign-in works. A
// candidate's answer says nothing about the entry's credential: recorded, it
// would clear a sign-in the vendor refuses.
//
// Mutation: run a candidate on the entry itself, and its answer lands in the
// entry's record.
func TestOnlyADiagnosisOnTheEntrysSignInIsRecordedOnIt(t *testing.T) {
	t.Parallel()
	p := candidateEntry(t, map[string]string{
		"FAKE_STDOUT": "ok", "FAKE_SHELL_PROBE_STDOUT": noLocalToolsReply, "FAKE_WEB_PROBE_STDOUT": noWebReply,
	})
	p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true, Values: map[string]string{"FAKE_TOKEN": "candidate"}})
	if at := p.Plan().State(time.Now()).LastAnswer; !at.IsZero() {
		t.Fatalf("a candidate's answer was recorded on the entry, at %v", at)
	}
	p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if p.Plan().State(time.Now()).LastAnswer.IsZero() {
		t.Error("the entry's own answered diagnosis was not recorded on its sign-in")
	}
}
