package cliagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// The cases here hold the WRITE-BACK RULE: a home's login is carried back to
// the shared directory only when its CLI changed it, judged against what the
// home was seeded from — and, for anything that only refreshes a login, only
// while the shared directory still holds exactly that. Every case that matters
// is two writers on one login, which is why each one interleaves a second.

// storeWorkspace is a workspace whose profile keeps a login BOTH ways — a
// credential file and a credential store's document — because the write-back
// rule is one rule over both, and each case below checks it for both at once.
func storeWorkspace(t *testing.T) *Workspace {
	t.Helper()
	profile := testProfile(t)
	profile.CredentialStore = &cliprofile.CredentialStore{
		ExportArgs: []string{"export"}, ImportArgs: []string{"import"},
	}
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "fake-store", profile)
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}
	return ws
}

// writeLogin puts both kinds of shared login in place.
func writeLogin(t *testing.T, ws *Workspace, file, document string) {
	t.Helper()
	if err := os.MkdirAll(ws.CredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(ws.CredentialsDir(), "creds.json"): file,
		ws.documentPath(): document,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// hasLogin is [Workspace.HasLogin], a login that cannot be read failing the
// case.
func hasLogin(t *testing.T, ws *Workspace) bool {
	t.Helper()
	present, err := ws.HasLogin(t.Context())
	if err != nil {
		t.Fatalf("HasLogin: %v", err)
	}
	return present
}

// loginOnDisk lists what the shared directory holds of a login RIGHT NOW,
// read without its lock, as absolute paths: every login file present, and a
// write's journal standing beside them — what a case that holds the lock
// itself sees, and what an operator clearing the directory by hand removes.
func loginOnDisk(t *testing.T, ws *Workspace) []string {
	t.Helper()
	paths := make([]string, 0, len(ws.loginNames())+1)
	for _, name := range ws.loginNames() {
		paths = append(paths, ws.sharedPath(name))
	}
	paths = append(paths, hostbox.CredentialJournal(ws.CredentialsDir()))
	var out []string
	for _, path := range paths {
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			out = append(out, path)
		case !os.IsNotExist(err):
			t.Fatal(err)
		}
	}
	return out
}

// sharedLogin reads both kinds of shared login, "" for one that is absent.
func sharedLogin(t *testing.T, ws *Workspace) (file, document string) {
	t.Helper()
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	return read(filepath.Join(ws.CredentialsDir(), "creds.json")), read(ws.documentPath())
}

// refreshFile is a seat's CLI rewriting its login file in place.
func refreshFile(t *testing.T, co *Checkout, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(co.Home, ".fake", "creds.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// AN IDLE SEAT NEVER REVERTS ANOTHER SEAT'S REFRESH. Two seats seeded from one
// login; one refreshes it and writes the new one back; the other did nothing,
// so its file is still the seed and its export reproduces its import byte for
// byte. Compared with the shared login rather than with its seed, the idle
// seat's copy DIFFERED — and writing it back put the refresh token the vendor
// had just rotated away in front of every seat, which logged the fleet out at
// the next expiry.
func TestAnIdleSeatNeverRevertsAnotherSeatsRefresh(t *testing.T) {
	t.Parallel()
	ws := storeWorkspace(t)
	writeLogin(t, ws, `{"token":"R0"}`, `[{"id":"c","refresh":"R0"}]`)

	storeA, storeB := &stubStore{}, &stubStore{}
	a, err := ws.Acquire(t.Context(), "seat-a", "call", storeA)
	if err != nil {
		t.Fatalf("Acquire a: %v", err)
	}
	b, err := ws.Acquire(t.Context(), "seat-b", "call", storeB)
	if err != nil {
		t.Fatalf("Acquire b: %v", err)
	}

	refreshFile(t, a, `{"token":"R1"}`)
	storeA.exported = []byte(`[{"id":"c","refresh":"R1"}]`)
	if err := a.Release(t.Context()); err != nil {
		t.Fatalf("Release a: %v", err)
	}
	if file, doc := sharedLogin(t, ws); file != `{"token":"R1"}` || doc != `[{"id":"c","refresh":"R1"}]` {
		t.Fatalf("seat a's refresh did not land: file=%q doc=%q", file, doc)
	}

	storeB.exported = storeB.held
	if err := b.Release(t.Context()); err != nil {
		t.Fatalf("Release b: %v", err)
	}
	if file, doc := sharedLogin(t, ws); file != `{"token":"R1"}` || doc != `[{"id":"c","refresh":"R1"}]` {
		t.Errorf("an idle seat reverted another seat's refresh: file=%q doc=%q", file, doc)
	}
}

// A REFRESH IS A COMPARE-AND-SWAP. A seat that did refresh, while somebody else
// wrote a newer login, keeps the newer one: two seats refreshing one token is
// the vendor's race to settle, and the one that wrote first holds the token
// the vendor still honours.
func TestARefreshNeverWritesOverALoginThatMovedPastItsSeed(t *testing.T) {
	t.Parallel()
	ws := storeWorkspace(t)
	writeLogin(t, ws, `{"token":"R0"}`, `[{"id":"c","refresh":"R0"}]`)
	store := &stubStore{}
	co, err := ws.Acquire(t.Context(), "dev", "call", store)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Another writer lands first...
	writeLogin(t, ws, `{"token":"OTHER"}`, `[{"id":"c","refresh":"OTHER"}]`)
	// ...and this seat's CLI refreshed too.
	refreshFile(t, co, `{"token":"MINE"}`)
	store.exported = []byte(`[{"id":"c","refresh":"MINE"}]`)
	if err := co.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if file, doc := sharedLogin(t, ws); file != `{"token":"OTHER"}` || doc != `[{"id":"c","refresh":"OTHER"}]` {
		t.Errorf("a refresh was written over a login that had moved past its seed: file=%q doc=%q", file, doc)
	}
}

// AN OPERATOR'S NEW LOGIN SURVIVES A SEAT THAT WAS MID-CALL. `crewlet llm login`
// is the recovery for a dead login, and it runs while the engine has seats
// mid-call holding the old one: a seat's release that wrote its copy back put
// the dead login over the new one, with nothing to say so — whether the seat
// had refreshed or not.
func TestAnOperatorsLoginMidCallSurvivesTheSeatsRelease(t *testing.T) {
	t.Parallel()
	t.Run("credential store", func(t *testing.T) {
		t.Parallel()
		p := storeProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-operator"}, nil)
		writeDocument(t, p, `[{"id":"cred_1","key":"k-old"}]`+"\n")
		seat := &stubStore{}
		co, err := p.ws.Acquire(t.Context(), "dev", "call", seat)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		var errOut bytes.Buffer
		if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &errOut); err != nil {
			t.Fatalf("BrokerLogin: %v (%s)", err, errOut.String())
		}
		seat.exported = []byte(`[{"id":"cred_1","key":"k-refreshed"}]` + "\n")
		if err := co.Release(t.Context()); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if got := readDocument(t, p); !strings.Contains(got, "k-operator") || strings.Contains(got, "k-refreshed") {
			t.Errorf("a seat's release replaced the operator's new login: %q", got)
		}
	})
	t.Run("credential file", func(t *testing.T) {
		t.Parallel()
		p := fileProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-operator"})
		writeCreds(t, p, `{"token":"old"}`)
		co, err := p.ws.Acquire(t.Context(), "dev", "call", p)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		var errOut bytes.Buffer
		if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &errOut); err != nil {
			t.Fatalf("BrokerLogin: %v (%s)", err, errOut.String())
		}
		refreshFile(t, co, `{"token":"refreshed"}`)
		if err := co.Release(t.Context()); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if got := readCreds(t, p); got != `{"token":"k-operator"}` {
			t.Errorf("a seat's release replaced the operator's new login: %q", got)
		}
	})
}

// A STATUS NEVER REVERTS A REFRESH THAT LANDED WHILE IT RAN. `crewlet llm
// status` runs the CLI in a home seeded from the shared login and carries out
// what it holds afterwards; its CLI changed nothing, and comparing its export
// with the shared login — which a seat refreshed meanwhile — wrote the old
// login back over the new one.
func TestAStatusNeverRevertsARefreshThatLandedWhileItRan(t *testing.T) {
	t.Parallel()
	seeded := `[{"id":"cred_1","key":"k-old"}]` + "\n"
	refreshed := `[{"id":"cred_1","key":"k-refreshed"}]` + "\n"
	p := storeProvider(t, nil, nil)
	writeDocument(t, p, seeded)
	p.env = fakeChildEnv(map[string]string{
		"FAKE_STORE":         "1",
		"FAKE_MIDRUN_VERB":   "store-list",
		"FAKE_MIDRUN_WRITES": midrun(t, map[string]string{p.ws.documentPath(): refreshed}),
	})
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := readDocument(t, p); got != refreshed {
		t.Errorf("status wrote the login it started from back over a refresh: %q", got)
	}
}

// A LOGIN THAT CHANGED NOTHING WRITES NOTHING, even though a login command's
// result is written over whatever the shared directory holds: an operator who
// backed out of the sign-in leaves the home holding the login it was seeded
// with, and writing THAT over a refresh a seat made while the browser flow sat
// open reverted the refresh. For a login command the generation's baseline is
// the only guard — nothing compares what the shared directory holds — so each
// kind of login is held to it on its own.
//
// Mutation: judge a changed credential file, or a changed document, against
// the shared directory as it is now rather than against the baseline, and its
// case is red.
func TestALoginThatChangedNothingNeverRevertsARefresh(t *testing.T) {
	t.Parallel()
	run := "-test.run=TestCLIAgentFakeCLI"
	t.Run("credential store", func(t *testing.T) {
		t.Parallel()
		seeded := `[{"id":"cred_1","key":"k-old"}]` + "\n"
		refreshed := `[{"id":"cred_1","key":"k-refreshed"}]` + "\n"
		p := storeProvider(t, nil, map[string]any{"login_args": []any{run, "store-list"}})
		writeDocument(t, p, seeded)
		p.env = fakeChildEnv(map[string]string{
			"FAKE_STORE":         "1",
			"FAKE_MIDRUN_VERB":   "store-list",
			"FAKE_MIDRUN_WRITES": midrun(t, map[string]string{p.ws.documentPath(): refreshed}),
		})
		if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatalf("BrokerLogin: %v", err)
		}
		if got := readDocument(t, p); got != refreshed {
			t.Errorf("a login that changed nothing wrote the old login back over a refresh: %q", got)
		}
	})
	t.Run("credential file", func(t *testing.T) {
		t.Parallel()
		seeded := `{"token":"seeded"}`
		refreshed := `{"token":"refreshed"}`
		p := fakeProvider(t, nil, map[string]any{
			"credential_paths": []any{".fake/creds.json"},
			"volatile_paths":   []any{".local/share/fake"},
			"login_args":       []any{run, "store-list"},
		})
		writeCreds(t, p, seeded)
		p.env = fakeChildEnv(map[string]string{
			"FAKE_STORE":       "1",
			"FAKE_MIDRUN_VERB": "store-list",
			"FAKE_MIDRUN_WRITES": midrun(t, map[string]string{
				filepath.Join(p.ws.CredentialsDir(), "creds.json"): refreshed,
			}),
		})
		if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatalf("BrokerLogin: %v", err)
		}
		if got := readCreds(t, p); got != refreshed {
			t.Errorf("a login that changed nothing wrote the old login back over a refresh: %q", got)
		}
	})
}

