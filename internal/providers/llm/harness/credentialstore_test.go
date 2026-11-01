package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// fakeStore is a CLI that keeps its logins, its sessions and each session's
// usage in a DATABASE OF ITS OWN under XDG_DATA_HOME — OpenCode 2.0's shape —
// rather than in a file the engine could copy. Its verbs are words on argv:
//
//   - store-export prints every login, `[]` when it holds none;
//   - store-import reads an export on stdin and stores every login in it,
//     SKIPPING an id it already holds, as 2.0 does;
//   - store-login stores the login FAKE_LOGIN_KEY names;
//   - store-list prints the logins it holds;
//   - store-usage <session> prints that session's usage report;
//   - store-device-login plays a device-code sign-in ([fakeDeviceLogin]);
//   - store-status plays a status command ([fakeStatus]);
//   - store-file-login writes a login FILE, $HOME/.fake/creds.json, holding
//     FAKE_LOGIN_KEY — a CLI that keeps its login in a file, for the cases
//     about one;
//   - store-two-file-login writes ONE LOGIN IN TWO FILES, $HOME/.fake/auth.json
//     and $HOME/.fake/config.toml, each naming FAKE_LOGIN_KEY — kimi-code's
//     and gemini-cli's shape, for the cases about a login that is a set;
//   - store-token-login writes only $HOME/.fake/auth.json, naming
//     FAKE_LOGIN_KEY — a sign-in of that two-file shape that renews its token
//     and leaves the other file as it found it;
//   - store-token-only-login writes $HOME/.fake/auth.json as store-token-login
//     does and REMOVES $HOME/.fake/config.toml — a sign-in of that shape that
//     renews its token and drops the other file;
//   - anything else is a call: it answers with the key it holds, records the
//     usage FAKE_SESSION_USAGE names under the session `--session` named, and
//     then refreshes its login to FAKE_STORE_REFRESH, or drops every login on
//     FAKE_STORE_DROP.
//
// And three ways to fail or to race: FAKE_STORE_IMPORT_REJECT refuses every
// document, quoting it whole, and a document that does not parse is refused
// quoting only its last [fakeQuotedTail] bytes, as a parser's error quotes the
// context it stopped in; FAKE_STORE_EXPORT_FAIL fails every export after
// printing its logins (FAKE_STORE_EXPORT_FAIL_ONCE only the first, marking
// the file it names; FAKE_STORE_EXPORT_FAIL_KEY every one made while the
// database holds a login with that key), quoting them on stderr too under
// FAKE_STORE_EXPORT_QUOTE;
// and FAKE_MIDRUN_VERB names a verb during which FAKE_MIDRUN_WRITES ([midrun])
// is written, each file in path order — another writer's login landing while
// this command is running, with the operator epoch it moved where it moved one
// — after which the verb waits for the file FAKE_MIDRUN_AWAIT names to exist,
// where it names one: another writer's hold the case stands up while this
// command is running, a written file telling the case the command has started
// and the awaited one telling the command the hold stands.
func fakeStore() {
	data := filepath.Join(os.Getenv("XDG_DATA_HOME"), "fake")
	db := filepath.Join(data, "db.json")
	read := func() []map[string]string {
		var creds []map[string]string
		if raw, err := os.ReadFile(db); err == nil {
			_ = json.Unmarshal(raw, &creds)
		}
		return creds
	}
	write := func(creds []map[string]string) {
		if creds == nil {
			creds = []map[string]string{}
		}
		raw, _ := json.Marshal(creds)
		_ = os.MkdirAll(data, 0o700)
		_ = os.WriteFile(db, raw, 0o600)
	}
	verb, arg := "", ""
	for i, a := range os.Args {
		if strings.HasPrefix(a, "store-") {
			verb = a
			if i+1 < len(os.Args) {
				arg = os.Args[i+1]
			}
		}
	}
	// Every command unpacks itself into TMPDIR first, as a Bun runtime does,
	// so a TMPDIR pointed somewhere it should not be is somewhere written.
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		_ = os.MkdirAll(tmp, 0o700)
	}
	if v := os.Getenv("FAKE_MIDRUN_VERB"); v != "" && v == verb {
		var writes map[string]string
		if err := json.Unmarshal([]byte(os.Getenv("FAKE_MIDRUN_WRITES")), &writes); err != nil {
			fmt.Fprintf(os.Stderr, "FAKE_MIDRUN_WRITES is not a JSON object of paths: %v", err)
			os.Exit(2)
		}
		for _, path := range slices.Sorted(maps.Keys(writes)) {
			_ = os.WriteFile(path, []byte(writes[path]), 0o600)
		}
		if awaited := os.Getenv("FAKE_MIDRUN_AWAIT"); awaited != "" {
			// Bounded, so a case that never lets it go fails on its own
			// deadline rather than leaving a process behind it.
			deadline := time.Now().Add(fakeAwaitBound)
			for _, err := os.Stat(awaited); err != nil; _, err = os.Stat(awaited) {
				if !time.Now().Before(deadline) {
					fmt.Fprintf(os.Stderr, "FAKE_MIDRUN_AWAIT: %s never appeared", awaited)
					os.Exit(2)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
	switch verb {
	case "store-export":
		failOnce := os.Getenv("FAKE_STORE_EXPORT_FAIL_ONCE")
		failing := os.Getenv("FAKE_STORE_EXPORT_FAIL") == "1"
		if failOnce != "" {
			if _, err := os.Stat(failOnce); os.IsNotExist(err) {
				_ = os.WriteFile(failOnce, nil, 0o600)
				failing = true
			}
		}
		if key := os.Getenv("FAKE_STORE_EXPORT_FAIL_KEY"); key != "" &&
			slices.ContainsFunc(read(), func(c map[string]string) bool { return c["key"] == key }) {
			failing = true
		}
		if failing {
			// A CLI that printed its logins and THEN failed: the half
			// on stdout is every secret it holds.
			printed := `[{"id":"cred_1","key":"LEAKED-SECRET"}]`
			if creds := read(); len(creds) > 0 {
				raw, _ := json.Marshal(creds)
				printed = string(raw)
			}
			fmt.Print(printed)
			if os.Getenv("FAKE_STORE_EXPORT_QUOTE") == "1" {
				fmt.Fprintf(os.Stderr, "export broke after writing: %s", printed)
			} else {
				fmt.Fprint(os.Stderr, "export broke")
			}
			os.Exit(1)
		}
		raw, _ := json.Marshal(read())
		if len(read()) == 0 {
			raw = []byte("[]")
		}
		fmt.Println(string(raw))
	case "store-import":
		doc, _ := io.ReadAll(os.Stdin)
		var in []map[string]string
		if os.Getenv("FAKE_STORE_IMPORT_REJECT") == "1" {
			// Quoting what it rejected, as a CLI may.
			fmt.Fprintf(os.Stderr, "invalid document: %s", doc)
			os.Exit(1)
		}
		if err := json.Unmarshal(doc, &in); err != nil {
			// Quoting a WINDOW of it, as a parser does: a window cuts
			// whatever it falls across, a credential included.
			fmt.Fprintf(os.Stderr, "invalid document: unexpected end near %q",
				doc[max(0, len(doc)-fakeQuotedTail):])
			os.Exit(1)
		}
		creds := read()
		for _, c := range in {
			if !slices.ContainsFunc(creds, func(have map[string]string) bool { return have["id"] == c["id"] }) {
				creds = append(creds, c)
			}
		}
		write(creds)
		fmt.Printf("Imported %d\n", len(in))
	case "store-login":
		write(append(read(), map[string]string{"id": "cred_login", "key": os.Getenv("FAKE_LOGIN_KEY")}))
	case "store-file-login":
		file := filepath.Join(os.Getenv("HOME"), ".fake", "creds.json")
		_ = os.MkdirAll(filepath.Dir(file), 0o700)
		_ = os.WriteFile(file, []byte(`{"token":"`+os.Getenv("FAKE_LOGIN_KEY")+`"}`), 0o600)
	case "store-two-file-login":
		dir := filepath.Join(os.Getenv("HOME"), ".fake")
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"token":"`+os.Getenv("FAKE_LOGIN_KEY")+`"}`), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`provider = "`+os.Getenv("FAKE_LOGIN_KEY")+`"`), 0o600)
	case "store-token-login", "store-token-only-login":
		file := filepath.Join(os.Getenv("HOME"), ".fake", "auth.json")
		_ = os.MkdirAll(filepath.Dir(file), 0o700)
		_ = os.WriteFile(file, []byte(`{"token":"`+os.Getenv("FAKE_LOGIN_KEY")+`"}`), 0o600)
		if verb == "store-token-only-login" {
			_ = os.Remove(filepath.Join(filepath.Dir(file), "config.toml"))
		}
	case "store-device-login":
		fakeDeviceLogin()
	case "store-status":
		fakeStatus()
	case "store-list":
		for _, c := range read() {
			fmt.Println(c["id"])
		}
	case "store-usage":
		raw, err := os.ReadFile(filepath.Join(data, "sessions", arg+".json"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "no session %q", arg)
			os.Exit(1)
		}
		fmt.Printf(`{"data":{"tokens":%s}}`, raw)
	default:
		key := ""
		if creds := read(); len(creds) > 0 {
			key = creds[0]["key"]
		}
		for i, a := range os.Args {
			if a == "--session" && i+1 < len(os.Args) {
				usage := os.Getenv("FAKE_SESSION_USAGE")
				if usage == "" {
					usage = `{"input":1,"output":1}`
				}
				_ = os.MkdirAll(filepath.Join(data, "sessions"), 0o700)
				_ = os.WriteFile(filepath.Join(data, "sessions", os.Args[i+1]+".json"), []byte(usage), 0o600)
			}
		}
		switch {
		case os.Getenv("FAKE_STORE_REFRESH") != "":
			write([]map[string]string{{"id": "cred_1", "key": os.Getenv("FAKE_STORE_REFRESH")}})
		case os.Getenv("FAKE_STORE_DROP") == "1":
			write(nil)
		}
		fmt.Print("key=" + key)
	}
}

// fakeQuotedTail is how much of a document [fakeStore] quotes when it cannot
// parse it.
const fakeQuotedTail = 40

// fakeAwaitBound is how long [fakeStore] waits for FAKE_MIDRUN_AWAIT's file: far
// past the moment any case creates it, which is as soon as it has seen the
// command start and taken a lock nobody else holds.
const fakeAwaitBound = time.Minute

// storeProvider builds a provider whose CLI is [fakeStore].
func storeProvider(t *testing.T, env map[string]string, extra map[string]any) *Provider {
	t.Helper()
	merged := map[string]string{"FAKE_STORE": "1"}
	for k, v := range env {
		merged[k] = v
	}
	run := "-test.run=TestFakeHarness"
	overrides := map[string]any{
		"credential_store": map[string]any{
			"export_args": []any{run, "store-export"},
			"import_args": []any{run, "store-import"},
		},
		"volatile_paths": []any{".local/share/fake"},
		"login_args":     []any{run, "store-login"},
		"status_args":    []any{run, "store-list"},
	}
	for k, v := range extra {
		overrides[k] = v
	}
	return fakeProvider(t, merged, overrides)
}

// writeDocument puts a login document in the shared credential directory, as
// `crewlet llm login` would have left it.
func writeDocument(t *testing.T, p *Provider, body string) {
	t.Helper()
	if err := os.MkdirAll(p.ws.CredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ws.documentPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readDocument is the shared login document, or "" when there is none.
func readDocument(t *testing.T, p *Provider) string {
	t.Helper()
	raw, err := os.ReadFile(p.ws.documentPath())
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A SEAT STARTS EVERY GENERATION FROM THE SHARED LOGIN, imported into an empty
// database, and leaves nothing of that database behind: the database is the
// transcript as well as the login, so a copy of it would be one seat's
// conversations in every other seat.
func TestASeatStartsEveryGenerationFromTheSharedLogin(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, nil, nil)
	writeDocument(t, p, `[{"id":"cred_1","key":"k-shared"}]`)

	for range 2 {
		comp, err := ask(t, p, llm.Request{})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if comp.Content != "key=k-shared" {
			t.Fatalf("the call held %q, want the shared login imported into its home", comp.Content)
		}
	}
	// The seat the calls RAN in, as Complete names it. This looked in
	// SeatDir(""), a directory no call ever uses, so the absence below held
	// for any build.
	if _, err := os.Stat(seatHome(t, p)); err != nil {
		t.Fatalf("the calls did not run in %q: %v", seatHome(t, p), err)
	}
	if _, err := os.Stat(seatDatabase(t, p)); !os.IsNotExist(err) {
		t.Errorf("the seat's database outlived its generation (%v) — the next "+
			"generation would start from this one's transcript", err)
	}
}

// seatHome is the home of the seat [ask]'s calls run in — the one
// [llm.SeatOf] names for a context that names none.
func seatHome(t *testing.T, p *Provider) string {
	t.Helper()
	return filepath.Join(p.ws.SeatDir(llm.SeatOf(t.Context())), "home")
}

// seatDatabase is where [fakeStore] keeps the database of that seat's CLI.
func seatDatabase(t *testing.T, p *Provider) string {
	t.Helper()
	return filepath.Join(seatHome(t, p), ".local", "share", "fake")
}

// A LOGIN THE CALL REFRESHED IS WRITTEN BACK — exported from the database and
// put in place of the shared document — or the fleet is logged out the next
// time the old refresh token is presented.
func TestALoginTheCallRefreshedIsWrittenBack(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{"FAKE_STORE_REFRESH": "k-new"}, nil)
	writeDocument(t, p, `[{"id":"cred_1","key":"k-old"}]`)
	if _, err := ask(t, p, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-new") {
		t.Errorf("the shared login is %q, want the one the call refreshed", got)
	}
}

// A SEAT NEVER CREATES A LOGIN. Nothing in the shared directory means nobody
// logged this entry in — or somebody logged it out — and a login a seat's
// call happened to leave in its database is not the operator's to inherit.
func TestASeatNeverCreatesALogin(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{"FAKE_STORE_REFRESH": "k-minted"}, nil)
	if _, err := ask(t, p, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := readDocument(t, p); got != "" {
		t.Errorf("a seat wrote a login into an empty shared directory: %q", got)
	}
}

// A DATABASE HOLDING NO LOGIN NEVER REPLACES ONE. A CLI that dropped a
// credential it could not refresh exports nothing, and the shared login is the
// better one to keep — exactly as a credential file a call deleted is not
// deleted from the shared directory.
func TestADatabaseWithNoLoginNeverReplacesTheSharedOne(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{"FAKE_STORE_DROP": "1"}, nil)
	writeDocument(t, p, `[{"id":"cred_1","key":"k-kept"}]`)
	if _, err := ask(t, p, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-kept") {
		t.Errorf("the shared login is %q, want it kept over an export of nothing", got)
	}
}

// AN IMPORT THAT FAILS FAILS THE CALL, naming the remedy — a seat run with no
// login while the shared directory holds one would fail with a vendor's "not
// authenticated" that points at nothing — and never repeats the document: a
// CLI that rejects one may quote it, and it is a credential.
//
// The fake quotes the WHOLE document, and the failure carries none of it: not
// the secret, which the redaction rules would have caught anyway, and not the
// words around it, which only the scrub of what the import was handed does.
func TestAnImportThatFailsFailsTheCallWithoutRepeatingTheDocument(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{"FAKE_STORE_IMPORT_REJECT": "1"}, nil)
	secret := "sk-ant-" + strings.Repeat("Q", 40)
	document := "not a document " + secret
	writeDocument(t, p, document)
	_, err := ask(t, p, llm.Request{})
	if err == nil {
		t.Fatal("a call whose login could not be imported ran anyway")
	}
	if !strings.Contains(err.Error(), "crewlet llm login") {
		t.Errorf("the failure does not name the remedy: %v", err)
	}
	for _, repeated := range []string{secret, "not a document"} {
		if strings.Contains(err.Error(), repeated) {
			t.Errorf("the failure repeats the document (%q): %v", repeated, err)
		}
	}
}

// AN EXPORT THAT FAILS NEVER REPORTS WHAT IT PRINTED. Its stdout is every
// secret the CLI holds, so the error is the command, its exit and its stderr.
func TestAFailedExportNeverRepeatsWhatItPrinted(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{"FAKE_STORE_EXPORT_FAIL": "1"}, nil)
	home := t.TempDir()
	_, err := p.exportLogin(t.Context(), home, t.TempDir())
	if err == nil {
		t.Fatal("a failed export was read as a login")
	}
	if strings.Contains(err.Error(), "LEAKED-SECRET") {
		t.Errorf("the failure repeats the export's stdout: %v", err)
	}
	if !strings.Contains(err.Error(), "export broke") {
		t.Errorf("the failure drops the CLI's own stderr: %v", err)
	}
}

// THE LOGIN HOME IS WHERE A LOGIN IS MADE, and the only home whose export may
// create the shared document. It is rebuilt from that document for every
// command and left with no copy of the database afterwards, so `status` reads
// the login every seat reads, and logout removes it.
func TestALoginIsMadeInTheLoginHomeAndRemovedByLogout(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-login"}, nil)
	var out, errOut bytes.Buffer
	if err := p.BrokerLogin(t.Context(), nil, &out, &errOut); err != nil {
		t.Fatalf("BrokerLogin: %v (%s)", err, errOut.String())
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-login") {
		t.Fatalf("the shared login is %q after a login", got)
	}
	data := filepath.Join(p.ws.Root(), "login-home", ".local", "share", "fake")
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Errorf("the login home kept its database (%v): a second copy of the login "+
			"beside the document of record", err)
	}

	out.Reset()
	if err := p.Status(t.Context(), &out, &errOut); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(out.String(), "cred_login") {
		t.Errorf("status did not see the shared login: %q", out.String())
	}

	if err := p.Logout(t.Context(), &out, &errOut); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if hasLogin(t, p.ws) || readDocument(t, p) != "" {
		t.Error("the login survived a logout")
	}
}

// A LOGIN REPLACES A SHARED LOGIN THE CLI REFUSES TO IMPORT. The refusal sends
// the operator to `crewlet llm login`, and the login began by importing the very
// document that was refused, stopped at the same refusal and never started the
// vendor's sign-in — so the one remedy named was a loop, with the fleet signed
// out. A bundle exported by another build of the CLI, whose shapes this one
// decodes strictly, is how a document gets here. A status still refuses: it
// makes no login to put in the document's place.
//
// Mutation: return the seed's refusal from runInCredentialHome whatever the
// mode, and the login is refused here.
func TestALoginReplacesASharedLoginTheHarnessRefusesToImport(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-login"}, nil)
	damaged := `[{"id":"cred_1","key":"k-old"`
	writeDocument(t, p, damaged)

	_, err := ask(t, p, llm.Request{})
	if kind := llm.KindOf(err); kind != llm.KindAuth {
		t.Fatalf("a seat seeded from a refused document = %v (kind %v), want an auth failure", err, kind)
	}
	for _, way := range []string{"`crewlet llm login sub`", "`crewlet llm login sub -from-host`", "`crewlet llm logout sub`"} {
		if !strings.Contains(err.Error(), way) {
			t.Errorf("the refusal does not name %s: %v", way, err)
		}
	}
	err = p.Status(t.Context(), &bytes.Buffer{}, &bytes.Buffer{})
	var refused *loginRefusedError
	if !errors.As(err, &refused) {
		t.Errorf("Status over a refused document = %v, want the refusal", err)
	}
	if got := readDocument(t, p); got != damaged {
		t.Fatalf("a status changed the refused document: %q", got)
	}

	var errOut bytes.Buffer
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &errOut); err != nil {
		t.Fatalf("BrokerLogin over a refused document: %v (%s)", err, errOut.String())
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-login") || strings.Contains(got, "k-old") {
		t.Errorf("the login did not replace the refused document: %q", got)
	}
	if !strings.Contains(errOut.String(), "signing in afresh replaces it") {
		t.Errorf("the login replaced the shared document without saying so: %q", errOut.String())
	}
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("a seat after the login: %v", err)
	}
	if comp.Content != "key=k-login" {
		t.Errorf("a seat after the login held %q, want the new login", comp.Content)
	}
}

// A BUNDLE CARRIES THE DOCUMENT, so an engine rebuilt on every deploy comes up
// signed in to a CLI that keeps its login in its own database too.
func TestABundleCarriesTheDocument(t *testing.T) {
	t.Parallel()
	source := storeProvider(t, nil, nil)
	writeDocument(t, source, `[{"id":"cred_1","key":"k-bundled"}]`)
	blob, err := source.ExportBundle(t.Context())
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	target := storeProvider(t, nil, nil)
	if err := target.RestoreBundle(t.Context(), blob); err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	if got := readDocument(t, target); got != `[{"id":"cred_1","key":"k-bundled"}]` {
		t.Errorf("the restored login is %q", got)
	}
}

// A HUMAN'S LOGIN IS EXPORTED, NOT COPIED: the CLI reads their home as it
// would for them, and nothing is left behind in it — a call's TMPDIR would
// point inside the home it is given.
func TestAHostLoginIsExportedFromTheHumansHome(t *testing.T) {
	t.Parallel()
	p := storeProvider(t, nil, nil)
	home := t.TempDir()
	if got := p.HostLogin(t.Context(), home); len(got) != 0 {
		t.Errorf("an empty home reported a login: %v", got)
	}
	hostDB := filepath.Join(home, ".local", "share", "fake", "db.json")
	if err := os.MkdirAll(filepath.Dir(hostDB), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostDB, []byte(`[{"id":"cred_host","key":"k-host"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := p.HostLogin(t.Context(), home); len(got) != 1 {
		t.Errorf("HostLogin = %v, want the one login the home holds", got)
	}
	taken, err := p.AdoptHostLogin(t.Context(), home)
	if err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}
	if len(taken) != 1 || !strings.Contains(taken[0], home) {
		t.Errorf("taken = %v, want the home it was read from", taken)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-host") {
		t.Errorf("the adopted login is %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, "tmp")); !os.IsNotExist(err) {
		t.Errorf("reading a human's login left a tmp directory in their home (%v)", err)
	}
}

// THE USAGE REPORT IS THE CALL'S OWN: the session the call ran in, asked about
// by the id the engine gave it, and its reasoning tokens — reported apart from
// output — charged as output.
func TestTheUsageReportIsAskedAboutTheCallsOwnSession(t *testing.T) {
	t.Parallel()
	run := "-test.run=TestFakeHarness"
	usage := map[string]any{
		"session_args": []any{"--session", "ses_{session}"},
		"usage_args":   []any{run, "store-usage", "ses_{session}"},
		"usage": map[string]any{
			"input":       []any{[]any{"data", "tokens", "input"}},
			"output":      []any{[]any{"data", "tokens", "output"}},
			"cache_read":  []any{[]any{"data", "tokens", "cache", "read"}},
			"cache_write": []any{[]any{"data", "tokens", "cache", "write"}},
			"reasoning":   []any{[]any{"data", "tokens", "reasoning"}},
		},
	}
	p := storeProvider(t, map[string]string{
		"FAKE_SESSION_USAGE": `{"input":120,"output":30,"reasoning":45,"cache":{"read":800,"write":5}}`,
	}, usage)
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.InputTokens != 120+800+5 {
		t.Errorf("InputTokens = %d, want the whole prompt count, cache included (925)", comp.InputTokens)
	}
	if comp.OutputTokens != 30+45 {
		t.Errorf("OutputTokens = %d, want the visible output plus the reasoning (75)", comp.OutputTokens)
	}
	if comp.CacheRead != 800 || comp.CacheWrite != 5 {
		t.Errorf("cache = %d/%d, want 800/5", comp.CacheRead, comp.CacheWrite)
	}
}

// A USAGE REPORT THAT CANNOT BE HAD IS NOT A FAILED CALL: the counts fall back
// to an estimate and the answer the operator paid for is kept.
func TestAMissingUsageReportFallsBackToTheEstimate(t *testing.T) {
	t.Parallel()
	run := "-test.run=TestFakeHarness"
	p := storeProvider(t, nil, map[string]any{
		"session_args": []any{"--session", "ses_{session}"},
		// Asked about a session the call did not run in, so the report
		// fails.
		"usage_args": []any{run, "store-usage", "other_{session}"},
		"usage": map[string]any{
			"input":  []any{[]any{"data", "tokens", "input"}},
			"output": []any{[]any{"data", "tokens", "output"}},
		},
	})
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("a failed usage report failed the call: %v", err)
	}
	if comp.Content == "" || comp.InputTokens == 0 || comp.OutputTokens == 0 {
		t.Errorf("comp = %+v, want the answer with estimated counts", comp)
	}
}

// stubStore is a [loginStore] over a value, for the workspace's own rules.
type stubStore struct{ held, exported []byte }

func (s *stubStore) importLogin(_ context.Context, _, _ string, document []byte) error {
	s.held = append([]byte(nil), document...)
	return nil
}

func (s *stubStore) exportLogin(context.Context, string, string) ([]byte, error) {
	return s.exported, nil
}

func (s *stubStore) providerKey() string { return "stub" }

// A LOGOUT WHILE A SEAT IS MID-CALL IS NOT UNDONE BY THE SEAT. The shared
// login disappearing is `crewlet llm logout` having run, and a seat writing
// its copy back put a login the operator had just removed back on disk — for
// a credential file as much as a document.
func TestALogoutDuringACallIsNotUndoneByTheSeat(t *testing.T) {
	t.Parallel()
	profile := testProfile(t)
	profile.CredentialStore = &harnessprofile.CredentialStore{
		ExportArgs: []string{"export"}, ImportArgs: []string{"import"},
	}
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "fake-store", profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws.CredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(ws.CredentialsDir(), "creds.json")
	for path, body := range map[string]string{file: `{"token":"t"}`, ws.documentPath(): `[{"id":"c"}]`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := &stubStore{}
	checkout, err := ws.Acquire(t.Context(), "dev", "call", store)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if string(store.held) != `[{"id":"c"}]` {
		t.Fatalf("the seat imported %q, want the shared document", store.held)
	}

	// The operator logs out mid-call; the CLI refreshes both kinds.
	for _, path := range loginOnDisk(t, ws) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	seeded := filepath.Join(checkout.Home, ".fake", "creds.json")
	if err := os.WriteFile(seeded, []byte(`{"token":"refreshed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store.exported = []byte(`[{"id":"c","refreshed":true}]`)

	if err := checkout.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := loginOnDisk(t, ws); len(got) != 0 {
		t.Errorf("the seat put the login back after a logout: %v", got)
	}
}

// midrun is FAKE_MIDRUN_WRITES for writes, content by path: what [fakeStore]
// writes during the verb FAKE_MIDRUN_VERB names.
func midrun(t *testing.T, writes map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(writes)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
