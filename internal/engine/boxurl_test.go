package engine_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A BOX URL THAT IS SET AND UNUSABLE REFUSES THE BOOT, naming the variable,
// before anything is opened.
//
// The URLs a box dials back to the engine were handed on as they were: a
// telemetry receiver URL with no scheme minted every run an endpoint its
// exporter could not use, and a bridge URL with none mounted a bridge whose
// every minted endpoint failed, so every executor's tool calls failed —
// each silently, inside a box the engine never hears from again; and the code
// host's base is every run's clone and push. And the refusal never repeats
// the value: its user information is a credential.
//
// THE CONTROL holds the other half of the grammar: plain http to a host that
// is not loopback — the box's own host, or a name on the network — boots
// exactly as https does, and every endpoint minted for a run, and the code
// host's base a run is handed, starts with the value as the operator wrote
// it, its trailing '/' trimmed. The docs (code-sandbox.md, "Security model")
// promise that a box URL's scheme is the operator's call and say what plain
// http costs; a refusal added later would break that promise here rather than
// silently.
//
// Mutations: read CREWLET_BOX_GIT_URL raw rather than through ParseBoxURL, or
// outside ReadBoxURLs → its refused rows boot, or the one-list assertion
// fails, red; refuse a non-loopback http:// at boot → the control, red.
func TestABadBoxURLRefusesTheBoot(t *testing.T) {
	t.Parallel()
	variables := []string{sandbox.OtelReceiverURLVar, mcpbridge.BaseURLVar, sandbox.BoxGitURLVar}
	// ONE LIST: what this test boots on is what ReadBoxURLs reads, which is
	// also what `crewlet llm doctor` reports — so a box URL the boot refuses
	// is never one the doctor passes.
	var listed []string
	for _, url := range engine.ReadBoxURLs(func(string) string { return "" }).All() {
		listed = append(listed, url.Variable)
	}
	if !slices.Equal(listed, variables) {
		t.Errorf("ReadBoxURLs reads %v, want the box URLs this test boots on, %v", listed, variables)
	}
	for _, variable := range variables {
		for _, tc := range []struct{ name, value string }{
			{"no scheme", "engine.example.com:80"},
			{"user information", "https://ops:hunter2@engine.example.com"},
		} {
			t.Run(variable+" with "+tc.name, func(t *testing.T) {
				t.Parallel()
				// UNSEEDED: that nothing is at the store's path
				// afterwards is half the assertion.
				boot, _ := unseededBootstrap(t, func(b *config.Bootstrap) {
					b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
				})
				e, err := engine.New(t.Context(), engine.Options{
					Bootstrap:   boot,
					Company:     parsedCompany(t, companyDoc),
					Environment: config.MapSource{variable: tc.value},
				})
				if err == nil {
					e.Stop(context.Background())
					t.Fatalf("%s=%s booted: every box would be handed an endpoint it "+
						"cannot use", variable, tc.value)
				}
				if !strings.Contains(err.Error(), variable) {
					t.Errorf("the refusal does not name %s: %v", variable, err)
				}
				if strings.Contains(err.Error(), "hunter2") {
					t.Errorf("the refusal repeats the URL's password: %v", err)
				}
				for _, path := range []string{boot.Store.Path, boot.Stream.StoreDir} {
					if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
						t.Errorf("%s exists after the refusal (%v): the engine opened "+
							"something before it read the box URLs", path, statErr)
					}
				}
			})
		}
	}

	for _, tc := range []struct{ name, value, git string }{
		{"https", "https://engine.example.com", "https://git.example.com/box/git"},
		{"plain http to a name on the network", "http://engine.example.com", "http://git.example.com/box/git"},
		{"plain http to the box's host, with a path", "http://172.17.0.1:8080/base/",
			"http://172.17.0.1:8080/box/git/"},
	} {
		t.Run("the control: "+tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEngine(t, engine.Options{Environment: config.MapSource{
				sandbox.OtelReceiverURLVar: tc.value,
				mcpbridge.BaseURLVar:       tc.value,
				sandbox.BoxGitURLVar:       tc.git,
			}})
			want := strings.TrimRight(tc.value, "/")
			if got, want := engine.BoxGitURLOf(e), strings.TrimRight(tc.git, "/"); got != want {
				t.Errorf("%s=%s gives boxes the code host at %q, want %q", sandbox.BoxGitURLVar, tc.git,
					got, want)
			}

			receiver := e.OtelReceiver()
			if receiver == nil {
				t.Fatalf("%s=%s built no receiver", sandbox.OtelReceiverURLVar, tc.value)
			}
			if got := receiver.EndpointFor("4bf92f3577b34da6a3ce929d0e0e4736", false, time.Minute); !strings.HasPrefix(got, want+"/otlp/") {
				t.Errorf("a run's OTLP endpoint is %q, want it under %q", got, want+"/otlp/")
			}

			bridge := e.Bridge()
			if bridge == nil {
				t.Fatalf("%s=%s built no bridge", mcpbridge.BaseURLVar, tc.value)
			}
			// THE BASE EVERY RUN'S ENDPOINT IS MINTED UNDER, for a box that is
			// not on this node's own host: the value as the operator wrote it.
			if got := bridge.Reaches(false); got != want {
				t.Errorf("a run's bridge endpoints are minted under %q, want %q", got, want)
			}
		})
	}
}
