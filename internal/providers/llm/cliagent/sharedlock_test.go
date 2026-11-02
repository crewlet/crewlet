package cliagent

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// The cases here hold EVERY WRITER OF THE SHARED LOGIN TO ITS LOCK. A
// compare-and-swap is one only against writers that agree on the lock, and the
// other writers are processes of their own — a `crewlet llm` command, a local
// coding box's teardown — which the test stands in for by holding the lock
// through hostbox, on an open file description of its own, exactly as such a
// process would. Each writer must wait while it is held, leaving the shared
// directory as it was, and land once it is let go.

// lockHeldFor is how long a case holds the lock before it looks: far past the
// moment a writer reaches the lock, and far inside hostbox.CredentialLockWait,
// so the writer is still waiting when the lock is let go.
const lockHeldFor = 200 * time.Millisecond

// whileTheLockIsHeld takes the shared login's lock as another process would,
// starts write, and calls held once the writer has had lockHeldFor to reach
// the lock; then it lets the lock go and returns what write returned.
func whileTheLockIsHeld(t *testing.T, ws *Workspace, write func() error, held func()) error {
	t.Helper()
	release, err := hostbox.LockCredentials(t.Context(), ws.CredentialsDir())
	if err != nil {
		t.Fatalf("taking the shared login's lock: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- write() }()
	time.Sleep(lockHeldFor)
	func() {
		defer release()
		select {
		case err := <-done:
			t.Errorf("the write finished while another holder had the lock (%v)", err)
			done <- err
		default:
		}
		held()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(hostbox.CredentialLockWait + 5*time.Second):
		t.Fatal("the write never finished once the lock was let go")
		return nil
	}
}

// EVERY WRITER WAITS FOR THE SHARED LOGIN'S LOCK.
//
// Mutation: drop the lockShared from a seat's carry (syncCredentialsOut),
// -from-host, the logout's removal or a bundle's restore, and its case is red.
func TestEveryWriterOfTheSharedLoginWaitsForItsLock(t *testing.T) {
	t.Parallel()
	t.Run("a seat's release", func(t *testing.T) {
		t.Parallel()
		ws := newWorkspace(t)
		writeCreds(t, &Provider{ws: ws}, `{"token":"R0"}`)
		co, err := ws.Acquire(t.Context(), "dev", "call", nil)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		refreshFile(t, co, `{"token":"R1"}`)
		err = whileTheLockIsHeld(t, ws, func() error { return co.Release(t.Context()) }, func() {
			if got := readCreds(t, &Provider{ws: ws}); got != `{"token":"R0"}` {
				t.Errorf("a seat carried its refresh out while another holder had the lock: %q", got)
			}
		})
		if err != nil {
			t.Fatalf("Release: %v", err)
		}
		if got := readCreds(t, &Provider{ws: ws}); got != `{"token":"R1"}` {
			t.Errorf("the refresh did not land once the lock was free: %q", got)
		}
	})
	t.Run("-from-host", func(t *testing.T) {
		t.Parallel()
		p := loginProvider(t)
		home := t.TempDir()
		writeHostLogin(t, home, `{"token":"k-host"}`)
		err := whileTheLockIsHeld(t, p.ws, func() error {
			_, err := p.AdoptHostLogin(t.Context(), home)
			return err
		}, func() {
			if got := loginOnDisk(t, p.ws); len(got) > 0 {
				t.Errorf("-from-host wrote while another holder had the lock: %v", got)
			}
		})
		if err != nil {
			t.Fatalf("AdoptHostLogin: %v", err)
		}
		if got := readCreds(t, p); got != `{"token":"k-host"}` {
			t.Errorf("the adopted login did not land once the lock was free: %q", got)
		}
	})
	t.Run("logout", func(t *testing.T) {
		t.Parallel()
		p := loginProvider(t)
		writeCreds(t, p, `{"token":"k-live"}`)
		err := whileTheLockIsHeld(t, p.ws, func() error {
			return p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{})
		}, func() {
			if got := loginOnDisk(t, p.ws); len(got) != 1 {
				t.Errorf("the logout touched the login while another holder had the lock: %v", got)
			}
			if out, _ := p.ws.loggedOut(); out {
				t.Error("the logout recorded itself while another holder had the lock")
			}
		})
		if err != nil {
			t.Fatalf("Logout: %v", err)
		}
		if hasLogin(t, p.ws) {
			t.Errorf("the logout did not land once the lock was free: %v", loginOnDisk(t, p.ws))
		}
	})
	for name, restore := range map[string]func(*Provider, string) error{
		"an import": func(p *Provider, blob string) error { return p.ImportBundle(t.Context(), blob) },
		"a restore": func(p *Provider, blob string) error { return p.RestoreBundle(t.Context(), blob) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := loginProvider(t)
			blob := bundleOf(t, p.agent, "creds.json", `{"token":"k-bundled"}`)
			err := whileTheLockIsHeld(t, p.ws, func() error { return restore(p, blob) }, func() {
				if got := loginOnDisk(t, p.ws); len(got) > 0 {
					t.Errorf("%s wrote while another holder had the lock: %v", name, got)
				}
			})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got := readCreds(t, p); got != `{"token":"k-bundled"}` {
				t.Errorf("%s did not land once the lock was free: %q", name, got)
			}
		})
	}
}

