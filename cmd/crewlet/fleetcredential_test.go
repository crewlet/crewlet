package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nkeys"

	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// credentialKeys are the two keys a credential case's Tier A holds, the second
// active: what is written must be the ACTIVE key's operator nkey.
var credentialKeys = [2][]byte{bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)}

// credentialTierA is a Tier A for node-a — the node the engine's committed
// broker answer names — holding [credentialKeys], serving its API at
// serverURL, or none when serverURL is empty.
func credentialTierA(t *testing.T, serverURL string) (path string, material runtoken.Material) {
	t.Helper()
	dir := t.TempDir()
	api := ""
	if serverURL != "" {
		host, port, err := net.SplitHostPort(strings.TrimPrefix(serverURL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		api = fmt.Sprintf("api:\n  host: %s\n  port: %s\n  external_url: \"http://%s:%s\"\n"+
			"  auth:\n    admin: serve\n    tokens:\n"+
			"      - id: ops\n        token: %s\n        grants: [admin]\n",
			host, port, host, port, cliFixtureToken)
	}
	k1, k2 := secrets.EncodeKey(credentialKeys[0]), secrets.EncodeKey(credentialKeys[1])
	body := fmt.Sprintf("node:\n  id: node-a\nstore:\n  path: %s\n%s%s"+
		"secrets:\n  active_key_id: k2\n  keys:\n"+
		"    - {id: k1, material: \"%s\"}\n    - {id: k2, material: \"%s\"}\n",
		filepath.Join(dir, "index.db"), api, fixtureStream(dir), k1, k2)
	path = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, runtoken.Material{ActiveID: "k2", Keys: []runtoken.KeyMaterial{
		{ID: "k1", Material: k1}, {ID: "k2", Material: k2}}}
}

// operatorPublic is the public operator nkey key derives: the 32 bytes the
// keyring's rule derives under the client listener's domain, as an nkeys user.
func operatorPublic(t *testing.T, key runtoken.KeyMaterial) string {
	t.Helper()
	kp, err := nkeys.FromRawSeed(nkeys.PrefixByteUser,
		runtoken.DeriveKey(jetstream.ClientKeyDomain, key.ID, key.Material))
	if err != nil {
		t.Fatal(err)
	}
	public, err := kp.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return public
}

// brokerAnswerServer serves GET /fleet/broker with the engine's committed
// answer, changed by edit, and counts the requests it was sent.
func brokerAnswerServer(t *testing.T, status int, edit func(map[string]any)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "internal", "api",
		"testdata", "fleet_broker_answer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(answer)
	}
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path != "/fleet/broker" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(answer)
		} else {
			_, _ = w.Write([]byte(`{"error":"unauthenticated","code":"unauthenticated"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

// THE CREDENTIAL IS THE ACTIVE KEY'S OPERATOR KEY, WRITTEN WHERE ONLY ITS
// OWNER READS IT, AND POINTED AT THIS HOST'S OWN LISTENER.
//
// The file holds the seed whose public key is the operator key the broker
// derives from the ACTIVE key, created 0600; the seed never reaches the
// terminal; the invocation names the listener this host's node answered with.
// An existing file is refused and left as it was. A node that is not this
// host's — a -url naming a balancer or a peer — is refused before anything is
// written, since a loopback listener is its own host's; a node with no client
// listener says why; a node that serves no API is asked nothing and points at
// its broker_keys line; and a 401 names the variable the token comes from.
//
// Mutations: derive under the leaf domain → the key row, red; write the seed to
// stdout → the terminal row, red; overwrite the path → the existing-file row,
// red; trust another node's listener → its row, red; send an api-less node to
// the API → its row, red.
func TestTheBrokerCredentialIsTheActiveKeysOperatorKey(t *testing.T) {
	t.Parallel()
	t.Run("the active key's operator key, 0600, never on the terminal", func(t *testing.T) {
		t.Parallel()
		srv, _ := brokerAnswerServer(t, http.StatusOK, nil)
		cfg, material := credentialTierA(t, srv.URL)
		file := filepath.Join(t.TempDir(), "operator.nk")
		out, stderr, err := cli(t, "fleet", "broker", "credential", cfg, "-o", file)
		if err != nil {
			t.Fatalf("credential: %v\n%s", err, stderr)
		}
		seed, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		kp, err := nkeys.FromSeed(bytes.TrimSpace(seed))
		if err != nil {
			t.Fatalf("the file holds no nkey seed: %v", err)
		}
		public, err := kp.PublicKey()
		if err != nil {
			t.Fatal(err)
		}
		// THE OPERATOR DOMAIN, derived here on its own rather than read back
		// through OperatorCredential, so a credential derived under another
		// listener's domain — a leaf key the member's client listener refuses —
		// is a red case and not the same answer twice.
		want := operatorPublic(t, material.Keys[1])
		if _, offered, id, err := jetstream.OperatorCredential(material); err != nil ||
			offered != want || id != "k2" {
			t.Fatalf("OperatorCredential offers %s for %q (%v), want the active key k2's "+
				"operator key %s", offered, id, err, want)
		}
		if public != want {
			t.Errorf("the file holds key %s, want the active key k2's operator key %s", public, want)
		}
		if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the credential file's mode is %v, want 0600 (%v)", info.Mode().Perm(), err)
		}
		if strings.Contains(out+stderr, strings.TrimSpace(string(seed))) {
			t.Error("the seed reached the terminal")
		}
		for _, says := range []string{"nats --server nats://127.0.0.1:41234 --nkey " + file,
			`secrets.keys id "k2"`} {
			if !strings.Contains(out, says) {
				t.Errorf("the output does not say %q:\n%s", says, out)
			}
		}
	})

	t.Run("an existing file is never overwritten", func(t *testing.T) {
		t.Parallel()
		srv, _ := brokerAnswerServer(t, http.StatusOK, nil)
		cfg, _ := credentialTierA(t, srv.URL)
		file := filepath.Join(t.TempDir(), "operator.nk")
		if err := os.WriteFile(file, []byte("somebody's own file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := cli(t, "fleet", "broker", "credential", cfg, "-o", file); err == nil ||
			!strings.Contains(err.Error(), "never overwritten") {
			t.Fatalf("writing over an existing file answered %v", err)
		}
		if got, _ := os.ReadFile(file); string(got) != "somebody's own file\n" {
			t.Errorf("the existing file now holds %q", got)
		}
	})

	t.Run("another node's answer is refused and nothing is written", func(t *testing.T) {
		t.Parallel()
		srv, _ := brokerAnswerServer(t, http.StatusOK, func(a map[string]any) { a["node"] = "node-b" })
		cfg, _ := credentialTierA(t, srv.URL)
		file := filepath.Join(t.TempDir(), "operator.nk")
		_, _, err := cli(t, "fleet", "broker", "credential", cfg, "-o", file)
		if err == nil {
			t.Fatal("a listener another node answered with was printed")
		}
		for _, says := range []string{`"node-b"`, `"node-a"`, "node.id", "CREWLET_NODE_ID"} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("the refusal does not say %q: %v", says, err)
			}
		}
		if _, statErr := os.Stat(file); !os.IsNotExist(statErr) {
			t.Errorf("a refused credential left a file behind (%v)", statErr)
		}
	})

	t.Run("a node with no client listener says why", func(t *testing.T) {
		t.Parallel()
		srv, _ := brokerAnswerServer(t, http.StatusOK, func(a map[string]any) { delete(a, "client") })
		cfg, _ := credentialTierA(t, srv.URL)
		file := filepath.Join(t.TempDir(), "operator.nk")
		_, _, err := cli(t, "fleet", "broker", "credential", cfg, "-o", file)
		if err == nil || !strings.Contains(err.Error(), "no client listener") ||
			!strings.Contains(err.Error(), "clustered member's host") {
			t.Fatalf("a node with no client listener answered %v", err)
		}
		if _, statErr := os.Stat(file); !os.IsNotExist(statErr) {
			t.Errorf("a refused credential left a file behind (%v)", statErr)
		}
	})

	// ASKED NOTHING, which is what lets it answer at all: the API address a
	// node serving none would be asked at is refused (api.port is 0), and
	// any -url names another node, which the check above refuses.
	t.Run("a node serving no API is asked nothing", func(t *testing.T) {
		t.Parallel()
		cfg, material := credentialTierA(t, "")
		file := filepath.Join(t.TempDir(), "operator.nk")
		out, stderr, err := cli(t, "fleet", "broker", "credential", cfg, "-o", file)
		if err != nil {
			t.Fatalf("an api-less member's credential: %v\n%s", err, stderr)
		}
		seed, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		kp, err := nkeys.FromSeed(bytes.TrimSpace(seed))
		if err != nil {
			t.Fatal(err)
		}
		public, _ := kp.PublicKey()
		if want := operatorPublic(t, material.Keys[1]); public != want {
			t.Errorf("the api-less member's file holds %s, want %s", public, want)
		}
		for _, says := range []string{"nats --server nats://<client listener>", "broker_keys"} {
			if !strings.Contains(out, says) {
				t.Errorf("the output does not say %q:\n%s", says, out)
			}
		}
	})

	t.Run("a token the node refuses names where it comes from", func(t *testing.T) {
		t.Parallel()
		srv, _ := brokerAnswerServer(t, http.StatusUnauthorized, nil)
		cfg, _ := credentialTierA(t, srv.URL)
		_, _, err := cli(t, "fleet", "broker", "credential", cfg, "-o",
			filepath.Join(t.TempDir(), "operator.nk"))
		if err == nil || !strings.Contains(err.Error(), apiTokenEnv) {
			t.Fatalf("a 401 answered %v, want it to name %s", err, apiTokenEnv)
		}
	})
}