// A LOGIN THAT STORED NOTHING IS A FAILURE. The CLI exited 0 with no login
// anywhere this entry reads one — an operator who backed out of the provider
// picker, a second command that pruned the database under the flow — and the
// command printed "Login written to …" about a directory holding nothing.
func TestALoginThatStoredNothingFails(t *testing.T) {
	t.Parallel()
	run := "-test.run=TestCLIAgentFakeCLI"
	broker := func(ctx context.Context, p *Provider) error {
		return p.BrokerLogin(ctx, nil, &bytes.Buffer{}, &bytes.Buffer{})
	}
	for name, c := range map[string]struct {
		p     *Provider
		login func(context.Context, *Provider) error
	}{
		"credential store": {
			storeProvider(t, nil, map[string]any{"login_args": []any{run, "store-list"}}), broker,
		},
		"credential file": {fakeProvider(t, map[string]string{"FAKE_STORE": "1"}, map[string]any{
			"credential_paths": []any{".fake/creds.json"},
			"login_args":       []any{run, "store-list"},
		}), broker},
		"credential login": {
			fakeProvider(t, map[string]string{"FAKE_STORE": "1"}, map[string]any{
				"credential_paths": []any{".fake/creds.json"},
				"stdin_login": map[string]any{
					"args": []any{run, "store-list"}, "stdin_template": "{password}\n",
				},
			}),
			func(ctx context.Context, p *Provider) error {
				return p.CredentialLogin(ctx, "ops@example.com", "example-password",
					&bytes.Buffer{}, &bytes.Buffer{})
			},
		},
	} {
		p := c.p
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := c.login(t.Context(), p)
			if err == nil {
				t.Fatal("a login command that stored nothing was reported as a login")
			}
			if !strings.Contains(err.Error(), "without leaving a login") {
				t.Errorf("the failure does not say what happened: %v", err)
			}
			if hasLogin(t, p.ws) {
				t.Errorf("a login that stored nothing left %v", loginOnDisk(t, p.ws))
			}
		})
	}
}