// A BUNDLE IS DECIDED UNDER THE LOCK IT WRITES UNDER. Its emptiness — and
// whether a logout stands — is asked once cheaply and again under the lock,
// because a login or a logout that lands while the bundle is being validated
// is the newer decision: a login landing then is kept, and the import that
// found it says so rather than reporting a bundle restored; a logout landing
// then keeps the automatic restore out.
//
// Mutation: drop the check made under the lock in restoreBundle, and the
// bundle is written over the login that landed, or into the directory the
// logout emptied.
func TestABundleIsDecidedUnderTheLockItWritesUnder(t *testing.T) {
	t.Parallel()
	t.Run("a login lands", func(t *testing.T) {
		t.Parallel()
		p := loginProvider(t)
		blob := bundleOf(t, p.agent, "creds.json", `{"token":"k-bundled"}`)
		err := whileTheLockIsHeld(t, p.ws, func() error { return p.ImportBundle(t.Context(), blob) }, func() {
			// Another holder's login, written under the lock it holds.
			writeCreds(t, p, `{"token":"k-landed"}`)
		})
		if !errors.Is(err, ErrLoginPresent) {
			t.Errorf("an import over a login that landed while it waited = %v, want ErrLoginPresent", err)
		}
		if got := readCreds(t, p); got != `{"token":"k-landed"}` {
			t.Errorf("the import replaced the login that landed while it waited: %q", got)
		}
	})
	t.Run("a logout lands", func(t *testing.T) {
		t.Parallel()
		p := loginProvider(t)
		blob := bundleOf(t, p.agent, "creds.json", `{"token":"k-bundled"}`)
		err := whileTheLockIsHeld(t, p.ws, func() error { return p.RestoreBundle(t.Context(), blob) }, func() {
			// Another holder's logout of an empty directory, recorded
			// under the lock it holds.
			if err := p.ws.markLoggedOut(); err != nil {
				t.Fatal(err)
			}
		})
		if !errors.Is(err, ErrLoggedOut) {
			t.Errorf("a restore after a logout that landed while it waited = %v, want ErrLoggedOut", err)
		}
		if hasLogin(t, p.ws) {
			t.Errorf("the restore filled the directory a logout emptied while it waited: %v", loginOnDisk(t, p.ws))
		}
	})
}

// EVERY WAITER FOR THE SHARED LOGIN'S LOCK KEEPS ITS OWN DEADLINE. Another
// process holds the lock — a `crewlet llm` command stopped part way — while one
// seat of this process waits for it with no deadline of its own, and a second,
// whose call allows it a fraction of a second, has to be answered in that
// fraction: every seat's start reads the login under this lock, and a mutex
// queued ahead of the flock held the second until the first had the lock, or
// had waited out its whole hostbox.CredentialLockWait.
//
// Mutation: queue this process's takers on a mutex ahead of the flock, as
// lockShared once did, and the second waiter is answered ten seconds late.
func TestAWaiterForTheSharedLoginsLockKeepsItsOwnDeadline(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	release, err := hostbox.LockCredentials(t.Context(), ws.CredentialsDir())
	if err != nil {
		t.Fatalf("taking the shared login's lock: %v", err)
	}
	first := make(chan error, 1)
	go func() {
		_, err := ws.Login(context.Background())
		first <- err
	}()
	time.Sleep(lockHeldFor)

	ctx, cancel := context.WithTimeout(t.Context(), lockHeldFor)
	defer cancel()
	started := time.Now()
	_, err = ws.Login(ctx)
	waited := time.Since(started)
	release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a read of the login past its own deadline = %v, want its deadline", err)
	}
	if waited > 5*lockHeldFor {
		t.Errorf("a read of the login allowed %v waited %v, behind another waiter of this process",
			lockHeldFor, waited)
	}
	select {
	case err := <-first:
		if err != nil {
			t.Errorf("the waiter with no deadline did not take the lock once it was let go: %v", err)
		}
	case <-time.After(hostbox.CredentialLockWait + 5*time.Second):
		t.Fatal("the waiter with no deadline never finished once the lock was let go")
	}
}
