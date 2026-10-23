package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/whsec"
)

// storedSigningSecret is the GitLab signing secret the fleet holds in these
// cases: a well-formed whsec_ value over 32 bytes, which the plan reuses.
const storedSigningSecret = "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzISE="

// THE FIXTURE IS THE SHAPE ITS COMMENT CLAIMS. Nothing on the paths these
// cases drive checks the shape of a value the store hands back, so a fixture
// one byte short passed every one of them — and would break them all, for a
// reason the comment denies, the day a reused secret's shape is checked.
func TestTheStoredSigningSecretIsAWellFormedKey(t *testing.T) {
	t.Parallel()
	if !whsec.Valid(storedSigningSecret) {
		t.Fatalf("storedSigningSecret is not a whsec_ value over %d bytes", whsec.KeyBytes)
	}
}

// revealNode is a running node's /secrets surface as a CLI run resolving a
// company document meets it: POST /secrets/reveal answers the values it holds
// of the names asked, and every other route answers what its status says.
type revealNode struct {
	server *httptest.Server
	held   map[string]string

	mu       sync.Mutex
	asked    [][]string
	auth     []string
	requests []string
	refuse   int
}

func newRevealNode(t *testing.T, held map[string]string) *revealNode {
	t.Helper()
	n := &revealNode{held: held}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.requests = append(n.requests, r.Method+" "+r.URL.Path)
		n.auth = append(n.auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if n.refuse != 0 {
			w.WriteHeader(n.refuse)
			_, _ = io.WriteString(w, `{"error":"step_up_required"}`)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/secrets/reveal":
			var body struct {
				Names []string `json:"names"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n.asked = append(n.asked, body.Names)
			values, missing := map[string]string{}, []string{}
			for _, name := range body.Names {
				if v, ok := n.held[name]; ok {
					values[name] = v
				} else {
					missing = append(missing, name)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"values": values, "missing": missing})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/secrets/"):
			name := strings.TrimPrefix(r.URL.Path, "/secrets/")
			if v, ok := n.held[name]; ok {
				_ = json.NewEncoder(w).Encode(map[string]string{"name": name, "value": v})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not_found"}`)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/secrets/"):
			// THE BODY IS THE VALUE, as the route takes it.
			value, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if n.held == nil {
				n.held = map[string]string{}
			}
			name := strings.TrimPrefix(r.URL.Path, "/secrets/")
			n.held[name] = string(value)
			_ = json.NewEncoder(w).Encode(map[string]string{"name": name,
				"updated_by": "cli-fixture", "updated_by_kind": "operator"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no_route"}`)
		}
	}))
	t.Cleanup(n.server.Close)
	return n
}

// holds is the value the node holds under name, and whether it holds one.
func (n *revealNode) holds(name string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	value, ok := n.held[name]
	return value, ok
}

// seen is what the node was asked, request by request.
func (n *revealNode) seen() (asked [][]string, requests, auth []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.asked), slices.Clone(n.requests), slices.Clone(n.auth)
}

// gitlabDocVariables is every variable [gitlabCompanyDoc] reads: its three
// references, and no convention, since its one model names its key.
var gitlabDocVariables = []string{"ANTHROPIC_API_KEY", "GITLAB_SIGNING_SECRET", "GITLAB_TOKEN_SWE"}

// A RUN ON A NODE WHOSE ENGINE IS UP READS THE FLEET THROUGH IT.
//
// It used to open the node's own database for its values while the sink
// beside it routed: so against a running node — the documented case — every
// provisioning command died on the store's lock before it planned anything,
// `-dry-run` included, and the documented "-secret-store against a running
// node" was a gesture nobody could make. Now the lock routes the whole run
// through the node's API, and the values come from the fleet's store in ONE
// request for exactly the variables the document reads.
func TestARunOnARunningNodeReadsTheFleetThroughIt(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"GITLAB_SIGNING_SECRET": storedSigningSecret})
	cfg := bootstrapForURL(t, node.server.URL)
	seedTheStore(t, cfg)
	holdTheStore(t, storePathOf(t, cfg))

	out, errs, err := cli(t, "gitlab", "provision", gitlabCompanyFile(t), "-dry-run",
		"-config", cfg, "-public-url", "https://crewlet.example.com")
	if err != nil {
		t.Fatalf("a dry run on a running node failed: %v\n%s", err, errs)
	}
	if !strings.Contains(out, "webhook signing secret: reused") {
		t.Errorf("the plan did not reuse the fleet's signing secret:\n%s", out)
	}
	if strings.Contains(out, "own secret rows") {
		t.Errorf("a run that read the fleet said it read one node's rows:\n%s", out)
	}
	asked, _, auth := node.seen()
	if len(asked) != 1 {
		t.Fatalf("the node was asked %d time(s), want one question for the set: %v",
			len(asked), asked)
	}
	if got := slices.Sorted(slices.Values(asked[0])); !slices.Equal(got, gitlabDocVariables) {
		t.Errorf("the run asked for %v, want exactly the document's variables %v",
			got, gitlabDocVariables)
	}
	if !slices.Contains(auth, "Bearer "+cliFixtureToken) {
		t.Errorf("the run did not authenticate as the operator's own token: %v", auth)
	}
}

