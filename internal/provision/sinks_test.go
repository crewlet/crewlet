package provision_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// ONE SUITE OVER ALL THREE SINKS, because the contract is what the
// provisioning CLIs depend on and a sink that honours it differently is a
// run whose safety depends on which flag the operator passed.
type sinkCase struct {
	name string
	// build returns the sink and a reader for what it holds, so the suite
	// can assert on the result without knowing how it is stored.
	build func(t *testing.T) (provision.TokenSink, func(t *testing.T) map[string]string)
}

func sinkCases() []sinkCase {
	return []sinkCase{
		{
			name: "the secret store",
			build: func(t *testing.T) (provision.TokenSink, func(*testing.T) map[string]string) {
				db, err := store.Open(t.Context(),
					filepath.Join(t.TempDir(), "s.db"), store.Options{})
				if err != nil {
					t.Fatalf("store.Open: %v", err)
				}
				t.Cleanup(func() { _ = db.Close() })
				key, err := secrets.GenerateKey()
				if err != nil {
					t.Fatalf("GenerateKey: %v", err)
				}
				cipher, err := secrets.NewCipher(secrets.Keyring{
					ActiveID: "k1", Keys: map[string][]byte{"k1": key},
				})
				if err != nil {
					t.Fatalf("NewCipher: %v", err)
				}
				values := db.SecretValues(cipher)
				return provision.NewSecretStoreSink(values, "operator"),
					func(t *testing.T) map[string]string {
						got, err := values.All(context.Background())
						if err != nil {
							t.Fatalf("All: %v", err)
						}
						return got
					}
			},
		},
		{
			name: "an env file",
			build: func(t *testing.T) (provision.TokenSink, func(*testing.T) map[string]string) {
				path := filepath.Join(t.TempDir(), "creds", ".env")
				sink, err := provision.NewEnvFileSink(path)
				if err != nil {
					t.Fatalf("NewEnvFileSink: %v", err)
				}
				return sink, func(t *testing.T) map[string]string {
					return readEnvFile(t, path)
				}
			},
		},
	}
}

func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if name, value, ok := parseLine(line); ok {
			out[name] = value
		}
	}
	return out
}

// parseLine is deliberately a SECOND reader, hand-written here rather than
// the package's own: a test that parses with the code under test proves the
// two agree with each other and nothing about the file.
func parseLine(line string) (string, string, bool) {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
	name, rest, ok := strings.Cut(line, "=")
	if !ok || name == "" {
		return "", "", false
	}
	rest = strings.TrimSpace(rest)
	if len(rest) >= 2 && rest[0] == '\'' && rest[len(rest)-1] == '\'' {
		rest = rest[1 : len(rest)-1]
	}
	return name, rest, true
}

// A RECORDED CREDENTIAL IS DURABLE BEFORE Record RETURNS. Buffering to a
// flush opens a window where a token exists at the vendor and nowhere else:
// a crash there leaves it live, unrecorded, and nobody knows to revoke it.
func TestARecordedCredentialIsDurableImmediately(t *testing.T) {
	t.Parallel()
	for _, tc := range sinkCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink, read := tc.build(t)
			if err := sink.Record(t.Context(), "GITLAB_TOKEN_SWE", "glpat-abc"); err != nil {
				t.Fatalf("Record: %v", err)
			}
			// READ BEFORE FLUSH, deliberately.
			if got := read(t)["GITLAB_TOKEN_SWE"]; got != "glpat-abc" {
				t.Fatalf("before Flush the value is %q, want it already durable", got)
			}
			if err := sink.Flush(t.Context()); err != nil {
				t.Fatalf("Flush: %v", err)
			}
		})
	}
}

// DISCARD REMOVES WHAT THIS RUN WROTE. A credential revoked because the run
// could not finish must not be left standing in the sink: a dead token reads
// exactly like a live one, and an operator debugging the 401s cannot tell
// which of their credentials is real.
func TestDiscardRemovesEverythingTheRunRecorded(t *testing.T) {
	t.Parallel()
	for _, tc := range sinkCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink, read := tc.build(t)
			for name, value := range map[string]string{
				"TOKEN_A": "a", "TOKEN_B": "b", "TOKEN_C": "c",
			} {
				if err := sink.Record(t.Context(), name, value); err != nil {
					t.Fatalf("Record(%s): %v", name, err)
				}
			}
			if err := sink.Discard(t.Context()); err != nil {
				t.Fatalf("Discard: %v", err)
			}
			if got := read(t); len(got) != 0 {
				t.Fatalf("Discard left %v behind", got)
			}
		})
	}
}

