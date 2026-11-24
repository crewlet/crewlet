package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// hermesPair is a copy of a Nous login holding the token pair "pair-<n>" in
// Hermes's own shape: auth.json's providers.nous, or the shared copy's top
// level.
func hermesPair(name string, n int) string {
	pair := fmt.Sprintf(`{"access_token":"pair-%d","refresh_token":"pair-%d"}`, n, n)
	if name == "auth.json" {
		return `{"providers":{"nous":` + pair + `}}`
	}
	return pair
}

// pairOf is the n of the pair a copy of a Nous login holds, 0 for none.
func pairOf(raw []byte) int {
	var copyOf struct {
		AccessToken string `json:"access_token"`
		Providers   struct {
			Nous struct {
				AccessToken string `json:"access_token"`
			} `json:"nous"`
		} `json:"providers"`
	}
	if json.Unmarshal(raw, &copyOf) != nil {
		return 0
	}
	token := copyOf.AccessToken + copyOf.Providers.Nous.AccessToken
	n, _ := strconv.Atoi(strings.TrimPrefix(token, "pair-"))
	return n
}

// fakeHermesLogin plays Hermes Agent's two copies of a Nous login under
// HERMES_HOME: auth.json and shared/nous_auth.json, each holding "pair-<n>"
// ([hermesPair]), the higher n the later-issued pair. As Hermes does, it takes the shared copy's
// pair into auth.json when that copy holds the later one, and answers with
// the pair it signed in with. Then it rotates: "both" writes the next pair to
// both copies, as a refresh does; "shared" writes it to the shared copy alone,
// as a refresh whose auth.json write failed leaves it; "none" rotates nothing.
func fakeHermesLogin(rotate string) {
	home := os.Getenv("HERMES_HOME")
	auth := filepath.Join(home, "auth.json")
	shared := filepath.Join(home, "shared", "nous_auth.json")
	pair := func(path string) int {
		raw, _ := os.ReadFile(path) //nolint:gosec // the fake's own home
		return pairOf(raw)
	}
	write := func(path string, n int) {
		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err == nil {
			err = os.WriteFile(path, []byte(hermesPair(filepath.Base(path), n)), 0o600)
		}
		if err != nil {
			fmt.Fprint(os.Stderr, "UNWRITABLE: ", err)
			os.Exit(3)
		}
	}
	signedIn := pair(auth)
	if held := pair(shared); held > signedIn {
		signedIn = held
		write(auth, signedIn)
	}
	switch rotate {
	case "both":
		write(auth, signedIn+1)
		write(shared, signedIn+1)
	case "shared":
		write(shared, signedIn+1)
	}
	fmt.Printf("pair-%d", signedIn)
}

// hermesLoginProvider is an entry on dir driving the shipped hermes profile,
// its login files and HERMES_HOME included, with the fake playing its two
// copies of a Nous login.
func hermesLoginProvider(t *testing.T, dir, rotate string) *Provider {
	t.Helper()
	p, err := New(Config{
		Key: "nous", Harness: "hermes", StateDir: dir,
		Overrides: map[string]any{
			"binary":          os.Args[0],
			"complete_args":   []any{"-test.run=TestFakeHarness"},
			"model_args":      []any{},
			"prompt_mode":     "stdin",
			"prompt_args":     []any{},
			"usage_file_args": []any{},
		},
		Timeout: 20 * time.Second, MaxConcurrent: 2,
		Env:  fakeChildEnv(map[string]string{"FAKE_HERMES_LOGIN": rotate}),
		Auth: Auth{Mode: AuthSubscription},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// HERMES'S LOGIN IS ONE LOGIN, ITS TWO COPIES CARRIED AND SEEDED TOGETHER.
// Hermes Agent keeps a Nous login in auth.json and again in
// shared/nous_auth.json, and signs in with whichever holds the later-issued
// pair (untested on 0.23.4: it needs a stand-in Nous portal). A seat home
// seeded with auth.json alone kept the copy its own last refresh wrote, so a
// refresh the engine had not carried out came back: the call signed in with
// it, wrote it into auth.json, and the release carried it over the login of
// record. Seeded with both, the home
// holds the login of record and nothing else, and a refresh through either
// copy is carried out with the other.
//
// Mutation: drop .hermes/shared/nous_auth.json from the hermes profile's
// credential_paths, and the first case signs in with pair-2 and carries it out.
func TestHermesLoginIsKeptAsOne(t *testing.T) {
	t.Parallel()
	seat := llm.SeatOf(t.Context())
	for _, tc := range []struct {
		name, rotate string
		want         string
		auth, shared int
	}{
		{name: "a refresh the engine did not carry", rotate: "none",
			want: "pair-1", auth: 1, shared: 1},
		{name: "a refresh through both copies", rotate: "both",
			want: "pair-1", auth: 2, shared: 2},
		{name: "a refresh through the shared copy alone", rotate: "shared",
			want: "pair-1", auth: 1, shared: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			p := hermesLoginProvider(t, dir, tc.rotate)
			creds := p.ws.CredentialsDir()
			for _, name := range []string{"auth.json", "nous_auth.json"} {
				if err := os.MkdirAll(creds, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(creds, name), []byte(hermesPair(name, 1)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// What an earlier generation's refresh left in the home and the
			// engine never carried out: a later pair in the shared copy.
			left := filepath.Join(p.ws.SeatDir(seat), "home", ".hermes", "shared", "nous_auth.json")
			if err := os.MkdirAll(filepath.Dir(left), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(left, []byte(hermesPair("nous_auth.json", 2)), 0o600); err != nil {
				t.Fatal(err)
			}
			comp, err := ask(t, p, llm.Request{})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if comp.Content != tc.want {
				t.Errorf("the call signed in with %s, want the login of record's %s", comp.Content, tc.want)
			}
			for name, want := range map[string]int{"auth.json": tc.auth, "nous_auth.json": tc.shared} {
				if got, _ := os.ReadFile(filepath.Join(creds, name)); pairOf(got) != want {
					t.Errorf("the login of record's %s holds %q, want pair-%d", name, got, want)
				}
			}
		})
	}
}