// A READ THE NODE REFUSES FAILS THE RUN, and the environment never fills in.
//
// Resolving the refused names from this shell instead would hand the run
// whatever stale export the store exists to outrank — and for the signing
// secret, an export that is not the fleet's key is one the hook gets
// re-pointed at.
func TestARefusedReadThroughTheNodeFailsTheRun(t *testing.T) {
	// NOT PARALLEL: it sets a process variable the resolver would read.
	t.Setenv("GITLAB_SIGNING_SECRET", "whsec_ZTJlLXRlc3Qtc2lnbmluZy1rZXktMzItYnl0ZXMhISE=")
	node := newRevealNode(t, nil)
	node.refuse = http.StatusForbidden
	cfg := bootstrapForURL(t, node.server.URL)
	seedTheStore(t, cfg)
	holdTheStore(t, storePathOf(t, cfg))

	out, _, err := cli(t, "gitlab", "provision", gitlabCompanyFile(t), "-dry-run",
		"-config", cfg, "-public-url", "https://crewlet.example.com")
	if err == nil {
		t.Fatalf("a refused read resolved the run anyway:\n%s", out)
	}
	if !strings.Contains(err.Error(), "Nothing was resolved from the environment") {
		t.Errorf("the refusal does not say the environment was not used: %v", err)
	}
	if strings.Contains(out, "webhook signing secret") {
		t.Errorf("a plan was printed from values the node refused:\n%s", out)
	}
}

// `-api` FROM A MACHINE THAT IS NOT THE NODE reads the node, and creates no
// database here.
//
// The resolver used to ignore the flag and open the store Tier A names on
// THIS machine — creating an empty database there, which nothing ever reads,
// and resolving every value from it as nothing.
func TestAnAPINamedFromAnotherMachineIsReadAndNoStoreIsCreated(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"GITLAB_SIGNING_SECRET": storedSigningSecret})
	cfg := bootstrapWithKeyring(t, "k1")
	db := storePathOf(t, cfg)

	for _, config := range []string{cfg, filepath.Join(t.TempDir(), "absent.yaml")} {
		out, errs, err := cli(t, "gitlab", "provision", gitlabCompanyFile(t), "-dry-run",
			"-config", config, "-api", node.server.URL,
			"-public-url", "https://crewlet.example.com")
		if err != nil {
			t.Fatalf("-config %s: %v\n%s", config, err, errs)
		}
		if !strings.Contains(out, "webhook signing secret: reused") {
			t.Errorf("-config %s: the plan did not reuse the node's value:\n%s", config, out)
		}
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a database was created at %s by a run that read another node: %v", db, err)
	}
}

// A STOPPED NODE THAT HAS BOOTED DOES NOT MINT OVER WHAT THE FLEET HOLDS.
//
// Every start moves this node's own rows onto the fleet and deletes them here,
// so after one the table holds nothing the fleet does not — and the run used
// to read that empty table as "no signing secret", which is the signal to
// mint: the plan promised a fresh key, and a real run re-pointed a working
// hook at it. An absence this node's rows cannot vouch for is UNKNOWN now: the
// plan says so, a real run refuses before anything at GitLab is touched, and
// the refusal says how to reach the fleet. An explicit -rotate is a decision
// rather than a reason, and is planned as one.
func TestAStoppedNodeThatHasBootedDoesNotMintOverTheFleets(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_SIGNING_SECRET",
		"-value", storedSigningSecret); err != nil {
		t.Fatalf("seed: %v (%s)", err, errs)
	}
	bootOnto(t, cfg)
	company := gitlabCompanyFile(t)

	out, errs, err := provisionCmd(t, company, "-dry-run", "-config", cfg,
		"-public-url", "https://crewlet.example.com")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, errs)
	}
	if strings.Contains(out, "WILL BE MINTED") {
		t.Errorf("the plan promised to mint over a secret the fleet holds:\n%s", out)
	}
	for _, want := range []string{
		"webhook signing secret: UNKNOWN", // what the plan knows
		"own secret rows",                 // which store it read
		"-api",                            // how to read the fleet's
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run omits %q:\n%s", want, out)
		}
	}

	out, _, err = provisionCmd(t, company, "-print", "-config", cfg,
		"-public-url", "https://crewlet.example.com")
	if !errors.Is(err, provision.ErrUnvouched) {
		t.Fatalf("a real run minted over an unknown secret: %v\n%s", err, out)
	}
	for _, want := range []string{"GITLAB_SIGNING_SECRET", "crewlet run", "-api"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits %q: %v", want, err)
		}
	}
	if strings.Contains(out, "export ") {
		t.Errorf("a refused run printed a credential:\n%s", out)
	}

	out, errs, err = provisionCmd(t, company, "-dry-run", "-config", cfg,
		"-public-url", "https://crewlet.example.com", "-rotate")
	if err != nil {
		t.Fatalf("dry run with -rotate: %v\n%s", err, errs)
	}
	if !strings.Contains(out, "WILL BE ROTATED into GITLAB_SIGNING_SECRET") {
		t.Errorf("an explicit -rotate was not planned as one:\n%s", out)
	}
}

