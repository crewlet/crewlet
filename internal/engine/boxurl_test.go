package engine_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A BOX URL THAT IS SET AND UNUSABLE REFUSES THE BOOT before anything is
// opened, naming the variable and never the value (its user information is a
// credential): otherwise every run's endpoints fail silently, inside a box the
// engine never hears from again.
//
// THE CONTROL: plain http to a non-loopback host boots as https does, and a
// run's endpoints and Gitlet's base start with the value as written, trailing
// '/' trimmed: code-sandbox.md ("Security model") makes the scheme the
// operator's call, so a later refusal breaks here rather than silently.
//
// Mutations: read CREWLET_BOX_GIT_URL raw, or outside ReadBoxURLs → red;
// refuse a non-loopback http:// at boot → the control, red.
func TestABadBoxURLRefusesTheBoot(t *testing.T) {
	t.Parallel()
	variables := []string{sandbox.OtelReceiverURLVar, mcpbridge.BaseURLVar, sandbox.BoxGitURLVar}
	// ONE LIST: this test, the boot and `crewlet llm doctor` read the same
	// variables, so the doctor never passes a box URL the boot refuses.
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
					b.Stream.StoreDir = filepath.Join(brokertest.TempDir(t), "stream")
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
				t.Errorf("%s=%s gives boxes Gitlet at %q, want %q", sandbox.BoxGitURLVar, tc.git,
					got, want)
			}

			receiver := e.OtelReceiver()
			if receiver == nil {
				t.Fatalf("%s=%s built no receiver", sandbox.OtelReceiverURLVar, tc.value)
			}
			if got, err := receiver.EndpointFor("4bf92f3577b34da6a3ce929d0e0e4736", false); err != nil ||
				!strings.HasPrefix(got, want+"/otlp/") {
				t.Errorf("a run's OTLP endpoint is %q (%v), want it under %q", got, err, want+"/otlp/")
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

// A RUN'S TELEMETRY TOKEN LIVES THE RECEIVER'S FIXED LIFETIME: the receiver
// the engine builds mints for [sandbox.OtelTokenTTL], the only bound its
// tokens have. The token's expiry is read off the token itself, bracketed by
// the instants read before and after its mint.
//
// Mutation: build the engine's receiver with any other TTL, and the expiry
// misses it.
func TestARunsTelemetryTokenLivesTheReceiversFixedLifetime(t *testing.T) {
	t.Parallel()
	const base = "https://engine.example.com"
	e := newEngine(t, engine.Options{
		Environment: config.MapSource{sandbox.OtelReceiverURLVar: base}})
	before := time.Now()
	endpoint, err := e.OtelReceiver().EndpointFor("4bf92f3577b34da6a3ce929d0e0e4736", false)
	after := time.Now()
	if err != nil || !strings.HasPrefix(endpoint, base+"/otlp/") {
		t.Fatalf("a run's OTLP endpoint is %q (%v)", endpoint, err)
	}
	// A run token is version.key.subject.expiry.signature, its expiry in
	// Unix seconds (internal/runtoken).
	fields := strings.Split(strings.TrimPrefix(endpoint, base+"/otlp/"), ".")
	if len(fields) != 5 {
		t.Fatalf("the token has %d fields, want five", len(fields))
	}
	expiry, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		t.Fatalf("the token's expiry %q: %v", fields[3], err)
	}
	if expiry < before.Add(sandbox.OtelTokenTTL).Unix() || expiry > after.Add(sandbox.OtelTokenTTL).Unix() {
		t.Errorf("the token expires at %d, want %v after its mint", expiry, sandbox.OtelTokenTTL)
	}
}