// LOGIN, LOGOUT, STATUS LEAVES NO LOGIN — on a CLI that keeps its login in a
// file and has a status command but no logout of its own, which is hermes's
// shape. The login home kept the login's file after the login; logout removed
// only the shared one; and status carried the login home's copy back, so the
// login an operator had just revoked was what every seat was seeded with next.
func TestLoginThenLogoutThenStatusLeavesNoLogin(t *testing.T) {
	t.Parallel()
	p := fileProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-login"})
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("BrokerLogin: %v", err)
	}
	if !hasLogin(t, p.ws) {
		t.Fatal("the login wrote nothing")
	}
	if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if hasLogin(t, p.ws) {
		t.Errorf("status brought back the login logout removed: %v", loginOnDisk(t, p.ws))
	}
}

// THE LOGIN HOME KEEPS NO SECOND COPY of a login once it is carried out: the
// shared directory is the login of record, and a copy left behind is one a
// later command finds and carries back.
func TestTheLoginHomeKeepsNoCopyOfTheLogin(t *testing.T) {
	t.Parallel()
	p := fileProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-login"})
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("BrokerLogin: %v", err)
	}
	copyLeft := filepath.Join(p.ws.loginHome(), ".fake", "creds.json")
	if _, err := os.Stat(copyLeft); !os.IsNotExist(err) {
		t.Errorf("the login home kept a copy of the login (%v)", err)
	}
}