// NOR DOES IT REGISTER WHAT THIS SHELL EXPORTS IN THE FLEET'S PLACE.
//
// The environment still fills the resolver in behind this node's own rows, as
// the engine's own environment does behind the store. But the store outranks
// it there: what this shell exports for a name the rows lack is a guess at the
// fleet's value, and a guess that is not empty used to read as the store's own
// answer — the plan said "reused", and a real run registered it, re-pointing
// a working hook at a key no node verifies with. It is settled like an empty
// answer now: the sink's record where it holds one, and refused where it does
// not.
func TestAStoppedNodeDoesNotRegisterWhatTheShellExports(t *testing.T) {
	// NOT PARALLEL: it sets the process variables the resolver falls back
	// to and the administrator token is read from.
	const exported = "whsec_ZTJlLXRlc3Qtc2lnbmluZy1rZXktMzItYnl0ZXMhISE="
	t.Setenv("GITLAB_SIGNING_SECRET", exported)
	t.Setenv("GITLAB_ADMIN_TOKEN", "")
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_SIGNING_SECRET",
		"-value", storedSigningSecret); err != nil {
		t.Fatalf("seed: %v (%s)", err, errs)
	}
	bootOnto(t, cfg)
	company := gitlabCompanyFile(t)
	public := []string{"-config", cfg, "-public-url", "https://crewlet.example.com"}

	out, errs, err := provisionCmd(t, append([]string{company, "-dry-run"}, public...)...)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, errs)
	}
	if strings.Contains(out, "webhook signing secret: reused") ||
		!strings.Contains(out, "webhook signing secret: UNKNOWN") {
		t.Errorf("the plan reused what this shell exports as the fleet's key:\n%s", out)
	}

	out, _, err = provisionCmd(t, append([]string{company, "-print"}, public...)...)
	if !errors.Is(err, provision.ErrUnvouched) {
		t.Fatalf("a real run registered what this shell exports: %v\n%s", err, out)
	}

	// THE SINK'S OWN RECORD ANSWERS, as it does for an empty answer: the
	// file a previous run recorded into is what this run would register.
	// Past the signing secret the run needs an administrator token, which
	// this case withholds — so that refusal is the proof the check passed.
	envFile := filepath.Join(t.TempDir(), ".env.gitlab")
	if err := os.WriteFile(envFile, []byte("GITLAB_SIGNING_SECRET="+exported+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = provisionCmd(t, append([]string{company, "-env-file", envFile}, public...)...)
	if err == nil || errors.Is(err, provision.ErrUnvouched) ||
		!strings.Contains(err.Error(), "no administrator token") {
		t.Errorf("a key the sink records was refused as unknown: %v", err)
	}
}

// -secret-store ON A STOPPED NODE ANSWERS A MISS AS UNKNOWN.
//
// Every seat's keep-or-mint check reads the sink back, and "not held" is the
// reason to mint the seat a new token and revoke the one it has. On a stopped
// node the sink is that node's own table, whose miss says nothing about the
// fleet — so there it is the error a store that cannot say gives, which every
// such check refuses on. Through a node, a miss is the fleet's own answer and
// stays one, and a name either store holds is held on both.
func TestASecretStoreSinkOnAStoppedNodeAnswersAMissAsUnknown(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_TOKEN_SWE",
		"-value", "glpat-held"); err != nil {
		t.Fatalf("seed: %v (%s)", err, errs)
	}
	node := newRevealNode(t, map[string]string{"GITLAB_TOKEN_SWE": "glpat-held"})

	for _, tc := range []struct {
		name        string
		args        []string
		missUnknown bool
	}{
		{"this node's own table", []string{"-secret-store", "-config", cfg}, true},
		{"the fleet's, through a node",
			[]string{"-secret-store", "-config", cfg, "-api", node.server.URL}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("provision", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			sinks := addSinkFlags(fs, sinkDecisions{seatTokens: true})
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			at, err := openCompanyStore(tokenEnviron(cliFixtureToken), sinks.store,
				"crewlet gitlab provision")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(at.Close)
			sink, err := sinks.open(t.Context(), at, io.Discard)
			if err != nil {
				t.Fatal(err)
			}

			value, held, err := sink.Value(t.Context(), "GITLAB_TOKEN_SWE")
			if err != nil || !held || value != "glpat-held" {
				t.Errorf("a held name = (%q, %v, %v), want it held", value, held, err)
			}
			_, held, err = sink.Value(t.Context(), "GITLAB_TOKEN_PM")
			switch {
			case tc.missUnknown && !errors.Is(err, provision.ErrUnvouched):
				t.Errorf("a miss on a stopped node = (%v, %v), want it unknown", held, err)
			case !tc.missUnknown && (err != nil || held):
				t.Errorf("a miss through the node = (%v, %v), want not held", held, err)
			}
		})
	}
}