// AN AWKWARD TOKEN SURVIVES EVERY SINK. These are the shapes real vendor
// credentials take, and each breaks a different naive implementation.
func TestEverySinkCarriesAnAwkwardTokenVerbatim(t *testing.T) {
	t.Parallel()
	awkward := map[string]string{
		"TOKEN_DOLLAR": "sk-ant$notavariable",
		"TOKEN_BRACE":  "token-${HOME}-suffix",
		"TOKEN_SPACE":  "two words",
		"TOKEN_HASH":   "abc#notacomment",
		"TOKEN_QUOTE":  `abc"def`,
		"TOKEN_B64":    "c2VjcmV0LXZhbHVl==",
	}
	for _, tc := range sinkCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink, read := tc.build(t)
			for name, value := range awkward {
				if err := sink.Record(t.Context(), name, value); err != nil {
					t.Fatalf("Record(%s): %v", name, err)
				}
			}
			got := read(t)
			for name, want := range awkward {
				if got[name] != want {
					t.Errorf("%s came back as %q, want %q", name, got[name], want)
				}
			}
		})
	}
}

// ---- what only the env file can be asked ------------------------------ //

// THE FILE IS 0600 FROM THE MOMENT IT EXISTS. One that appears with default
// permissions and is tightened later has a window, however short, in which
// every process on the host can read a live credential.
func TestTheEnvFileIsPrivateFromCreation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "creds", ".env")
	if _, err := provision.NewEnvFileSink(path); err != nil {
		t.Fatalf("NewEnvFileSink: %v", err)
	}
	// BEFORE anything is recorded — a run that mints nothing still leaves
	// a correctly-moded file rather than one to remember to fix.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o, want 600", mode)
	}
}

// AN EXISTING FILE KEEPS ITS OTHER CREDENTIALS. A provisioning run rotates
// one seat's token; a sink that truncated would take every unrelated
// credential with it, and they are not this command's to lose.
func TestAnExistingEnvFileKeepsWhatItAlreadyHeld(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path,
		[]byte("export UNRELATED='keep-me'\nANTHROPIC_API_KEY=sk-ant-existing\n"),
		0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sink, err := provision.NewEnvFileSink(path)
	if err != nil {
		t.Fatalf("NewEnvFileSink: %v", err)
	}
	if err := sink.Record(t.Context(), "GITLAB_TOKEN_SWE", "glpat-new"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got := readEnvFile(t, path)
	for name, want := range map[string]string{
		"UNRELATED": "keep-me", "ANTHROPIC_API_KEY": "sk-ant-existing",
		"GITLAB_TOKEN_SWE": "glpat-new",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	// AND A ROLLBACK LEAVES THEM. Discard removes what this run wrote,
	// not what it found.
	if err := sink.Discard(t.Context()); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	after := readEnvFile(t, path)
	if after["UNRELATED"] != "keep-me" || after["ANTHROPIC_API_KEY"] != "sk-ant-existing" {
		t.Fatalf("Discard removed credentials it did not write: %v", after)
	}
	if _, still := after["GITLAB_TOKEN_SWE"]; still {
		t.Error("Discard left the minted credential behind")
	}
}

// A VALUE NO ASSIGNMENT CAN CARRY IS REFUSED BEFORE ANYTHING IS WRITTEN, so
// the caller revokes the credential rather than leaving one recorded in a
// form a reader will mangle.
func TestAnUnwritableValueIsRefusedWithoutTouchingTheFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".env")
	sink, err := provision.NewEnvFileSink(path)
	if err != nil {
		t.Fatalf("NewEnvFileSink: %v", err)
	}
	if err := sink.Record(t.Context(), "TOKEN", "has'a'quote"); err == nil {
		t.Fatal("a value that cannot be written safely was accepted")
	}
	if got := readEnvFile(t, path); len(got) != 0 {
		t.Fatalf("the refused value reached the file: %v", got)
	}
}

// ---- printing --------------------------------------------------------- //

// THE PRINT SINK SAYS WHAT IT CANNOT UNDO. It cannot unprint a credential,
// so a rollback names them: these are revoked, do not paste them anywhere.
func TestThePrintSinkAnnouncesARollback(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	sink, err := provision.NewPrintSink(&out)
	if err != nil {
		t.Fatalf("NewPrintSink: %v", err)
	}
	if err := sink.Record(t.Context(), "TOKEN_A", "a"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !strings.Contains(out.String(), "TOKEN_A='a'") {
		t.Fatalf("the sink printed %q", out.String())
	}
	if err := sink.Discard(t.Context()); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "REVOKED") || !strings.Contains(body, "TOKEN_A") {
		t.Fatalf("a rollback said %q, want it to name the dead credential", body)
	}
	// AND IT IS A STATEMENT, NOT A COMMENT. The stream is meant to be
	// sourced — that is the whole reason it emits `export` lines — and a
	// comment is a no-op to a shell, so an operator who piped it into
	// `source` and then hit a rollback would keep a revoked token exported
	// in their session.
	if !strings.Contains(body, "\nunset TOKEN_A\n") {
		t.Errorf("a rollback emitted no `unset`, so sourcing it leaves the "+
			"revoked value exported:\n%s", body)
	}
}