// A STATUS NEVER CREATES A LOGIN. Only a command that makes one may; one that
// asks who the entry is signed in as, run while nobody is, leaves nobody
// signed in whatever its CLI wrote.
func TestAStatusNeverCreatesALogin(t *testing.T) {
	t.Parallel()
	run := "-test.run=TestCLIAgentFakeCLI"
	p := fakeProvider(t, map[string]string{"FAKE_STORE": "1", "FAKE_LOGIN_KEY": "k-minted"}, map[string]any{
		"credential_paths": []any{".fake/creds.json"},
		"status_args":      []any{run, "store-file-login"},
	})
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if hasLogin(t, p.ws) {
		t.Errorf("a status created a login: %v", loginOnDisk(t, p.ws))
	}
}

// A SEAT'S HOME KEEPS NO LOGIN THE SHARED DIRECTORY NO LONGER HOLDS. A
// credential file is not volatile, so a home keeps an earlier generation's
// copy — and a seed that only skipped the missing shared file left the CLI
// signed in with it, still calling as a login `crewlet llm logout` removed.
func TestASeatsHomeKeepsNoLoginTheSharedDirectoryNoLongerHolds(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	if err := os.MkdirAll(ws.CredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(ws.CredentialsDir(), "creds.json")
	if err := os.WriteFile(shared, []byte(`{"token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := ws.Acquire(t.Context(), "dev", "one", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := os.Remove(shared); err != nil {
		t.Fatal(err)
	}
	second, err := ws.Acquire(t.Context(), "dev", "two", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = second.Release(t.Context()) }()
	if _, err := os.Stat(filepath.Join(second.Home, ".fake", "creds.json")); !os.IsNotExist(err) {
		t.Errorf("the seat started signed in with a login the shared directory no longer holds (%v)", err)
	}
}

// A FAILED EXPORT KEEPS THE LOGIN, and the next generation carries it out. The
// call refreshed the login inside the CLI's database — rotating the refresh
// token — and the export after it failed: pruning the database then deleted
// the only copy of the token the vendor still honours.
func TestALoginWhoseExportFailedIsCarriedOutByTheNextGeneration(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "export-failed-once")
	p := storeProvider(t, map[string]string{
		"FAKE_STORE_REFRESH": "k-new", "FAKE_STORE_EXPORT_FAIL_ONCE": marker,
	}, nil)
	writeDocument(t, p, `[{"id":"cred_1","key":"k-old"}]`+"\n")
	if _, err := ask(t, p, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := readDocument(t, p); strings.Contains(got, "k-new") {
		t.Fatalf("an export that failed was written: %q", got)
	}
	data := seatDatabase(t, p)
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("the database holding the refreshed login was pruned after its export failed: %v", err)
	}

	// The next call refreshes nothing; it starts from the login the last one
	// left, carried out first.
	p.env = fakeChildEnv(map[string]string{"FAKE_STORE": "1"})
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "key=k-new" {
		t.Errorf("the next call held %q, want the login the last one refreshed", comp.Content)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-new") {
		t.Errorf("the shared login is %q, want the refreshed one carried out", got)
	}
}

// A SEAT WHOSE OLD LOGIN STILL CANNOT BE CARRIED OUT DOES NOT RUN — not on the
// stale database, and not by discarding it — and says so in a kind the
// fallback chain moves past: the login is fine, the CLI did not answer.
func TestASeatWhoseLoginCannotBeCarriedOutIsRefusedRetryably(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{"FAKE_STORE_REFRESH": "k-new", "FAKE_STORE_EXPORT_FAIL": "1"}, nil)
	writeDocument(t, p, `[{"id":"cred_1","key":"k-old"}]`+"\n")
	if _, err := ask(t, p, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_, err := ask(t, p, llm.Request{})
	if err == nil {
		t.Fatal("a seat ran on top of a login it could not carry out")
	}
	if kind := llm.KindOf(err); kind != llm.KindServer {
		t.Errorf("kind = %v, want server — retryable, and no fault of the credential: %v", kind, err)
	}
	if strings.Contains(err.Error(), "k-new") {
		t.Errorf("the refusal repeats what the export printed: %v", err)
	}
	data := seatDatabase(t, p)
	if _, statErr := os.Stat(data); statErr != nil {
		t.Errorf("the database was discarded although it was never carried out: %v", statErr)
	}
}

// A GENERATION OUTLIVES ITS PROCESS. A seat whose engine stopped mid-call — a
// deploy, an OOM kill — left a home holding whatever its CLI refreshed and no
// release to carry it out; the next process's first call into the seat
// carries it out before it prunes.
func TestAGenerationLeftOpenByAnEarlierProcessIsCarriedOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profile := testProfile(t)
	open := func() *Workspace {
		ws, err := Shared(dir, "fake-cli", profile)
		if err != nil {
			t.Fatalf("Shared: %v", err)
		}
		return ws
	}
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws := open()
	writeCreds(t, &Provider{ws: ws}, `{"token":"R0"}`)
	co, err := ws.Acquire(t.Context(), "dev", "call", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	refreshFile(t, co, `{"token":"R1"}`)

	// The process ends here, with no release; the next one starts afresh.
	// What its ending does to the seat's generation lock — the kernel drops
	// an flock with the process that held it — is done here by hand.
	endProcess(co)
	forgetWorkspace(dir)
	next, err := open().Acquire(t.Context(), "dev", "call-after", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = next.Release(t.Context()) }()
	if got := readCreds(t, &Provider{ws: ws}); got != `{"token":"R1"}` {
		t.Errorf("the shared login is %q, want the refresh the earlier process never carried out", got)
	}
}

// A LOGIN WHOSE EXPORT FAILED IS KEPT IN THE LOGIN HOME, and the next command
// carries it out: the operator finished the browser flow, and pruning the
// database after an export that failed made them do it again.
func TestALoginWhoseExportFailedIsCarriedOutByTheNextCommand(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "export-failed-once")
	p := storeProvider(t, map[string]string{
		"FAKE_LOGIN_KEY": "k-login", "FAKE_STORE_EXPORT_FAIL_ONCE": marker,
	}, nil)
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("a login whose export failed was reported as written")
	}
	if got := readDocument(t, p); got != "" {
		t.Fatalf("an export that failed was written: %q", got)
	}
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-login") {
		t.Errorf("the shared login is %q, want the login the failed export left", got)
	}
}

// A LOGOUT DISCARDS A LOGIN ITS COMMAND NEVER CARRIED OUT. A login whose
// export failed is kept in the login home for the next command — and the next
// command after a logout must not be the one that writes it back.
func TestALogoutDiscardsALoginNoCommandCarriedOut(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "export-failed-once")
	p := storeProvider(t, map[string]string{
		"FAKE_LOGIN_KEY": "k-login", "FAKE_STORE_EXPORT_FAIL_ONCE": marker,
	}, nil)
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("a login whose export failed was reported as written")
	}
	if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := readDocument(t, p); got != "" {
		t.Errorf("the command after a logout wrote back the login it had discarded: %q", got)
	}
}

// ONE COMMAND AT A TIME IN THE LOGIN HOME, and a second is told so rather than
// pruning the database a sign-in in progress is about to store its login in.
func TestASecondCommandIsRefusedTheLoginHomeWhileOneHoldsIt(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, nil, nil)
	unlock, err := p.ws.lockLoginHome(t.Context())
	if err != nil {
		t.Fatalf("lockLoginHome: %v", err)
	}
	defer unlock()
	err = p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{})
	var busy *loginHomeBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("Status = %v, want the login home refused as busy", err)
	}
	if !strings.Contains(err.Error(), "wait for it to finish") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
}

// THE SHARED LOGIN'S LOCK EXCLUDES ANOTHER PROCESS, which is the half a mutex
// cannot reach: every `crewlet llm` command writes the same directory. An open
// file description is what an flock belongs to, so a second description in
// this process stands in for the other process exactly.
func TestTheSharedLoginsLockExcludesAnotherHolder(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	release, err := hostbox.LockFile(t.Context(), ws.sharedLock(), 0, "the test")
	if err != nil {
		t.Fatalf("LockFile = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if unlock, err := ws.lockShared(ctx); err == nil {
		unlock()
		t.Fatal("the shared login's lock was taken while another holder had it")
	}
	release()
	unlock, err := ws.lockShared(t.Context())
	if err != nil {
		t.Fatalf("the lock was not free once its holder let go: %v", err)
	}
	unlock()
}

// fileProvider is a provider whose CLI keeps its login in ONE FILE, with a
// login command that writes it, a status command that changes nothing, and no
// logout of its own.
func fileProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	run := "-test.run=TestCLIAgentFakeCLI"
	merged := map[string]string{"FAKE_STORE": "1"}
	for k, v := range env {
		merged[k] = v
	}
	return fakeProvider(t, merged, map[string]any{
		"credential_paths": []any{".fake/creds.json"},
		"volatile_paths":   []any{".local/share/fake"},
		"login_args":       []any{run, "store-file-login"},
		"status_args":      []any{run, "store-list"},
	})
}

// writeCreds puts a login file in the shared credential directory.
func writeCreds(t *testing.T, p *Provider, body string) {
	t.Helper()
	if err := os.MkdirAll(p.ws.CredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.ws.CredentialsDir(), "creds.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readCreds is the shared login file, or "" when there is none.
func readCreds(t *testing.T, p *Provider) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(p.ws.CredentialsDir(), "creds.json"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// endProcess does to a checkout's generation what its process ending would:
// the kernel drops the generation lock with the process, and nothing runs the
// release.
func endProcess(co *Checkout) {
	co.seat.mu.Lock()
	defer co.seat.mu.Unlock()
	if co.seat.held != nil {
		co.seat.held()
		co.seat.held = nil
	}
}