// A STOPPED NODE'S NOTE CLAIMS NO MORE THAN THE RUN DOES.
//
// The note a run on a stopped node prints used to say nothing was minted into
// a variable this node's own rows lack. That held for a webhook or signing
// secret, and for a seat's token under -secret-store, whose sink reads the
// same rows back. But -print and -env-file decide a seat's token from what
// they hold, on this route as on every other: here the fleet holds the bot's
// token, the node has booted (so its rows hold nothing), and a run into a sink
// that lacks it mints the bot a new token and revokes the one the fleet
// holds — under a line that promised nothing would be minted. The note now
// says what becomes of each decision, and each run below is held to it.
//
// And it says so only of the decisions the command takes: GitHub, Jira and
// Confluence mint no seat a token, and Mattermost has no webhook secret, so a
// note reciting both told a GitHub run that -print would mint a token for a
// seat it lacked. A command that records through no sink takes none of them,
// so its note makes no claim about any.
//
// Nor of a decision this run does not refuse: the command's own flag for a
// fresh value (-rotate, -recreate-webhooks, -recreate-webhook) mints without
// asking what anything holds, so a run that passed it was told nothing would
// be minted into a variable these rows lack, and then minted into one. Its
// note says the flag goes ahead instead — and with -secret-store, the one
// sink whose read the stopped route refuses, the run is held to both halves.
// What it says goes ahead for a webhook or signing secret is a mint into the
// whole ${VAR} that names it: a literal is the document's own, which these
// rows have no say in, and a GitLab -rotate registers it as it is — under a
// note that used to promise a fresh secret for whatever the run registered,
// right above a plan saying "reused".
func TestAStoppedNodesNoteClaimsNoMoreThanTheRunDoes(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "MM_TOKEN_CEO",
		"-value", "mmtok-the-fleets"); err != nil {
		t.Fatalf("seed: %v (%s)", err, errs)
	}
	bootOnto(t, cfg)

	for _, tc := range []struct {
		name   string
		sink   func(t *testing.T) []string
		minted func(t *testing.T, out string, sink []string) string
	}{
		{"-print", func(*testing.T) []string { return []string{"-print"} },
			func(_ *testing.T, out string, _ []string) string { return out }},
		{"-env-file", func(t *testing.T) []string {
			return []string{"-env-file", filepath.Join(t.TempDir(), "fresh.env")}
		}, func(t *testing.T, _ string, sink []string) string {
			recorded, err := os.ReadFile(sink[1])
			if err != nil {
				t.Fatalf("the env file the run recorded into: %v", err)
			}
			return string(recorded)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chat := newBotHost(t)
			company := writeFile(t, t.TempDir(), "company.yaml",
				strings.Replace(chatCompanyDoc, "url: https://chat.example.com",
					"url: "+chat.server.URL, 1))
			sink := tc.sink(t)
			out, errs, err := cliIn(t, noEnviron, append(append([]string{"mattermost",
				"provision", company}, sink...), "-admin-token", botHostAdmin,
				"-config", cfg)...)
			if err != nil {
				t.Fatalf("provision: %v\n%s%s", err, out, errs)
			}

			// WHAT THE RUN DID: the sink lacked the bot's token, so a
			// fresh one was minted into it and the fleet's was revoked.
			if got := tc.minted(t, out, sink); !strings.Contains(got, "MM_TOKEN_CEO=") ||
				!strings.Contains(got, botHostMinted) {
				t.Fatalf("the run did not mint the bot a token into its sink:\n%s", got)
			}
			if revoked := chat.revokedIDs(); !slices.Equal(revoked, []string{botHostFleetToken}) {
				t.Errorf("revoked %v, want the token the fleet holds (%s)",
					revoked, botHostFleetToken)
			}

			// AND WHAT ITS NOTE SAID about the same run.
			if !strings.Contains(out, "own secret rows") {
				t.Fatalf("a run on a stopped node did not say which store it read:\n%s", out)
			}
			if strings.Contains(out, "Nothing is minted into one") {
				t.Errorf("the note promised nothing would be minted, and the run "+
					"minted:\n%s", out)
			}
			if !strings.Contains(out, noteSeatTokensSinks) {
				t.Errorf("the note does not say the sink decides a seat's "+
					"token:\n%s", out)
			}
		})
	}

	t.Run("-secret-store refuses, and -rotate goes ahead", func(t *testing.T) {
		t.Parallel()
		// A NODE OF ITS OWN: this run records into the rows the other
		// cases only read.
		cfg := bootstrapWithKeyring(t, "k1")
		if _, errs, err := secretsCmd(t, cfg, "set", "MM_TOKEN_CEO",
			"-value", "mmtok-the-fleets"); err != nil {
			t.Fatalf("seed: %v (%s)", err, errs)
		}
		bootOnto(t, cfg)
		chat := newBotHost(t)
		company := writeFile(t, t.TempDir(), "company.yaml",
			strings.Replace(chatCompanyDoc, "url: https://chat.example.com",
				"url: "+chat.server.URL, 1))
		run := func(extra ...string) (string, error) {
			out, errs, err := cliIn(t, noEnviron, append(append([]string{"mattermost",
				"provision", company, "-secret-store"}, extra...), "-admin-token",
				botHostAdmin, "-config", cfg)...)
			return out + errs, err
		}

		// WITHOUT THE FLAG: the bot exists, these rows lack its token,
		// and the note says that seat is handed no new one — so the run
		// refuses rather than revoke the token the fleet holds.
		out, err := run()
		if !errors.Is(err, provision.ErrUnvouched) {
			t.Fatalf("a run whose sink cannot vouch for the bot's token = %v, "+
				"want it refused as unknown\n%s", err, out)
		}
		if !strings.Contains(out, noteSeatTokensSinks) {
			t.Errorf("the refusing run's note does not say it refuses:\n%s", out)
		}
		if revoked := chat.revokedIDs(); len(revoked) != 0 {
			t.Fatalf("the refused run revoked %v", revoked)
		}

		// WITH IT: the note says -rotate goes ahead, and it does.
		out, err = run("-rotate")
		if err != nil {
			t.Fatalf("provision -rotate: %v\n%s", err, out)
		}
		ahead := freshGoesAhead("rotate") + noteSeatTokensFresh + "."
		if !strings.Contains(out, ahead) {
			t.Errorf("the note does not say -rotate goes ahead, %q:\n%s", ahead, out)
		}
		if strings.Contains(out, noteSeatTokensSinks) {
			t.Errorf("the note says a seat is handed no new token, and the "+
				"run minted one:\n%s", out)
		}
		if got, errs, err := secretsCmd(t, cfg, "get", "MM_TOKEN_CEO", "-reveal"); err != nil ||
			strings.TrimSpace(got) != botHostMinted {
			t.Errorf("this node's rows hold %q (%v, %s), want the token the run "+
				"minted", got, err, errs)
		}
		if revoked := chat.revokedIDs(); !slices.Equal(revoked, []string{botHostFleetToken}) {
			t.Errorf("revoked %v, want the token the fleet holds (%s)",
				revoked, botHostFleetToken)
		}
	})

	t.Run("each command names only the decisions it takes", func(t *testing.T) {
		t.Parallel()
		// EVERY RUN STOPS SHORT OF ITS VENDOR: the credential it would
		// reach one with names a variable nothing holds, or a Mattermost
		// that nothing listens on. The note is printed before either.
		mattermost := strings.Replace(chatCompanyDoc, "url: https://chat.example.com",
			"url: http://127.0.0.1:1", 1)
		for _, tc := range []struct {
			command         string
			doc             string
			args            []string
			secrets, tokens bool
			// fresh is the command's flag for a fresh value when the
			// case passes it.
			fresh string
			// plan is the line a GitLab dry run prints for the signing
			// secret — what the run itself would do with it, beside
			// what the note says it does.
			plan string
			// literal: the document's webhook or signing secret is a
			// literal rather than a whole ${VAR}.
			literal bool
		}{
			{"gitlab", gitlabCompanyDoc, []string{"-dry-run", "-public-url",
				"https://engine.example.com"}, true, true, "",
				"webhook signing secret: UNKNOWN", false},
			{"gitlab", gitlabCompanyDoc, []string{"-dry-run", "-rotate", "-public-url",
				"https://engine.example.com"}, true, true, "rotate",
				"webhook signing secret: WILL BE ROTATED into GITLAB_SIGNING_SECRET", false},
			// A LITERAL IS THE DOCUMENT'S OWN, which these rows have no
			// say in, so -rotate registers it as it is: a note promising
			// a fresh secret for whatever the run registers sat right
			// above this plan. And the plan says so itself — it printed
			// a plain "reused", so a dry run with -rotate never said the
			// flag would leave this secret in place.
			{"gitlab", strings.Replace(gitlabCompanyDoc, `"${GITLAB_SIGNING_SECRET}"`,
				`"`+noteCaseLiteralSigningSecret+`"`, 1), []string{"-dry-run", "-rotate",
				"-public-url", "https://engine.example.com"}, true, true, "rotate",
				"webhook signing secret: reused — -rotate leaves the webhook signing " +
					"secret alone", true},
			{"github", noteCaseGitHubDoc, []string{"-dry-run"}, true, false, "", "", false},
			{"github", noteCaseGitHubDoc, []string{"-dry-run", "-recreate-webhooks"},
				true, false, "recreate-webhooks", "", false},
			{"jira", noteCaseJiraDoc, []string{"-dry-run"}, true, false, "", "", false},
			{"jira", noteCaseJiraDoc, []string{"-dry-run", "-recreate-webhook"},
				true, false, "recreate-webhook", "", false},
			{"confluence", noteCaseConfluenceDoc, []string{"-dry-run"}, true, false, "", "", false},
			{"confluence", noteCaseConfluenceDoc, []string{"-dry-run", "-recreate-webhooks"},
				true, false, "recreate-webhooks", "", false},
			// A Mattermost dry run reads no value, so this one is real.
			{"mattermost", mattermost, []string{"-print"}, false, true, "", "", false},
			{"mattermost", mattermost, []string{"-print", "-rotate"}, false, true, "rotate", "", false},
		} {
			name := strings.Join(append([]string{tc.command}, tc.args...), " ")
			if tc.literal {
				name += " (a literal secret)"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				company := writeFile(t, t.TempDir(), "company.yaml", tc.doc)
				out, _, _ := cliIn(t, noEnviron, append(append([]string{tc.command,
					"provision", company}, tc.args...), "-config", cfg)...)
				if !strings.Contains(out, "own secret rows") {
					t.Fatalf("a run on a stopped node did not say which store it "+
						"read:\n%s", out)
				}
				if !strings.Contains(out, tc.plan) {
					t.Errorf("the plan does not say %q:\n%s", tc.plan, out)
				}
				refuses := tc.fresh == ""
				for _, sentence := range []struct {
					text  string
					takes bool
				}{
					{noteSecretsRefused, tc.secrets && refuses},
					{noteSeatTokensSinks, tc.tokens && refuses},
					{noteSecretsFresh, tc.secrets && !refuses},
					{noteSeatTokensFresh, tc.tokens && !refuses},
					// The flag this run passed, by name, and no lead at
					// all for a run that passed none.
					{freshGoesAhead(tc.fresh), !refuses},
					{"is a decision rather than a reason", !refuses},
				} {
					switch said := strings.Contains(out, sentence.text); {
					case sentence.takes && !said:
						t.Errorf("the note omits what the run does, %q:\n%s",
							sentence.text, out)
					case !sentence.takes && said:
						t.Errorf("the note speaks of what the run does not do, "+
							"%q:\n%s", sentence.text, out)
					}
				}
				if tc.literal {
					// The flag's clause about a secret, read off the
					// note as printed: the plan above registers the
					// literal as it is, so a clause that mints for
					// whatever the run registers claims a mint into
					// it. Scoped to a whole ${VAR}, it cannot.
					lead := freshGoesAhead(tc.fresh)
					_, after, ok := strings.Cut(out, lead)
					clause, _, _ := strings.Cut(after, ";")
					if !ok || !strings.Contains(clause, "minted afresh") {
						t.Fatalf("no clause after %q says what becomes of the "+
							"secret:\n%s", lead, out)
					}
					if !strings.Contains(clause, "whole ${VAR}") {
						t.Errorf("the note claims a fresh secret for a literal "+
							"the run reuses, %q:\n%s", clause, out)
					}
				}
			})
		}
	})

	t.Run("a command with no sink", func(t *testing.T) {
		t.Parallel()
		_, errs, err := cliIn(t, noEnviron, "llm", "list", "-company",
			cliAgentCompanyFile(t), "-config", cfg)
		if err != nil {
			t.Fatalf("llm list: %v\n%s", err, errs)
		}
		if !strings.Contains(errs, "own secret rows") {
			t.Fatalf("a run on a stopped node did not say which store it read:\n%s", errs)
		}
		for _, claim := range []string{"minted", "-print", "-secret-store"} {
			if strings.Contains(errs, claim) {
				t.Errorf("the note of a command that records nothing says %q:\n%s",
					claim, errs)
			}
		}
	})
}