// SOURCING THE WHOLE STREAM LEAVES THE ENVIRONMENT AS IT STARTED, which is
// the property the `unset` lines exist for and the one a reader cannot check
// by eye on a multi-seat run.
func TestSourcingARolledBackPrintStreamUnsetsEverything(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	sink, err := provision.NewPrintSink(&out)
	if err != nil {
		t.Fatalf("NewPrintSink: %v", err)
	}
	names := []string{"TOKEN_A", "TOKEN_B", "TOKEN_C"}
	for _, name := range names {
		if err := sink.Record(t.Context(), name, "v-"+name); err != nil {
			t.Fatalf("Record %s: %v", name, err)
		}
	}
	if err := sink.Discard(t.Context()); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	// Replay the stream the way a shell would: an `export` sets, an
	// `unset` clears, and a comment does nothing at all.
	env := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "export "):
			name, value, _ := strings.Cut(strings.TrimPrefix(line, "export "), "=")
			env[name] = value
		case strings.HasPrefix(line, "unset "):
			delete(env, strings.TrimPrefix(line, "unset "))
		}
	}
	if len(env) != 0 {
		t.Errorf("sourcing the rolled-back stream leaves %v exported", env)
	}
}

// A SINK WITH NOWHERE TO WRITE IS REFUSED BEFORE ANYTHING IS MINTED.
//
// The failure a nil stream produces is otherwise the worst-timed one there
// is: the run reaches the vendor, mints a live credential, and only then
// discovers it has nothing to print it to — leaving a token that exists in
// GitLab and in no operator's hands.
func TestAPrintSinkWithNoStreamIsRefused(t *testing.T) {
	t.Parallel()
	sink, err := provision.NewPrintSink(nil)
	if err == nil {
		t.Fatal("a sink with no stream was built, so the first minted " +
			"credential would be destroyed on its way out")
	}
	if sink != nil {
		t.Errorf("a refused sink came back usable: %v", sink)
	}
}

// EVERY SINK SAYS WHERE THE VALUES WENT, which is what tells an operator
// whether to go looking in a file or in the store.
func TestEverySinkDescribesItself(t *testing.T) {
	t.Parallel()
	for _, tc := range sinkCases() {
		sink, _ := tc.build(t)
		if strings.TrimSpace(sink.Describe()) == "" {
			t.Errorf("%s describes itself as nothing", tc.name)
		}
	}
	described, err := provision.NewPrintSink(&bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewPrintSink: %v", err)
	}
	if strings.TrimSpace(described.Describe()) == "" {
		t.Error("the print sink describes itself as nothing")
	}
}

// AND SAYS WHAT STILL HAS TO HAPPEN, which is a different question.
//
// The secret store is the trap the pair exists for: it needs no file to
// source, so a report that stopped at Describe read as "you are finished" —
// while the running engine kept resolving from the snapshot it built at its
// last apply, refusing every delivery signed with the new secret.
func TestEverySinkSaysWhatStillHasToHappen(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, tc := range sinkCases() {
		sink, _ := tc.build(t)
		step := strings.TrimSpace(sink.NextStep())
		if step == "" {
			sinkName := tc.name
			t.Errorf("%s says nothing about what still has to happen", sinkName)
			continue
		}
		if other, clash := seen[step]; clash {
			t.Errorf("%s and %s give the same next step, so one of them is "+
				"telling an operator to do the wrong thing: %q", tc.name, other, step)
		}
		seen[step] = tc.name
	}

	store, _ := sinkCases()[0].build(t)
	if got := store.NextStep(); strings.Contains(got, "source") {
		t.Errorf("the secret store's next step tells an operator to source a "+
			"file it never wrote: %q", got)
	} else if !strings.Contains(got, "activate") {
		t.Errorf("the secret store's next step does not name the gesture that "+
			"makes a running engine reload its snapshot: %q", got)
	}
}

// AND IT IS STILL 0600 AFTER A REWRITE. Every Record replaces the file
// through a temp-and-rename, and a rename carries the temp file's mode — so
// a rewrite is exactly where the permissions would silently widen.
func TestTheEnvFileStaysPrivateAcrossRewrites(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".env")
	sink, err := provision.NewEnvFileSink(path)
	if err != nil {
		t.Fatalf("NewEnvFileSink: %v", err)
	}
	for _, name := range []string{"TOKEN_A", "TOKEN_B", "TOKEN_C"} {
		if err = sink.Record(t.Context(), name, "v"); err != nil {
			t.Fatalf("Record(%s): %v", name, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat after %s: %v", name, err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("mode = %o after writing %s, want 600", mode, name)
		}
	}
	// AND THE REWRITE LEAVES NO TEMP FILE BEHIND: each holds the whole
	// credential set, at 0600, in a directory an operator will not think
	// to look in.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the directory holds %v, want only the .env", names)
	}
}
