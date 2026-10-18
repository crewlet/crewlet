package cliagent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The cases here hold the PENDING LOGIN: a `crewlet llm login` whose export
// failed after the operator finished signing in is kept in the login home and
// carried out by the next command — ordered by the operator epoch, so it still
// outranks a seat's refresh of the login it replaced and never an operator's
// own later write of the login of record — and a carry that can never succeed
// says how to discard it, and never stands between the operator and the
// commands that do.

// pendingLogin leaves the login FAKE_LOGIN_KEY names pending in the login
// home: a login command whose export failed once.
func pendingLogin(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	merged := map[string]string{
		"FAKE_LOGIN_KEY": "k-pending", "FAKE_STORE_EXPORT_FAIL_ONCE": filepath.Join(t.TempDir(), "once"),
	}
	for k, v := range env {
		merged[k] = v
	}
	p := storeProvider(t, merged, nil)
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("a login whose export failed was reported as written")
	}
	return p
}

// A PENDING LOGIN NEVER OVERWRITES A LATER `-from-host`. The operator adopted
// their own host login after the failed sign-in; the next `status` carried the
// pending login out over whatever the shared directory held — that adoption.
func TestAPendingLoginNeverOverwritesALaterAdoptedHostLogin(t *testing.T) {
	t.Parallel()
	p := pendingLogin(t, nil)
	host := t.TempDir()
	writeFile(t, filepath.Join(host, ".local", "share", "fake", "db.json"), `[{"id":"cred_h","key":"k-host"}]`)
	if _, err := p.AdoptHostLogin(t.Context(), host); err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-host") || strings.Contains(got, "k-pending") {
		t.Errorf("a pending login was carried out over the operator's later -from-host: %q", got)
	}
}

// A PENDING LOGIN NEVER OVERWRITES A LATER IMPORT, which `crewlet llm import`
// writes into the empty directory a failed first login leaves.
func TestAPendingLoginNeverOverwritesALaterImportedBundle(t *testing.T) {
	t.Parallel()
	p := pendingLogin(t, nil)
	if err := p.ImportBundle(t.Context(), bundleOf(t, "exported-credentials",
		`[{"id":"cred_b","key":"k-bundle"}]`)); err != nil {
		t.Fatalf("ImportBundle: %v", err)
	}
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-bundle") || strings.Contains(got, "k-pending") {
		t.Errorf("a pending login was carried out over the operator's later import: %q", got)
	}
}

// A PENDING LOGIN STILL OUTRANKS A SEAT'S REFRESH of the login it replaced:
// the operator signed in because that login was the one to replace, and a
// seat refreshing it in the meantime changes nothing about that.
func TestAPendingLoginStillOutranksASeatsRefresh(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "once")
	p := storeProvider(t, map[string]string{
		"FAKE_LOGIN_KEY": "k-pending", "FAKE_STORE_EXPORT_FAIL_ONCE": marker,
	}, nil)
	writeDocument(t, p, `[{"id":"cred_1","key":"k-old"}]`+"\n")
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("a login whose export failed was reported as written")
	}
	p.env = fakeChildEnv(map[string]string{"FAKE_STORE": "1", "FAKE_STORE_REFRESH": "k-refreshed"})
	if _, err := ask(t, p, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-refreshed") {
		t.Fatalf("the seat's refresh did not land: %q", got)
	}
	p.env = fakeChildEnv(map[string]string{"FAKE_STORE": "1"})
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-pending") {
		t.Errorf("a seat's refresh of the old login outranked the operator's pending login: %q", got)
	}
}

// A PENDING LOGIN OUTRANKS A BOOT RESTORE, which is not an operator's write:
// it fills the directory the failed login left empty with the login the
// deployment was configured with, and the sign-in on this node is the newer.
func TestAPendingLoginOutranksABootRestore(t *testing.T) {
	t.Parallel()
	p := pendingLogin(t, nil)
	if err := p.RestoreBundle(t.Context(), bundleOf(t, "exported-credentials", `[{"id":"cred_b","key":"k-bundle"}]`)); err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-pending") {
		t.Errorf("a boot restore outranked the operator's pending login: %q", got)
	}
}

// A LOGOUT SUPERSEDES A PENDING LOGIN IT COULD NOT DISCARD. Another command held
// the login home, so the logout left it alone — and the next command carried
// the pending login out over the logout.
func TestALogoutSupersedesAPendingLoginItCouldNotDiscard(t *testing.T) {
	t.Parallel()
	p := pendingLogin(t, nil)
	unlock, err := p.ws.lockLoginHome(t.Context())
	if err != nil {
		t.Fatalf("lockLoginHome: %v", err)
	}
	if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	unlock()
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := readDocument(t, p); got != "" {
		t.Errorf("a pending login was carried out over a logout: %q", got)
	}
}