// noteCaseLiteralSigningSecret is the literal GitLab signing secret a case of
// [TestAStoppedNodesNoteClaimsNoMoreThanTheRunDoes] writes into the document:
// a placeholder of the 32 bytes a whsec_ value has to carry, since config
// validation holds a literal to the shape.
const noteCaseLiteralSigningSecret = "whsec_bGl0ZXJhbC1zaWduaW5nLXNlY3JldC0zMi1ieXRlcyE="

// The company documents of [TestAStoppedNodesNoteClaimsNoMoreThanTheRunDoes]:
// one integration each, whose org credential names a variable nothing holds,
// so each run stops before it reaches the vendor.
const (
	noteCaseGitHubDoc = `
name: Nimbus
integrations:
  github:
    enabled: true
    url: https://github.example.com
    webhook_secret: "${CREWLET_NOTE_CASE_GITHUB_WEBHOOK_SECRET}"
    token: "${CREWLET_NOTE_CASE_GITHUB_TOKEN}"
    provisioning:
      org: nimbus
roles:
  - name: SWE
    handle: swe
    goal: ship
`
	noteCaseJiraDoc = `
name: Nimbus
integrations:
  jira:
    url: https://jira.example.com
    token: "${CREWLET_NOTE_CASE_JIRA_TOKEN}"
    webhook_secret: "${CREWLET_NOTE_CASE_JIRA_WEBHOOK_SECRET}"
roles:
  - name: SWE
    handle: swe
    goal: ship
`
	noteCaseConfluenceDoc = `
name: Nimbus
integrations:
  confluence:
    url: https://confluence.example.com
    email: bot@example.com
    token: "${CREWLET_NOTE_CASE_CONFLUENCE_TOKEN}"
    webhook_secret: "${CREWLET_NOTE_CASE_CONFLUENCE_WEBHOOK_SECRET}"
roles:
  - name: SWE
    handle: swe
    goal: ship
`
)

