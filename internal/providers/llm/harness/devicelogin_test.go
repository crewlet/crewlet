package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// fakeDeviceLogin plays Codex's `login --device-auth` as measured on the
// installed CLI: the steps in ANSI colour, the address, the code's lifetime on
// the line before the code, then a wait for the person's approval, after which
// it writes its login file and exits 0. It writes its pid to FAKE_DEVICE_PID
// first, where that names a file.
//
//   - FAKE_DEVICE_STREAM=stderr prints the steps on stderr instead of stdout;
//   - FAKE_DEVICE_MINUTES is the lifetime it prints (15 when unset);
//   - FAKE_DEVICE_PRINT=none prints the first step and no code, then exits
//     FAKE_EXIT, having written its login file where FAKE_DEVICE_WRITE=1;
//     =hang prints nothing and waits;
//   - FAKE_DEVICE_APPROVE names the file whose appearance is the approval:
//     $HOME/.fake/creds.json is then written with FAKE_LOGIN_KEY.
func fakeDeviceLogin() {
	if pidFile := os.Getenv("FAKE_DEVICE_PID"); pidFile != "" {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	w := os.Stdout
	if os.Getenv("FAKE_DEVICE_STREAM") == "stderr" {
		w = os.Stderr
	}
	minutes := os.Getenv("FAKE_DEVICE_MINUTES")
	if minutes == "" {
		minutes = "15"
	}
	switch os.Getenv("FAKE_DEVICE_PRINT") {
	case "none":
		fmt.Fprint(w, "\x1b[1mFollow these steps to sign in with ChatGPT using device code authorization:\x1b[0m\n")
		fmt.Fprint(os.Stderr, "error: the device authorization endpoint answered 503\n")
		if os.Getenv("FAKE_DEVICE_WRITE") == "1" {
			file := filepath.Join(os.Getenv("HOME"), ".fake", "creds.json")
			_ = os.MkdirAll(filepath.Dir(file), 0o700)
			_ = os.WriteFile(file, []byte(`{"token":"`+os.Getenv("FAKE_LOGIN_KEY")+`"}`), 0o600)
		}
		code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
		os.Exit(code)
	case "hang":
		time.Sleep(fakeAwaitBound)
		os.Exit(0)
	}
	fmt.Fprintf(w, "\n\x1b[1mFollow these steps to sign in with ChatGPT using device code authorization:\x1b[0m\n\n"+
		"1. Open this link in your browser and sign in to your account\n"+
		"   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m\n\n"+
		"2. Enter this one-time code \x1b[90m(expires in %s minutes)\x1b[0m\n"+
		"   \x1b[94mNJR6-SRA6K\x1b[0m\n\n"+
		"\x1b[90mDevice codes are a common phishing target. Never share this code.\x1b[0m\n\n", minutes)
	approve := os.Getenv("FAKE_DEVICE_APPROVE")
	deadline := time.Now().Add(fakeAwaitBound)
	for _, err := os.Stat(approve); err != nil; _, err = os.Stat(approve) {
		if !time.Now().Before(deadline) {
			fmt.Fprintln(os.Stderr, "fake: never approved")
			os.Exit(1)
		}
		time.Sleep(10 * time.Millisecond)
	}
	file := filepath.Join(os.Getenv("HOME"), ".fake", "creds.json")
	_ = os.MkdirAll(filepath.Dir(file), 0o700)
	_ = os.WriteFile(file, []byte(`{"token":"`+os.Getenv("FAKE_LOGIN_KEY")+`"}`), 0o600)
	fmt.Fprintln(w, "Successfully logged in")
}

// deviceProvider is a provider whose CLI keeps its login in one file and signs
// in by [fakeDeviceLogin], read with the SHIPPED codex profile's
// device_code_patterns, so the patterns are held to the output they were
// measured on.
func deviceProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	codex, err := harnessprofile.Load("codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	patterns := codex.DeviceCodePatterns
	merged := map[string]string{"FAKE_STORE": "1", "FAKE_DEVICE_APPROVE": filepath.Join(t.TempDir(), "approved")}
	for k, v := range env {
		merged[k] = v
	}
	run := "-test.run=TestFakeHarness"
	return fakeProvider(t, merged, map[string]any{
		"credential_paths":  []any{".fake/creds.json"},
		"volatile_paths":    []any{".local/share/fake"},
		"device_login_args": []any{run, "store-device-login"},
		"device_code_patterns": map[string]any{
			"url": patterns.URL, "code": patterns.Code, "lifetime": patterns.Lifetime,
		},
	})
}

// approve is the person approving the code at the vendor.
func approve(t *testing.T, p *Provider) {
	t.Helper()
	if err := os.WriteFile(p.env["FAKE_DEVICE_APPROVE"], nil, 0o600); err != nil {
		t.Error(err)
	}
}

// codes collects what a device sign-in relays.
type codes struct {
	mu  sync.Mutex
	got []DeviceCode
}

func (c *codes) relay(then func(DeviceCode)) func(DeviceCode) {
	return func(code DeviceCode) {
		c.mu.Lock()
		c.got = append(c.got, code)
		c.mu.Unlock()
		if then != nil {
			then(code)
		}
	}
}

func (c *codes) all() []DeviceCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]DeviceCode(nil), c.got...)
}

