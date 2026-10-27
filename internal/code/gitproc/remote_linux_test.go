package gitproc_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/redact"
)

// THE CREDENTIAL NEVER REACHES AN ARGUMENT. While the remote holds the request
// a run sent it — the remote helper waiting on its answer, git fetch waiting
// on the helper — every process of the machine is read from /proc: no
// process's command line, which every user of the machine can read, holds the
// header's value, the credential in it or what a Basic credential encodes,
// while the remote helper's environment, which only its own user can read,
// holds the header — so the header the remote received reached it through the
// environment alone. And the remote, which echoes what it was sent in the error
// it answers with, has every one of them taken out of the lines the run hands
// back, on every git a git node may run.
//
// Mutation: hand git the header as a `-c http.extraHeader=…` before its
// subcommand, and the command line of the git the pool starts holds it.
func TestTheCredentialNeverReachesArgv(t *testing.T) {
	t.Parallel()
	onNodeGits(t, credentialNeverInArgvOn)
}

// credentialNeverInArgvOn is TestTheCredentialNeverReachesArgv on the git at
// git, for a Bearer credential and a Basic one.
func credentialNeverInArgvOn(t *testing.T, git string) {
	password := secretHex(t, "pw-")
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + password))
	bearer := secretHex(t, "ghp_")
	for _, tc := range []struct {
		name, value string
		secrets     []string // every form a process's arguments must not hold
	}{
		{"bearer", "Bearer " + bearer, []string{bearer}},
		{"basic", "Basic " + basic, []string{basic, password}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var (
				mu sync.Mutex
				// url is the remote's, set before the run; the rest is what
				// the remote saw while it held the run's request.
				url      string
				received string
				holders  []string
				helper   bool
				inEnv    bool
			)
			r, _ := tlsRemote(t, "/r.git", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				received = req.Header.Get("Authorization")
				holders, helper, inEnv = readProcs(t, tc.secrets, url, "Authorization: "+tc.value)
				mu.Unlock()
				echoed := "this host was sent " + req.Header.Get("Authorization")
				if _, pw, ok := req.BasicAuth(); ok {
					echoed += "\nwhose password is " + pw
				}
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintln(w, echoed)
			}))
			r, err := r.WithHeader("Authorization: " + tc.value)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			url = r.URL()
			mu.Unlock()

			res, err := fetchFrom(t, poolOn(t, git, gitproc.Options{}), r, gittest.Empty(t))
			var exit *gitproc.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("a fetch the remote refused: %v, want an *ExitError\n%s", err, res.Stderr)
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case received != tc.value:
				t.Fatalf("the remote received the header %q, want %q", received, tc.value)
			case !helper:
				t.Fatalf("no remote helper dialing %s was alive while the remote held its request: "+
					"the processes were not read while the dial was in flight", url)
			}
			if len(holders) > 0 {
				t.Errorf("command lines hold the credential:\n  %s", strings.Join(holders, "\n  "))
			}
			if !inEnv {
				t.Error("the remote helper's environment does not hold the header the remote received")
			}
			for what, text := range map[string]string{"stderr": res.Stderr, "the error": err.Error()} {
				for _, s := range append([]string{tc.value}, tc.secrets...) {
					if strings.Contains(text, s) {
						t.Errorf("%s names %q:\n%s", what, s, text)
					}
				}
			}
			if !strings.Contains(res.Stderr, "remote: this host was sent "+redact.Marker) {
				t.Errorf("the remote's echo is not in stderr with the credential taken out:\n%s", res.Stderr)
			}
		})
	}
}

// readProcs reads every process of the machine from /proc: the command lines
// holding any of secrets, each "<pid>: <argv>"; whether a remote helper
// dialing url was among them; and whether such a helper's environment holds
// header. A process that ended while it was read is passed over.
func readProcs(t *testing.T, secrets []string, url, header string) (holders []string, helper, inEnv bool) {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Errorf("listing /proc: %v", err)
		return nil, false, false
	}
	for _, e := range entries {
		pid := e.Name()
		if strings.TrimLeft(pid, "0123456789") != "" {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + pid + "/cmdline")
		if err != nil || len(cmdline) == 0 {
			continue
		}
		argv := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
		if slices.ContainsFunc(secrets, func(s string) bool { return bytes.Contains(cmdline, []byte(s)) }) {
			holders = append(holders, pid+": "+strings.Join(argv, " "))
		}
		if !slices.Contains(argv, url) || !slices.ContainsFunc(argv, func(a string) bool { return strings.HasSuffix(a, "remote-https") }) {
			continue
		}
		helper = true
		environ, err := os.ReadFile("/proc/" + pid + "/environ")
		if err != nil {
			continue
		}
		for _, v := range strings.Split(string(environ), "\x00") {
			if name, value, _ := strings.Cut(v, "="); strings.HasPrefix(name, "GIT_CONFIG_VALUE_") && value == header {
				inEnv = true
			}
		}
	}
	return holders, helper, inEnv
}