// The bot [newBotHost] holds, and the tokens it knows of it: the one the fleet
// holds, which a run that does not hold it retires, and the one the run mints.
const (
	botHostAdmin      = "mm-admin-token"
	botHostFleetToken = "tok-the-fleets"
	botHostMinted     = "mmtok-minted-by-this-run"
)

// botHost is a Mattermost serving the one bot [chatCompanyDoc]'s seat names,
// already in its team and its channels, with a token this tool minted before —
// the token a fleet holds for it. It answers what `crewlet mattermost
// provision` asks of a bot that exists, and remembers what it was told to
// revoke.
type botHost struct {
	server *httptest.Server

	mu      sync.Mutex
	revoked []string
}

func newBotHost(t *testing.T) *botHost {
	t.Helper()
	h := &botHost{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		reply := func(body any) { _ = json.NewEncoder(w).Encode(body) }
		path := strings.TrimPrefix(r.URL.Path, "/api/v4")
		switch {
		case r.Method == http.MethodGet && path == "/teams/name/nimbus":
			reply(map[string]string{"id": "team-1", "name": "nimbus"})
		case r.Method == http.MethodGet && path == "/users/me":
			reply(map[string]string{"id": "admin", "username": "root",
				"roles": "system_user system_admin"})
		case r.Method == http.MethodGet && path == "/config/client":
			reply(map[string]string{})
		case r.Method == http.MethodGet && path == "/bots":
			reply([]map[string]string{{"user_id": "user-ceo", "username": "agent-ceo",
				"display_name": "CEO"}})
		case r.Method == http.MethodGet && path == "/users/username/agent-ceo":
			reply(map[string]string{"id": "user-ceo", "username": "agent-ceo"})
		case r.Method == http.MethodGet && path == "/teams/team-1/members/user-ceo":
			reply(map[string]string{"team_id": "team-1", "user_id": "user-ceo"})
		case r.Method == http.MethodGet && path == "/users/user-ceo/teams/team-1/channels":
			reply([]map[string]string{
				{"id": "ch-general", "name": "general", "team_id": "team-1"},
				{"id": "ch-lead", "name": "leadership", "team_id": "team-1"},
			})
		case r.Method == http.MethodPost && path == "/users/user-ceo/tokens":
			reply(map[string]string{"id": "tok-minted", "token": botHostMinted,
				"description": "crewlet-ceo"})
		case r.Method == http.MethodGet && path == "/users/user-ceo/tokens":
			reply([]map[string]string{
				{"id": botHostFleetToken, "description": "crewlet-ceo"},
				{"id": "tok-minted", "description": "crewlet-ceo"},
			})
		case r.Method == http.MethodPost && path == "/users/tokens/revoke":
			var body struct {
				ID string `json:"token_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.revoked = append(h.revoked, body.ID)
			reply(map[string]string{"status": "OK"})
		default:
			w.WriteHeader(http.StatusNotFound)
			reply(map[string]string{"message": "no route " + r.Method + " " + path})
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

// revokedIDs is every token id the host was told to revoke, in order.
func (h *botHost) revokedIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.revoked)
}

// A STOPPED NODE'S RUN NEVER CREATES THE DATABASE IT READS.
//
// A node whose engine has never run here — or a machine holding a copy of its
// Tier A file — has no rows to read, and opening the path to find that out
// would leave an empty database and a lock file behind, which nothing reads.
func TestARunOnAStoppedNodeCreatesNoDatabase(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	db := storePathOf(t, cfg)

	out, errs, err := provisionCmd(t, gitlabCompanyFile(t), "-dry-run", "-config", cfg,
		"-public-url", "https://crewlet.example.com")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, errs)
	}
	if !strings.Contains(out, "UNKNOWN") {
		t.Errorf("a store with no rows was read as one vouching for nothing:\n%s", out)
	}
	for _, path := range []string{db, db + ".lock"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a read created %s: %v", path, err)
		}
	}
}

// A COMMAND THAT READS NO VALUE OPENS NO STORE.
//
// `slack provision` builds URLs and manifests from Tier A's address and reads
// none of the company's values, so against a running node it has no reason
// to meet the lock or to need a credential for the node — and it used to meet
// the lock, because it built the resolver every other command builds and then
// threw it away. `llm list` reads the company's values
// and so reads them through the node, as every provisioning command does.
func TestCommandsOnARunningNodeDoNotDieOnItsLock(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, nil)
	cfg := bootstrapForURL(t, node.server.URL)
	seedTheStore(t, cfg)
	holdTheStore(t, storePathOf(t, cfg))
	company := gitlabCompanyFile(t)

	// WITH NO CREDENTIAL FOR THE NODE AT ALL: a run that routed its store
	// would need one to reach the node it is going through, so succeeding
	// without one is what shows nothing was opened.
	if out, errs, err := cliIn(t, noEnviron, "slack", "provision", company, "-dry-run",
		"-config", cfg); err != nil {
		t.Errorf("slack provision -dry-run on a running node: %v\n%s%s", err, out, errs)
	}
	if _, requests, _ := node.seen(); len(requests) != 0 {
		t.Errorf("a command that reads no value asked the node %v", requests)
	}

	if out, errs, err := cli(t, "llm", "list", "-company", cliAgentCompanyFile(t),
		"-config", cfg); err != nil {
		t.Errorf("llm list on a running node: %v\n%s%s", err, out, errs)
	}
	asked, _, _ := node.seen()
	if len(asked) != 1 {
		t.Fatalf("llm list asked the node %d time(s) for the document's values, "+
			"want one", len(asked))
	}
	// THE CONVENTION IS ASKED FOR TOO: no document names the bundle
	// `crewlet llm export -secret-store` writes, and a run handed only the
	// referenced names would restore a bundle from the environment instead.
	if !slices.Contains(asked[0], "CREWLET_LLM_CLI_SUB_CREDENTIALS") {
		t.Errorf("llm list asked for %v, which leaves out the entry's bundle", asked[0])
	}
}

// A RUN THAT CANNOT GET THROUGH A RUNNING NODE IS TOLD ITS OWN WAY PAST.
//
// The refusal used to be the one written for `crewlet secrets set`: stop the
// engine (which leaves a provisioner this node's own rows and refuses its mint
// with "start `crewlet run`"), use "an access token" (which the reveal every
// one of these runs makes refuses), or let the environment stand in (which a
// routed read never does). Through the lock and through -api alike, it now
// names the bearer the reveal takes and the node to take it to instead.
func TestARunReadingTheCompanysValuesIsToldItsOwnWayPast(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, nil)
	cfg := bootstrapForURL(t, node.server.URL)
	seedTheStore(t, cfg)
	holdTheStore(t, storePathOf(t, cfg))
	company := gitlabCompanyFile(t)

	for _, tc := range []struct {
		name string
		args []string
		also []string
	}{
		// THROUGH THE LOCK, -api is the way to another node; given -api,
		// the node is already chosen and the bearer is all that is missing.
		{"through the lock", []string{"-config", cfg}, []string{"-api", "api.auth.admin"}},
		{"through -api", []string{"-config", cfg, "-api", node.server.URL}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := cliIn(t, noEnviron, append([]string{"gitlab", "provision",
				company, "-dry-run"}, tc.args...)...)
			if err == nil {
				t.Fatal("a run with no bearer for the node read its values")
			}
			for _, want := range append([]string{"crewlet gitlab provision", apiTokenEnv,
				"Tier A", "machine token"}, tc.also...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal omits %q: %v", want, err)
				}
			}
			for _, wrong := range []string{"access token", "environment",
				"or stop `crewlet run`"} {
				if strings.Contains(err.Error(), wrong) {
					t.Errorf("the refusal offers %q, which does not get this "+
						"run past it: %v", wrong, err)
				}
			}
		})
	}
	if _, requests, _ := node.seen(); len(requests) != 0 {
		t.Errorf("a run with no bearer asked the node %v", requests)
	}
}

// cliAgentCompanyFile is a company whose one cli-agent entry, "sub", names no
// credential of its own — so it reads its bundle under the conventional name —
// and whose CLI is `true` for every command.
func cliAgentCompanyFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return writeFile(t, dir, "company.yaml", fmt.Sprintf(`name: Acme
providers:
  llm:
    sub:
      type: cli-agent
      model: m
      cli:
        agent: custom
        state_dir: %q
        overrides:
          binary: "true"
          complete_args: ["-p"]
          model_args: ["--model", "{model}"]
          output: text
          credential_paths: [".fake/creds.json"]
          status_args: ["status"]
          login_args: ["login"]
roles:
  - name: CEO
    handle: ceo
    llm: sub
`, filepath.Join(dir, "state")))
}

// A SCRATCH NODE IS REFUSED BY THE COMMAND THAT WAS RUN.
//
// A node without `data` keeps no rows to read or write offline, and the
// refusal used to name `crewlet secrets` whatever the operator ran — sending
// somebody running a provisioner off to a command they had not typed.
func TestAScratchStoreIsRefusedByTheCommandThatWasRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := fmt.Sprintf("node:\n  id: agent-1\n  roles: [seats]\nstore:\n  path: %s\n"+
		"  scratch: true\nstream:\n  leaf:\n    urls: [\"nats-leaf://data-a.example.com:7422\"]\n"+
		"coordination:\n  type: embedded-kv\n"+fixtureKeyring,
		filepath.Join(dir, "index.db"))
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"gitlab", "provision", gitlabCompanyFile(t), "-dry-run", "-config", cfg},
		{"llm", "list", "-company", cliAgentCompanyFile(t), "-config", cfg},
	} {
		command := "crewlet " + args[0] + " " + args[1]
		_, _, err := cli(t, args...)
		if err == nil || !strings.Contains(err.Error(), command) ||
			!strings.Contains(err.Error(), "store.scratch") ||
			!strings.Contains(err.Error(), "-api") {
			t.Errorf("%s: err = %v, want a refusal naming the command, "+
				"store.scratch and -api", command, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "config.yaml" {
			t.Errorf("a refused run left %s behind", entry.Name())
		}
	}
}

// seedTheStore creates the database the Tier A at cfg names, as a node's
// first start does, so a case can hand it to a holder.
func seedTheStore(t *testing.T, cfg string) {
	t.Helper()
	if _, errs, err := secretsCmd(t, cfg, "set", "SEEDED", "-value", "x"); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}
}

// storePathOf is the database the Tier A at cfg names.
func storePathOf(t *testing.T, cfg string) string {
	t.Helper()
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return boot.Store.Path
}

// bootOnto does to the node at cfg what its engine's start does to its own
// secret rows: move every one onto the fleet's store and delete it here.
func bootOnto(t *testing.T, cfg string) {
	t.Helper()
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenNode(t.Context(), boot.Store.Path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	fleet := fleetsecrets.New(coordmem.NewFleet(), cipher)
	if _, err := fleetsecrets.Migrate(t.Context(), db.SecretValues(cipher), fleet); err != nil {
		t.Fatalf("migrate onto the fleet: %v", err)
	}
	left, err := db.SecretValues(cipher).List(t.Context())
	if err != nil || len(left) != 0 {
		t.Fatalf("the boot left %d row(s) here (%v)", len(left), err)
	}
}