// A DEVICE SIGN-IN RELAYS THE ADDRESS AND THE CODE, ONCE, AND AN APPROVAL
// SIGNS THE ENTRY IN. The code is read off Codex's own output, colour and all,
// on whichever stream it is written; its lifetime off the line before it; and
// once the person approves, the login the CLI wrote is the entry's, under a new
// install record.
//
// Mutation: strip no ANSI (the code is read with its colour), read stdout alone
// (the stderr case relays nothing), or call out per line (twice), and this is
// red.
func TestADeviceSignInRelaysItsCodeOnceAndAnApprovalSignsIn(t *testing.T) {
	t.Parallel()
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			t.Parallel()
			p := deviceProvider(t, map[string]string{"FAKE_DEVICE_STREAM": stream, "FAKE_LOGIN_KEY": "k-device"})
			var relayed codes
			started := time.Now()
			err := DeviceLogin(t.Context(), p, relayed.relay(func(DeviceCode) { approve(t, p) }))
			if err != nil {
				t.Fatalf("DeviceLogin: %v", err)
			}
			got := relayed.all()
			if len(got) != 1 {
				t.Fatalf("relayed %d codes, want exactly one: %+v", len(got), got)
			}
			if got[0].URL != "https://auth.openai.com/codex/device" || got[0].Code != "NJR6-SRA6K" {
				t.Errorf("relayed %+v, want the address and the code the CLI printed", got[0])
			}
			if life := got[0].ExpiresAt.Sub(started); life < 14*time.Minute || life > 16*time.Minute {
				t.Errorf("the code expires %s after the start, want the 15 minutes the CLI printed", life)
			}
			if creds := readCreds(t, p); creds != `{"token":"k-device"}` {
				t.Errorf("the approved login is not the entry's: %q", creds)
			}
			if installRecord(t, p).IsZero() {
				t.Error("the device sign-in installed a login with no install record")
			}
		})
	}
}

// A CODE THAT LAPSES STOPS THE SIGN-IN, its process group with it, and signs
// nothing in: the deadline is the lifetime the CLI printed plus the grace.
//
// Mutation: drop the deadline after the code is read, and this case waits
// for the fake's own bound and is red.
func TestADeviceCodeThatLapsesStopsTheSignIn(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	p := deviceProvider(t, map[string]string{"FAKE_DEVICE_MINUTES": "2", "FAKE_DEVICE_PID": pidFile})
	timing := deviceTiming{codeWait: 20 * time.Second, lifetime: time.Hour, minute: 100 * time.Millisecond,
		grace: 100 * time.Millisecond}
	var relayed codes
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	err := deviceLogin(ctx, p, relayed.relay(nil), timing)
	if !errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("DeviceLogin with a lapsed code = %v, want ErrDeviceCodeExpired", err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("the lapsed sign-in took %s, want about the printed lifetime and the grace", took)
	}
	if got := relayed.all(); len(got) != 1 || got[0].ExpiresAt.Sub(started) > time.Second {
		t.Errorf("relayed %+v, want one code expiring after the printed two (short) minutes", got)
	}
	assertGone(t, pidFile)
	if creds := readCreds(t, p); creds != "" {
		t.Errorf("a lapsed sign-in left a login: %q", creds)
	}
}

// A CANCELLED SIGN-IN KILLS ITS CLI and answers the cancellation, so a person
// who cancels from the dashboard leaves no process polling the vendor.
//
// Mutation: start the CLI on a context the cancellation does not reach, and
// the call waits on the CLI for the fake's whole bound here.
func TestACancelledDeviceSignInKillsItsHarness(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	p := deviceProvider(t, map[string]string{"FAKE_DEVICE_PID": pidFile})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var relayed codes
	started := time.Now()
	err := DeviceLogin(ctx, p, relayed.relay(func(DeviceCode) { cancel() }))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DeviceLogin cancelled = %v, want context.Canceled", err)
	}
	if took := time.Since(started); took > termGrace+5*time.Second {
		t.Errorf("the cancelled sign-in returned after %s, want once its CLI was stopped", took)
	}
	assertGone(t, pidFile)
	if creds := readCreds(t, p); creds != "" {
		t.Errorf("a cancelled sign-in left a login: %q", creds)
	}
}