// A CARRY THAT CAN NEVER SUCCEED NAMES THE WAY OUT — for a seat, the record to
// remove; for the login home, the commands that discard it — because it
// refuses every attempt for as long as it fails.
func TestACarryThatCannotSucceedNamesTheWayOut(t *testing.T) {
	t.Parallel()
	t.Run("seat", func(t *testing.T) {
		t.Parallel()
		p := storeProvider(t, map[string]string{"FAKE_STORE_REFRESH": "k-new", "FAKE_STORE_EXPORT_FAIL": "1"}, nil)
		writeDocument(t, p, `[{"id":"cred_1","key":"k-old"}]`+"\n")
		if _, err := ask(t, p, llm.Request{}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		_, err := ask(t, p, llm.Request{})
		var carry *carryOutError
		if !errors.As(err, &carry) {
			t.Fatalf("Complete = %v, want the kept login's carry-out refused", err)
		}
		record := filepath.Join(p.ws.SeatDir("shared"), generationFile)
		if !strings.Contains(err.Error(), "remove "+record) {
			t.Errorf("the refusal does not name the record to remove (%s): %v", record, err)
		}
	})
	t.Run("login home", func(t *testing.T) {
		t.Parallel()
		p := storeProvider(t, map[string]string{
			"FAKE_LOGIN_KEY": "k-pending", "FAKE_STORE_EXPORT_FAIL_KEY": "k-pending",
		}, nil)
		if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatal("a login whose export failed was reported as written")
		}
		err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{})
		var carry *carryOutError
		if !errors.As(err, &carry) {
			t.Fatalf("Status = %v, want the kept login's carry-out refused", err)
		}
		for _, want := range []string{"`crewlet llm login`", "`crewlet llm logout`",
			filepath.Join(p.ws.Root(), loginGenerationFile)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %s: %v", want, err)
			}
		}
	})
}

// A LOGIN REPLACES A PENDING LOGIN IT CANNOT CARRY OUT. It is the command that
// repairs a broken sign-in, and refusing it over the leftovers of the last one
// left an operator whose export kept failing unable to sign in at all.
func TestALoginReplacesAPendingLoginItCannotCarryOut(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{
		"FAKE_LOGIN_KEY": "k-pending", "FAKE_STORE_EXPORT_FAIL_KEY": "k-pending",
	}, nil)
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("a login whose export failed was reported as written")
	}
	p.env = fakeChildEnv(map[string]string{
		"FAKE_STORE": "1", "FAKE_LOGIN_KEY": "k-new", "FAKE_STORE_EXPORT_FAIL_KEY": "k-pending",
	})
	var errOut bytes.Buffer
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &errOut); err != nil {
		t.Fatalf("BrokerLogin: %v (%s)", err, errOut.String())
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-new") || strings.Contains(got, "k-pending") {
		t.Errorf("the shared login is %q, want the new sign-in alone", got)
	}
	if !strings.Contains(errOut.String(), "discarding the login") {
		t.Errorf("the operator was not told the kept login was discarded: %q", errOut.String())
	}
}

// A LOGOUT DISCARDS A PENDING LOGIN IT CANNOT CARRY OUT, and is not failed by
// it: the vendor's own logout used to begin by carrying the pending login out,
// over the login it was about to revoke, and reported that carry's failure as
// the logout's.
func TestALogoutDiscardsAPendingLoginItCannotCarryOut(t *testing.T) {
	t.Parallel()
	run := "-test.run=TestCLIAgentFakeCLI"
	p := storeProvider(t, map[string]string{
		"FAKE_LOGIN_KEY": "k-pending", "FAKE_STORE_EXPORT_FAIL_KEY": "k-pending",
	}, map[string]any{"logout_args": []any{run, "store-list"}})
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("a login whose export failed was reported as written")
	}
	if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status after the logout: %v", err)
	}
	if got := readDocument(t, p); got != "" {
		t.Errorf("the pending login survived the logout: %q", got)
	}
	if _, err := os.Stat(filepath.Join(p.ws.Root(), loginGenerationFile)); !os.IsNotExist(err) {
		t.Errorf("the login home's generation is still open after the logout (%v)", err)
	}
}

// A PENDING LOGIN IS ORDERED BY WHEN IT FINISHED. An operator's write that
// landed while the browser flow was still open is one the sign-in, finishing
// later, would have been written over had its export worked — so a failed
// export must not turn the order round.
func TestAPendingLoginOutranksAWriteThatLandedWhileItRan(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "once")
	p := storeProvider(t, nil, nil)
	// The operator's other write is the epoch moving on mid-flow; what it
	// wrote does not matter here, only that it came before this login ended.
	p.env = fakeChildEnv(map[string]string{
		"FAKE_STORE": "1", "FAKE_LOGIN_KEY": "k-pending", "FAKE_STORE_EXPORT_FAIL_ONCE": marker,
		"FAKE_MIDRUN_VERB": "store-login",
		"FAKE_MIDRUN_PATH": filepath.Join(p.ws.Root(), loginEpochFile),
		"FAKE_MIDRUN_BODY": "7\n",
	})
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("a login whose export failed was reported as written")
	}
	p.env = fakeChildEnv(map[string]string{"FAKE_STORE": "1"})
	if err := p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-pending") {
		t.Errorf("a write that landed while the sign-in ran outranked it once its export failed: %q", got)
	}
}

// THE LATER OF TWO SIGN-INS WINS. An operator's write that landed while a
// login's browser flow was open — a `-from-host` in another terminal — is
// written over by the login when it finishes: the epoch orders only a login
// carried out LATER, and a login whose own carry honoured it was thrown away
// the moment the operator completed it.
func TestALoginOutranksAWriteThatLandedWhileItRan(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, nil, nil)
	p.env = fakeChildEnv(map[string]string{
		"FAKE_STORE": "1", "FAKE_LOGIN_KEY": "k-login",
		"FAKE_MIDRUN_VERB": "store-login",
		"FAKE_MIDRUN_PATH": filepath.Join(p.ws.Root(), loginEpochFile),
		"FAKE_MIDRUN_BODY": "7\n",
	})
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("BrokerLogin: %v", err)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-login") {
		t.Errorf("a login was dropped for a write that landed while it ran: %q", got)
	}
}