// assertGone fails the case unless the process whose pid is in pidFile, and
// its group, is gone.
func assertGone(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile) //nolint:gosec // the case's own file
	if err != nil {
		t.Fatalf("the fake never wrote its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if procgroup.Exists(pid) {
		t.Errorf("the device sign-in's CLI (pid %d) outlived the sign-in", pid)
	}
}

// A CLI THAT PRINTS NO CODE IS A FAILURE THAT SAYS SO, and relays nothing:
// one that exits without one is reported with what it did print, and one that
// prints nothing at all is stopped once the code's wait is out rather than held
// for a code's whole lifetime.
//
// Mutation: read a code-less exit 0 as approval, or drop the code's wait, and
// this is red.
func TestADeviceSignInThatPrintsNoCodeFails(t *testing.T) {
	t.Parallel()
	t.Run("exits", func(t *testing.T) {
		t.Parallel()
		p := deviceProvider(t, map[string]string{"FAKE_DEVICE_PRINT": "none", "FAKE_EXIT": "1"})
		var relayed codes
		err := DeviceLogin(t.Context(), p, relayed.relay(nil))
		if err == nil || !strings.Contains(err.Error(), "without printing a device code") ||
			!strings.Contains(err.Error(), "503") {
			t.Errorf("DeviceLogin = %v, want a failure naming the missing code and what the CLI said", err)
		}
		if got := relayed.all(); len(got) != 0 {
			t.Errorf("relayed %+v from a CLI that printed no code", got)
		}
	})
	t.Run("exits 0", func(t *testing.T) {
		t.Parallel()
		// Even one that left a login: nobody was shown a code to approve.
		p := deviceProvider(t, map[string]string{"FAKE_DEVICE_PRINT": "none", "FAKE_EXIT": "0",
			"FAKE_DEVICE_WRITE": "1", "FAKE_LOGIN_KEY": "k-unshown"})
		if err := DeviceLogin(t.Context(), p, func(DeviceCode) {}); err == nil {
			t.Error("a CLI that exited 0 without a code was taken as approved")
		}
	})
	t.Run("hangs", func(t *testing.T) {
		t.Parallel()
		p := deviceProvider(t, map[string]string{"FAKE_DEVICE_PRINT": "hang"})
		timing := deviceLoginTimingDefault
		timing.codeWait = 200 * time.Millisecond
		started := time.Now()
		err := deviceLogin(t.Context(), p, func(DeviceCode) {}, timing)
		if err == nil || !strings.Contains(err.Error(), "printed no device code within") {
			t.Errorf("DeviceLogin = %v, want the code's wait reported", err)
		}
		if took := time.Since(started); took > 20*time.Second {
			t.Errorf("a CLI that printed nothing held the sign-in for %s", took)
		}
	})
}

// A DEVICE SIGN-IN IS REFUSED WHERE IT COULD SIGN NOTHING IN: a CLI with no
// device sign-in, and an entry whose calls read no login (api-key, env). The
// refusal comes before any process starts.
//
// Mutation: drop either refusal, and its case is red.
func TestADeviceSignInIsRefusedWhereNoLoginIsRead(t *testing.T) {
	t.Parallel()
	t.Run("no device sign-in", func(t *testing.T) {
		t.Parallel()
		p := fileProvider(t, nil)
		if err := DeviceLogin(t.Context(), p, func(DeviceCode) {}); !errors.Is(err, ErrNoLoginCommand) {
			t.Errorf("DeviceLogin on a CLI with none = %v, want ErrNoLoginCommand", err)
		}
	})
	for _, mode := range []AuthMode{AuthAPIKey, AuthEnv} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			p := deviceProvider(t, nil)
			p.auth = Auth{Mode: mode, APIKey: "sk-entry"}
			err := DeviceLogin(t.Context(), p, func(DeviceCode) { t.Error("a refused sign-in relayed a code") })
			if err == nil || !strings.Contains(err.Error(), "not signed in by a login") {
				t.Errorf("DeviceLogin under %s = %v, want it refused as reading no login", mode, err)
			}
		})
	}
}
