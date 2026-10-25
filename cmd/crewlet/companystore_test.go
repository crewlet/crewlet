package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/slack"
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

// seedTheStore creates the database the Tier A at cfg names, as a node's first
// start does, holding one value.
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

// markBrokerRun leaves in the store directory the Tier A at cfg names what a
// member's first start leaves there: the broker's own state directory.
func markBrokerRun(t *testing.T, cfg string) {
	t.Helper()
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(boot.Stream.StoreDir, server.JetStreamStoreDir), 0o700); err != nil {
		t.Fatal(err)
	}
}

// rowsOf is every value this node's own table at cfg holds, read straight off
// the file, as no command reads it on a node that has run.
func rowsOf(t *testing.T, cfg string) map[string]string {
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
	rows, err := db.SecretValues(cipher).All(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// filesBeside is every file in the directory holding db — the database and
// its sidecars among them — each as its size, modification time and bytes, so
// two readings differ wherever anything opened, wrote or created one.
func filesBeside(t *testing.T, db string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(db))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			out[entry.Name()] = info.Mode().String()
			continue
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(db), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = fmt.Sprintf("%d bytes, %s, %x", info.Size(),
			info.ModTime().Format(time.RFC3339Nano), sha256.Sum256(raw))
	}
	return out
}

// servingAPI is the api block of a node that serves one: a port, the address
// browsers use, and a Tier A token that clears the floor.
func servingAPI(port int) string {
	return fmt.Sprintf("api:\n  host: 127.0.0.1\n  port: %d\n"+
		"  external_url: \"http://127.0.0.1:%d\"\n  auth:\n    admin: serve\n"+
		"    tokens:\n      - id: ops\n        token: %s\n        grants: [admin]\n",
		port, port, cliFixtureToken)
}

// tierAWith writes a Tier A naming a store in its own directory, the fixture
// keyring, and blocks — the stream's, the API's, the node's — and returns its
// path, refusing a document Tier A itself refuses.
func tierAWith(t *testing.T, blocks func(dir string) string) string {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf("store:\n  path: %s\n", filepath.Join(dir, "index.db")) +
		blocks(dir) + fixtureKeyring
	path := filepath.Join(dir, "crewlet.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBootstrapForStore(path); err != nil {
		t.Fatalf("the fixture's Tier A is refused: %v\n%s", err, body)
	}
	return path
}

// stoppedShape is a stopped node whose own rows are not the company's whole
// secret store, for the reason its name gives.
type stoppedShape struct {
	name string
	// tierA writes the node's Tier A, serving an API.
	tierA func(t *testing.T) string
	// says is what the refusal says of the node.
	says string
}

var stoppedShapes = []stoppedShape{
	{
		name: "a sole member whose broker has run",
		tierA: func(t *testing.T) string {
			cfg := tierAWith(t, func(dir string) string {
				return "node:\n  id: cli-test\n" + fixtureStream(dir) + servingAPI(1)
			})
			markBrokerRun(t, cfg)
			return cfg
		},
		says: "has run before",
	},
	{
		name: "a member of a cluster that never started",
		tierA: func(t *testing.T) string {
			return tierAWith(t, func(dir string) string {
				return "node:\n  id: node-a\ncoordination:\n  type: embedded-kv\n" +
					"stream:\n  store_dir: " + filepath.Join(dir, "stream") + "\n  replicas: 3\n" +
					"  cluster:\n    name: crewlet\n    port: 6222\n" +
					"    peers: [\"nats://node-b.example.com:6222\", \"nats://node-c.example.com:6222\"]\n" +
					servingAPI(1)
			})
		},
		says: "member of a fleet (stream.cluster)",
	},
	{
		name: "a client of an external broker",
		tierA: func(t *testing.T) string {
			return tierAWith(t, func(string) string {
				return "node:\n  id: cli-test\ncoordination:\n  type: embedded-kv\n" +
					"stream:\n  type: nats\n  url: nats://nats.example.com:4222\n" +
					servingAPI(1)
			})
		},
		says: "external broker (stream.type: nats)",
	},
}

// untouchedVendor is every vendor these cases name, answering nothing: a run
// that reached it did something before it was refused.
func untouchedVendor(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a refused run reached the vendor: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// companyAt writes doc, with every vendor address in it pointed at vendor.
func companyAt(t *testing.T, doc, vendor string) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "company.yaml", strings.ReplaceAll(doc, "VENDOR", vendor))
}

// readerCompanyDoc is a company every reader of the company has work in: a
// GitLab seat, a Mattermost bot, a Slack app, a GitHub, Jira and Confluence
// block, each signing secret a whole ${VAR}, every vendor at VENDOR — GitHub's
// with an organization to register its hook on, without which a run registers
// none.
const readerCompanyDoc = `
name: Nimbus
providers:
  llm:
    main:
      model: claude-sonnet-5
integrations:
  gitlab:
    enabled: true
    url: VENDOR
    signing_secret: "${GITLAB_SIGNING_SECRET}"
    provisioning:
      group: nimbus
  github:
    enabled: true
    url: VENDOR
    token: ghp-an-org-token
    webhook_secret: "${GITHUB_WEBHOOK_SECRET}"
    provisioning:
      org: nimbus
  jira:
    url: VENDOR
    email: ops@example.com
    token: a-jira-token
    webhook_secret: "${JIRA_WEBHOOK_SECRET}"
  confluence:
    url: VENDOR
    email: ops@example.com
    token: a-confluence-token
    webhook_secret: "${CONFLUENCE_WEBHOOK_SECRET}"
  mattermost:
    enabled: true
    url: VENDOR
    team: nimbus
roles:
  - name: CEO
    handle: ceo
    llm: main
    mcp_env:
      gitlab:
        GITLAB_TOKEN: "${GITLAB_TOKEN_CEO}"
    integrations:
      mattermost:
        bot_token: "${MM_TOKEN_CEO}"
      slack:
        bot_token: "${SLACK_BOT_TOKEN_CEO}"
        signing_secret: "${SLACK_SIGNING_SECRET_CEO}"
`

// cliAgentCompanyFile is a company whose one entry, "sub", names no credential
// of its own — so it reads its bundle under the conventional name — and whose
// CLI is `true` for every command, and the entry as the run builds it.
func cliAgentCompanyFile(t *testing.T) (string, *cliagent.Provider) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	path := writeFile(t, dir, "company.yaml", fmt.Sprintf(`name: Acme
providers:
  llm:
    sub:
      agent: custom
      model: m
      state_dir: %q
      overrides:
        binary: "true"
        complete_args: ["-p"]
        model_args: ["--model", "{model}"]
        output: text
        credential_paths: [".fake/creds.json"]
        host_credential_paths: [".fake/creds.json"]
        status_args: ["status"]
roles:
  - name: CEO
    handle: ceo
    llm: sub
`, state))
	p, err := cliagent.New(cliagent.Config{
		Key: "sub", Agent: "custom", StateDir: state,
		Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "true", "complete_args": []any{"-p"},
			"model_args":            []any{"--model", "{model}"},
			"output":                "text",
			"credential_paths":      []any{".fake/creds.json"},
			"host_credential_paths": []any{".fake/creds.json"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return path, p
}

// aBundle is a credential bundle of the entry cliAgentCompanyFile declares,
// exported from a login of its own.
func aBundle(t *testing.T) string {
	t.Helper()
	source := bundleProvider(t)
	creds := filepath.Join(source[0].provider.Workspace().CredentialsDir(), "creds.json")
	if err := os.MkdirAll(filepath.Dir(creds), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(creds, []byte(`{"token":"from-the-rows"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := source[0].provider.ExportBundle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// readerRun is one command that reads the company's stored values, as a case
// runs it against the Tier A at cfg and a vendor that must not be reached.
type readerRun struct {
	name string
	// args is the command line; entry, for the llm reports, is the entry
	// whose credentials directory a restore would write into.
	args func(t *testing.T, cfg, vendor string) (args []string, entry *cliagent.Provider)
}

// readersOfTheCompany is every command that reads the company's stored values:
// each vendor command that resolves the company, its -dry-run where that dry
// run resolves values, the command's flag for a fresh value, a real `slack
// provision` whatever its sink — `-secret-store` reads the seats' tokens back,
// and `-print` asks which of its names the store holds, which it outranks —
// and the four llm reports.
func readersOfTheCompany() []readerRun {
	vendorRun := func(name string, extra ...string) readerRun {
		fields := strings.Fields(name)
		return readerRun{name: strings.Join(append(fields, extra...), " "),
			args: func(t *testing.T, cfg, vendor string) ([]string, *cliagent.Provider) {
				company := companyAt(t, readerCompanyDoc, vendor)
				args := append(slices.Clone(fields), company)
				switch fields[0] {
				case "confluence":
					if fields[1] == "import" {
						dir := t.TempDir()
						if err := os.WriteFile(filepath.Join(dir, "page.md"),
							[]byte("# A page\n\nBody.\n"), 0o600); err != nil {
							t.Fatal(err)
						}
						args = append(args, dir)
					}
				}
				args = append(args, extra...)
				return append(args, "-config", cfg), nil
			}}
	}
	llmRun := func(sub string, extra ...string) readerRun {
		return readerRun{name: "llm " + sub,
			args: func(t *testing.T, cfg, _ string) ([]string, *cliagent.Provider) {
				company, entry := cliAgentCompanyFile(t)
				return append([]string{"llm", sub, "sub", "-company", company, "-config", cfg},
					extra...), entry
			}}
	}
	const sink, public = "-print", "-public-url=https://crewlet.example.com"
	const slackConfigToken = "-config-token=xoxe-1-a-fixture-refresh-token"
	return []readerRun{
		vendorRun("gitlab provision", sink, public, "-admin-token=glpat-admin"),
		vendorRun("gitlab provision", "-dry-run", public),
		vendorRun("gitlab provision", sink, public, "-admin-token=glpat-admin", "-rotate"),
		vendorRun("github provision", sink, public),
		vendorRun("github provision", "-dry-run"),
		vendorRun("github provision", sink, public, "-recreate-webhooks"),
		vendorRun("jira provision", sink, public),
		vendorRun("jira provision", "-dry-run"),
		vendorRun("jira provision", sink, public, "-recreate-webhook"),
		vendorRun("confluence provision", sink, public),
		vendorRun("confluence provision", "-dry-run"),
		vendorRun("confluence provision", sink, public, "-recreate-webhooks"),
		vendorRun("confluence import"),
		vendorRun("confluence resync"),
		vendorRun("mattermost provision", sink, "-admin-token=mm-admin"),
		vendorRun("mattermost provision", sink, "-admin-token=mm-admin", "-rotate"),
		vendorRun("mattermost doctor", "-admin-token=mm-admin"),
		// WITH AN APP-CONFIGURATION TOKEN, so a run the refusal let through
		// goes on to Slack, which no case's run reaches: its first request
		// fails the case ([unreachableSlack]).
		vendorRun("slack provision", "-secret-store", public, slackConfigToken),
		vendorRun("slack provision", "-secret-store", public, slackConfigToken, "-reinstall"),
		vendorRun("slack provision", sink, public, slackConfigToken),
		vendorRun("slack provision", sink, public, slackConfigToken, "-reinstall"),
		llmRun("list"),
		llmRun("status"),
		llmRun("export"),
		llmRun("export", "-secret-store"),
		llmRun("doctor", "-no-smoke"),
	}
}

// A STOPPED NODE WHOSE OWN ROWS ARE NOT THE WHOLE STORE REFUSES EVERY READER
// OF THE COMPANY, before it reads, plans or changes anything.
//
// The company's values are on a store only a running engine serves — the
// broker on this node has run, or the node is one member of a cluster, or a
// client of one somebody else runs — so a miss in this node's rows says
// nothing about them. A run that read it as unset minted over the fleet's
// value, and the engine's next start took the run's write over the fleet's;
// a report restored a configured bundle from these rows into an empty
// credentials directory, which the engine then refused to overwrite. So each
// such run is refused with what the node is and the two ways to the fleet's
// values, whatever flag for a fresh value it was given, and:
//
//   - nothing is printed but the error, on either stream: not a plan, nor a
//     route line;
//   - no vendor is reached;
//   - no database is created where there was none, and one that exists is
//     never opened — an open applies pending schema — so its file and its
//     lock's stamp are as they were, byte for byte and to the nanosecond;
//   - an llm entry's configured bundle — here in this node's rows — is not
//     restored into its credentials directory.
//
// Mutation: drop the StoreHasRun stat, and the sole member's rows go red; drop
// SoleMember from stoppedNode, and the cluster member's rows do; let a reader
// through a target with stopped set, or open the file before deciding, and
// every row does.
func TestAStoppedNodeThatHasRunRefusesEveryReaderOfTheCompany(t *testing.T) {
	t.Parallel()
	bundle := aBundle(t)
	for _, shape := range stoppedShapes {
		for _, seeded := range []bool{false, true} {
			name := shape.name + ", no database"
			if seeded {
				name = shape.name + ", rows held"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				cfg := shape.tierA(t)
				db := storePathOf(t, cfg)
				var before map[string]string
				if seeded {
					// `secrets set` is the one write a stopped node that
					// has run still takes, so this seeds it as an operator
					// would.
					if _, errs, err := secretsCmd(t, cfg, "set",
						"CREWLET_LLM_CLI_SUB_CREDENTIALS", "-value", bundle); err != nil {
						t.Fatalf("seed: %v (%s)", err, errs)
					}
					seedTheStore(t, cfg)
					before = rowsOf(t, cfg)
				}
				files := filesBeside(t, db)
				for _, reader := range readersOfTheCompany() {
					t.Run(reader.name, func(t *testing.T) {
						vendor := untouchedVendor(t)
						args, entry := reader.args(t, cfg, vendor)
						out, errs, err := cli(t, args...)
						if !errors.Is(err, errStoppedNode) {
							t.Fatalf("err = %v, want the stopped-node refusal\n%s%s", err, out, errs)
						}
						for _, want := range []string{"crewlet " + strings.Join(args[:2], " "),
							shape.says, "-api", "Nothing was read or changed."} {
							if !strings.Contains(err.Error(), want) {
								t.Errorf("the refusal omits %q: %v", want, err)
							}
						}
						if out != "" || errs != "" {
							t.Errorf("a refused run printed:\n%s%s", out, errs)
						}
						if entry != nil {
							if has, _ := entry.Workspace().HasLogin(t.Context()); has {
								t.Error("a refused report restored a bundle into the entry's " +
									"credentials directory")
							}
						}
					})
				}
				if after := filesBeside(t, db); !maps.Equal(files, after) {
					t.Errorf("refused runs changed the files beside %s: %v, then %v", db, files, after)
				}
				if seeded {
					if after := rowsOf(t, cfg); !maps.Equal(before, after) {
						t.Errorf("the rows changed under refused runs: %v, then %v", before, after)
					}
					return
				}
				for _, path := range []string{db, db + ".lock"} {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("a refused run created %s: %v", path, err)
					}
				}
			})
		}
	}
}

// A RUN THAT READS NO STORED VALUE DECIDES NO ROUTE, and so runs on a stopped
// node that has run: a dry run that resolves nothing, a plan with nothing in it
// — a Slack run with no seat to provision among them, whatever its sink.
// Refusing them would refuse a question the store has no part in.
//
// A Slack seat whose credentials are literals is no seat to provision: the run
// leaves it to the operator who wrote them, and records or reads back nothing
// for it.
//
// Mutation: route before the empty-plan or dry-run return, or decide a Slack
// run's plan is empty by its seats rather than by those it can provision, and
// these go red.
func TestARunThatReadsNoStoredValueRunsOnAStoppedNodeThatHasRun(t *testing.T) {
	t.Parallel()
	cfg := stoppedShapes[0].tierA(t)
	vendor := untouchedVendor(t)
	company := companyAt(t, readerCompanyDoc, vendor)
	empty := companyAt(t, "name: Nimbus\nintegrations:\n  mattermost:\n    enabled: true\n"+
		"    url: VENDOR\n    team: nimbus\n  confluence:\n    url: VENDOR\n    email: ops@example.com\n"+
		"    token: t\n    webhook_secret: \"${CONFLUENCE_WEBHOOK_SECRET}\"\nroles:\n"+
		"  - name: CEO\n    handle: ceo\n", vendor)
	literal := companyAt(t, "name: Nimbus\nproviders:\n  llm:\n    main:\n      model: claude-sonnet-5\n"+
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: main\n    integrations:\n      slack:\n"+
		"        bot_token: xoxb-a-literal-token\n        signing_secret: a-literal-signing-secret\n",
		vendor)
	ledger := filepath.Join(t.TempDir(), "ledger.json")
	for _, args := range [][]string{
		{"mattermost", "provision", company, "-dry-run"},
		{"mattermost", "provision", empty, "-print", "-admin-token=mm-admin"},
		{"confluence", "import", company, t.TempDir(), "-dry-run"},
		{"confluence", "import", empty, t.TempDir()},
		{"slack", "provision", empty, "-print", "-ledger", ledger},
		{"slack", "provision", literal, "-secret-store", "-ledger", ledger},
	} {
		t.Run(strings.Join(args[:2], " ")+" "+strings.Join(args[3:], " "), func(t *testing.T) {
			out, errs, err := cli(t, append(args, "-config", cfg)...)
			if err != nil {
				t.Fatalf("a run that reads no stored value was refused: %v\n%s%s", err, out, errs)
			}
			if strings.Contains(out, "resolving ${VAR}") {
				t.Errorf("a run that reads no stored value decided a route:\n%s", out)
			}
		})
	}
}

// A STOPPED NODE SERVING NO API IS TOLD ONLY OF -api: Tier A gives it no
// API, so starting it as configured gives no way to read the values through,
// and offering that would send an operator to a step that ends at the same
// refusal. What was read is said as read — `crewlet run -api-port` and
// `-roles` override both, and this command cannot know whether the engine was
// started with one — so the refusal claims no more of the node than its Tier
// A says.
func TestAStoppedNodeServingNoAPIIsToldOnlyOfAPI(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		blocks func(dir string) string
		says   string
	}{
		"api.port 0": {
			blocks: func(dir string) string { return "node:\n  id: cli-test\n" + fixtureStream(dir) },
			says:   "api.port is 0 (a `crewlet run -api-port` override is not known here)",
		},
		"no ingress role": {
			blocks: func(dir string) string {
				return "node:\n  id: cli-test\n  roles: [data, seats, workers]\n" +
					fixtureStream(dir) + servingAPI(1)
			},
			says: "leave out ingress (a `crewlet run -roles` override is not known here)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := tierAWith(t, tc.blocks)
			markBrokerRun(t, cfg)
			_, _, err := cli(t, "gitlab", "provision", gitlabCompanyFile(t), "-dry-run",
				"-config", cfg)
			if !errors.Is(err, errStoppedNode) {
				t.Fatalf("err = %v, want the stopped-node refusal", err)
			}
			for _, want := range []string{"Tier A gives this node no API", tc.says, "-api URL"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal omits %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "crewlet run -config") {
				t.Errorf("a node serving no API was told to start its engine and re-run: %v", err)
			}
		})
	}
}

// THE ROUTE LINE AND THE REFUSAL NAME EVERY PATH FROM THE ROOT. Each path in
// Tier A is relative to the working directory, which is the one thing a run
// from the wrong place gets wrong — so the line naming this node's rows as the
// whole store, and the refusal naming its broker's state, say each path from
// the root however Tier A spells it, and a run that found nothing because it
// looked in the wrong place says where it looked.
//
// The paths here are spelled relative to this test's own working directory,
// so nothing has to change it.
//
// Mutation: print a path as Tier A spells it, and its relative spelling shows.
func TestTheRouteAndTheRefusalNameEveryPathFromTheRoot(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative := func(t *testing.T, path string) string {
		t.Helper()
		rel, err := filepath.Rel(wd, path)
		if err != nil || filepath.IsAbs(rel) {
			t.Fatalf("no relative spelling of %s from %s: %q, %v", path, wd, rel, err)
		}
		return rel
	}
	for _, ran := range []bool{false, true} {
		t.Run(fmt.Sprintf("broker has run %v", ran), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			db, streamDir := filepath.Join(dir, "index.db"), filepath.Join(dir, "stream")
			cfg := writeFile(t, dir, "crewlet.yaml", fmt.Sprintf(
				"store:\n  path: %s\nnode:\n  id: cli-test\nstream:\n  store_dir: %s\n",
				relative(t, db), relative(t, streamDir))+servingAPI(1)+fixtureKeyring)
			want := []string{streamDir}
			if ran {
				markBrokerRun(t, cfg)
			} else {
				want = append(want, db)
			}
			out, errs, err := cli(t, "gitlab", "provision", gitlabCompanyFile(t), "-dry-run",
				"-config", cfg, "-public-url", "https://crewlet.example.com")
			said := out + errs
			if ran {
				if !errors.Is(err, errStoppedNode) {
					t.Fatalf("err = %v, want the stopped-node refusal", err)
				}
				said = err.Error()
			} else if err != nil || !strings.Contains(said, "company's whole secret store") {
				t.Fatalf("err = %v, want a run on the whole store:\n%s", err, said)
			}
			for _, path := range want {
				if !strings.Contains(said, path) {
					t.Errorf("%s is not named from the root:\n%s", path, said)
				}
				if rel := relative(t, path); strings.Contains(said, rel) {
					t.Errorf("%s is named as Tier A spells it:\n%s", rel, said)
				}
			}
		})
	}
}

// WHERE WHETHER THE BROKER HAS RUN CANNOT BE TOLD, THE RUN IS REFUSED, never
// read as a node that has not: a store directory that is a file — a stat
// failure no privilege bypasses — names the failure in the refusal.
//
// Mutation: read a stat error as "never ran", and the run resolves.
func TestAnUnreadableBrokerStateIsRefusedRatherThanReadAsNew(t *testing.T) {
	t.Parallel()
	cfg := tierAWith(t, func(dir string) string {
		file := filepath.Join(dir, "stream")
		if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		return "node:\n  id: cli-test\n" + fixtureStream(dir) + servingAPI(1)
	})
	out, _, err := cli(t, "gitlab", "provision", gitlabCompanyFile(t), "-dry-run", "-config", cfg)
	if !errors.Is(err, errStoppedNode) {
		t.Fatalf("err = %v, want the stopped-node refusal\n%s", err, out)
	}
	if !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "cannot be told") {
		t.Errorf("the refusal does not name what could not be read: %v", err)
	}
}

// A NODE WHOSE BROKER HAS NEVER RUN READS ITS OWN ROWS AS THE WHOLE STORE.
//
// The fleet's store is that member's own JetStream, which does not exist until
// its broker first starts, so its rows are the company's whole store: a row is
// held, and none is definitively not held. A provisioning run there before the
// first start is how a company is set up, and it reads and records there, says
// so on its first line naming both directories from the root, and creates no
// database for a read — only a write does.
func TestANodeWhoseBrokerHasNeverRunReadsItsRowsAsTheWholeStore(t *testing.T) {
	t.Parallel()
	t.Run("the rows answer, and the run says whose", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapWithKeyring(t, "k1")
		if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_SIGNING_SECRET",
			"-value", storedSigningSecret); err != nil {
			t.Fatalf("seed: %v (%s)", err, errs)
		}
		out, errs, err := provisionCmd(t, gitlabCompanyFile(t), "-dry-run", "-config", cfg,
			"-public-url", "https://crewlet.example.com")
		if err != nil {
			t.Fatalf("a dry run before the first start: %v\n%s", err, errs)
		}
		first, _, _ := strings.Cut(out, "\n")
		boot, _ := loadBootstrapForStore(cfg)
		for _, want := range []string{boot.Store.Path, boot.Stream.StoreDir,
			"these rows are the company's whole secret store"} {
			if !strings.Contains(first, want) {
				t.Errorf("the first line does not say %q: %q", want, first)
			}
		}
		if !strings.Contains(out, "reused — the configured one already resolves") {
			t.Errorf("the stored signing key did not reach the plan:\n%s", out)
		}
	})
	t.Run("a read creates no database, and the first write does", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapWithKeyring(t, "k1")
		db := storePathOf(t, cfg)
		out, errs, err := provisionCmd(t, gitlabCompanyFile(t), "-dry-run", "-config", cfg,
			"-public-url", "https://crewlet.example.com")
		if err != nil {
			t.Fatalf("dry run: %v\n%s", err, errs)
		}
		if !strings.Contains(out, "which do not exist yet") ||
			!strings.Contains(out, "WILL BE MINTED into GITLAB_SIGNING_SECRET") {
			t.Errorf("a node with no rows yet was not planned as one:\n%s", out)
		}
		for _, path := range []string{db, db + ".lock"} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a read created %s: %v", path, err)
			}
		}

		sink := secretStoreSink(t, noEnviron, "-config", cfg)
		if err := sink.Record(t.Context(), "GITLAB_TOKEN_SWE", "glpat-minted"); err != nil {
			t.Fatalf("record into the rows: %v", err)
		}
		if got := rowsOf(t, cfg)["GITLAB_TOKEN_SWE"]; got != "glpat-minted" {
			t.Errorf("a -secret-store write before the first start did not land in the "+
				"rows: %q", got)
		}
	})
}

// secretStoreSink is a `-secret-store` sink opened as a provisioning run opens
// it, under the store flags given.
func secretStoreSink(t *testing.T, environ config.Source, flags ...string) provision.TokenSink {
	t.Helper()
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sinks := addSinkFlags(fs)
	if err := fs.Parse(append([]string{"-secret-store"}, flags...)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(flags, "-config") {
		*sinks.store.bootstrap = filepath.Join(t.TempDir(), "absent.yaml")
	}
	companySecrets, err := openCompanyStore(environ, sinks.store,
		"crewlet gitlab provision", readsTheCompany)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(companySecrets.Close)
	sink, err := sinks.open(t.Context(), companySecrets, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

// A RUN THAT ACTS ON ITS OWN RUNS ON A STOPPED NODE WHEREVER IT HAS RUN.
//
// `crewlet llm login` writes a new token, which the next start carries onto
// the fleet later-wins, and `logout` and `import` remove or restore a login on
// this host and write no row: none of them is right or wrong by what the
// fleet holds. So they run on a stopped node that has run, the token landing
// in its rows with the note that says when it reaches the fleet, and nothing
// said about the rows they read the entry from — no route line, no refusal.
//
// Mutation: hold them to the rule, and every case is refused.
func TestAWriterLandsInAStoppedNodesRowsWhereverItHasRun(t *testing.T) {
	t.Parallel()
	cfg := stoppedShapes[0].tierA(t)

	t.Run("login -token-stdin", func(t *testing.T) {
		store := llmStoreAt(t, noEnviron, "login", "-config", cfg)
		company := writeFile(t, t.TempDir(), "company.yaml",
			"name: Acme\nproviders:\n  llm:\n"+healthyDoctorEntry(t, "claude", "m")+
				"roles:\n  - name: CEO\n    handle: ceo\n    llm: claude\n")
		var notes bytes.Buffer
		doc, keys, resolver, err := loadLLMConfig(t.Context(), company, store, "login", "claude", &notes)
		if err != nil {
			t.Fatalf("a login on a stopped node that has run was refused: %v", err)
		}
		providers, err := cliAgents(doc, keys, resolver)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := loginLLM(t.Context(), loginRequest{providers: providers, key: "claude",
			tokenStdin: true, companySecrets: store, stdin: strings.NewReader("fake-pasted\n")},
			&out, &notes); err != nil {
			t.Fatalf("login -token-stdin: %v", err)
		}
		if notes.Len() > 0 {
			t.Errorf("a login on rows that are not the whole store said of them: %q", notes.String())
		}
		if !strings.Contains(out.String(), secretsLocalNote) {
			t.Errorf("the write does not say when the fleet reads it:\n%s", out.String())
		}
		if got := rowsOf(t, cfg)["FAKE_CLI_TOKEN"]; got != "fake-pasted" {
			t.Errorf("the token did not land in this node's rows: %q", got)
		}
	})
	t.Run("logout and import", func(t *testing.T) {
		company, entry := cliAgentCompanyFile(t)
		for _, sub := range []string{"logout", "import"} {
			out, errs, err := cli(t, "llm", sub, "sub", "-company", company, "-config", cfg)
			if errors.Is(err, errStoppedNode) {
				t.Errorf("llm %s on a stopped node that has run was refused: %v", sub, err)
			}
			if strings.Contains(out+errs, "resolving ${VAR}") {
				t.Errorf("llm %s said of rows that are not the whole store:\n%s%s", sub, out, errs)
			}
		}
		if has, _ := entry.Workspace().HasLogin(t.Context()); has {
			t.Error("the premise: an import of an empty stdin restored a login")
		}
	})
}

// A BROKER LOGIN OR AN IMPORT NAMES THE DOCTOR AS IT CAN BE RUN FROM WHERE IT
// RAN, with the -company, -config and -api the run was given: the doctor reads
// the company document and the store the run did, and a bare one after a run
// through -api reads this machine's Tier A instead, which may be a stopped node
// that refuses it. Both run on a stopped node whose own rows are not the
// company's whole secret store, and the doctor that checks what they wrote is
// refused there, since it reads the company's stored values: so there each ends
// on the running engine and the token its reveal takes, or -api — only -api
// where Tier A gives the node no API — and anywhere else on the doctor as it
// stands.
//
// Mutation: end on the bare doctor whatever the route, and the stopped nodes'
// rows go red; condition it everywhere, and the others do; leave out a flag the
// run was given, and every row does, the -api one beside a Tier A that is a
// stopped node that has run among them.
func TestALoginNamesTheDoctorAsItCanBeRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  func(t *testing.T) string
		// api is whether the run names a node with -api.
		api bool
		// says is what the line names beyond the doctor, none where it is
		// the doctor as it stands.
		says []string
	}{
		{name: "a stopped node that has run", cfg: stoppedShapes[0].tierA,
			says: []string{"once this node's engine is running (crewlet run -config ",
				apiTokenEnv + " holds one of ", "or with -api URL naming a running node"}},
		{name: "a stopped node that has run, serving no API", cfg: func(t *testing.T) string {
			cfg := tierAWith(t, func(dir string) string { return "node:\n  id: cli-test\n" + fixtureStream(dir) })
			markBrokerRun(t, cfg)
			return cfg
		}, says: []string{"and -api URL naming a running node", "api.port is 0"}},
		{name: "-api beside a stopped node that has run", cfg: stoppedShapes[0].tierA, api: true},
		{name: "a single node before its first start", cfg: func(t *testing.T) string {
			return bootstrapWithKeyring(t, "k1")
		}},
		{name: "no Tier A", cfg: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "absent.yaml")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg(t)
			environ, flags := noEnviron, []string{"-config", cfg}
			if tc.api {
				environ = tokenEnviron("ops-token")
				flags = append(flags, "-api", newRevealNode(t, nil).server.URL)
			}
			check := func(t *testing.T, company, out string) {
				t.Helper()
				doctor := strings.Join(append([]string{"crewlet llm doctor sub -company", company},
					flags...), " ")
				lines := strings.Split(strings.TrimSpace(out), "\n")
				last := lines[len(lines)-1]
				if !strings.HasPrefix(last, "Verify it with `"+doctor+"`") {
					t.Fatalf("the run does not end on the doctor with its own flags, `%s`:\n%s",
						doctor, out)
				}
				if tc.says == nil && last != "Verify it with `"+doctor+"`." {
					t.Errorf("a run whose doctor runs as it stands conditioned it: %q", last)
				}
				for _, want := range tc.says {
					if !strings.Contains(last, want) {
						t.Errorf("the line omits %q, so it sends the operator to a doctor "+
							"refused here: %q", want, last)
					}
				}
				if tc.says != nil && strings.Contains(tc.name, "no API") &&
					strings.Contains(last, apiTokenEnv) {
					t.Errorf("a node serving no API was told to start its engine: %q", last)
				}
			}
			t.Run("import", func(t *testing.T) {
				company, _ := cliAgentCompanyFile(t)
				store := llmStoreAt(t, environ, "import", flags...)
				doc, keys, resolver, err := loadLLMConfig(t.Context(), company, store,
					"import", "sub", io.Discard)
				if err != nil {
					t.Fatalf("an import was refused: %v", err)
				}
				providers, err := cliAgents(doc, keys, resolver)
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				if err := importLLM(t.Context(), providers, "sub", company, store,
					strings.NewReader(aBundle(t)), &out); err != nil {
					t.Fatalf("import: %v", err)
				}
				check(t, company, out.String())
			})
			t.Run("login", func(t *testing.T) {
				dir := t.TempDir()
				company := writeFile(t, dir, "company.yaml", fmt.Sprintf(`name: Acme
providers:
  llm:
    sub:
      agent: custom
      model: m
      state_dir: %q
      overrides:
        binary: sh
        complete_args: ["-p"]
        model_args: ["--model", "{model}"]
        output: text
        credential_paths: [".fake/creds.json"]
        status_args: ["-c", "exit 0"]
        login_args: ["-c", 'mkdir -p "$HOME/.fake" && echo {} > "$HOME/.fake/creds.json"']
roles:
  - name: CEO
    handle: ceo
    llm: sub
`, filepath.Join(dir, "state")))
				out, errs, err := cliIn(t, environ, append([]string{"llm", "login", "sub",
					"-company", company}, flags...)...)
				if err != nil {
					t.Fatalf("login: %v\n%s%s", err, out, errs)
				}
				check(t, company, out)
			})
		})
	}
}

// ON A STOPPED NODE THAT HAS RUN, `crewlet secrets` ONLY SETS.
//
// Before a sole member's first start its own table is the company's whole
// store and every subcommand works on it. After one the company's rows are
// the fleet's, and this node's are what was written since it stopped: `list`
// and `get` would answer for those, an `unset` of a name the fleet holds
// removes nothing (the migration copies rows and carries no removal), and a
// `rekey` re-seals rows that are not the ones a rotation is about. So those
// are refused, naming the running engine and -api, and `set` — whose value
// the next start carries onto the fleet later-wins — writes the rows.
//
// Mutation: let a reader through a stopped target, and get, list, unset and
// rekey answer from the rows; hold `set` to the rule, and the write is refused.
func TestOnAStoppedNodeThatHasRunTheSecretsCommandOnlySets(t *testing.T) {
	t.Parallel()
	cfg := stoppedShapes[0].tierA(t)
	out, errs, err := secretsCmd(t, cfg, "set", "ROTATED", "-value", "v2")
	if err != nil {
		t.Fatalf("secrets set on a stopped node that has run: %v (%s)", err, errs)
	}
	if !strings.Contains(out, secretsLocalNote) {
		t.Errorf("the write does not say when the fleet reads it:\n%s", out)
	}
	if got := rowsOf(t, cfg)["ROTATED"]; got != "v2" {
		t.Fatalf("the value did not land in the rows: %q", got)
	}
	for _, args := range [][]string{
		{"get", "ROTATED", "-reveal"}, {"list"}, {"unset", "ROTATED"},
		{"rekey"}, {"rekey", "-dry-run"},
	} {
		out, _, err := secretsCmd(t, cfg, args...)
		if !errors.Is(err, errStoppedNode) {
			t.Errorf("secrets %s = %v, want the stopped-node refusal\n%s", args[0], err, out)
			continue
		}
		if !strings.Contains(err.Error(), "crewlet secrets "+args[0]) || out != "" {
			t.Errorf("secrets %s: the refusal names no command, or printed %q: %v",
				args[0], out, err)
		}
	}
	if got := rowsOf(t, cfg)["ROTATED"]; got != "v2" {
		t.Errorf("a refused unset changed the rows: %q", got)
	}
}

// A MISTAKE IN THE COMMAND LINE IS ANSWERED AS ONE, before any route is
// decided: on a stopped node that has run, where every subcommand but set is
// refused, a mistyped subcommand, a missing name or a `get` without -reveal is
// told what is wrong with it — not to start the engine, which would not fix
// it.
//
// Mutation: check the command line after the store is routed, and each of
// these gets the stopped-node refusal instead.
func TestASecretsUsageErrorIsAnsweredBeforeAnyRoute(t *testing.T) {
	t.Parallel()
	cfg := stoppedShapes[0].tierA(t)
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"lsit"}, `unknown secrets command "lsit"`},
		{[]string{"get", "TOKEN"}, "pass -reveal"},
		{[]string{"get", "-reveal"}, "secrets get needs a name"},
		{[]string{"unset"}, "secrets unset needs a name"},
		{[]string{"set", "-value", "v"}, "secrets set needs a name"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			_, _, err := secretsCmd(t, cfg, tc.args...)
			if err == nil || errors.Is(err, errStoppedNode) || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want %q", err, tc.says)
			}
		})
	}
}

// A PROVISIONING RUN'S CHOICE OF SINK IS ANSWERED AS A MISTAKE IN ITS COMMAND
// LINE, before any route is decided: on a stopped node that has run, where
// every real provisioning run is refused, a run naming no sink, or two, is
// told to name exactly one — not to start the engine, which would not fix it —
// and reaches no vendor.
//
// Mutation: check the sink only where it is opened, after the route, and the
// GitLab, GitHub, Jira and Confluence rows get the stopped-node refusal
// instead.
func TestAProvisioningRunsChoiceOfSinkIsAnsweredBeforeAnyRoute(t *testing.T) {
	t.Parallel()
	cfg := stoppedShapes[0].tierA(t)
	const public = "-public-url=https://crewlet.example.com"
	for _, command := range [][]string{
		{"gitlab", "provision", public, "-admin-token=glpat-admin"},
		{"github", "provision", public},
		{"jira", "provision", public},
		{"confluence", "provision", public},
		{"mattermost", "provision", "-admin-token=mm-admin"},
		{"slack", "provision", public, "-config-token=xoxe-refresh"},
	} {
		for _, tc := range []struct {
			name  string
			sinks []string
			says  string
		}{
			{"no sink", nil, "pass -secret-store, -env-file PATH or -print"},
			{"two sinks", []string{"-secret-store", "-print"},
				"name exactly one of -secret-store, -env-file and -print"},
		} {
			t.Run(strings.Join(command[:2], " ")+", "+tc.name, func(t *testing.T) {
				t.Parallel()
				company := companyAt(t, readerCompanyDoc, untouchedVendor(t))
				args := append(slices.Clone(command[:2]), company)
				args = append(append(args, command[2:]...), tc.sinks...)
				out, errs, err := cli(t, append(args, "-config", cfg)...)
				if err == nil || errors.Is(err, errStoppedNode) || !strings.Contains(err.Error(), tc.says) {
					t.Errorf("err = %v, want %q\n%s%s", err, tc.says, out, errs)
				}
				if tc.sinks == nil && !errors.Is(err, provision.ErrNoSink) {
					t.Errorf("err = %v, want provision.ErrNoSink", err)
				}
			})
		}
	}
}

// A RUN ON A NODE WHOSE ENGINE IS UP READS THE FLEET THROUGH IT.
//
// The engine holds its store for as long as it runs, so the lock routes the
// whole run through the node's API — a dry run included — and the values come
// from the fleet's store: the listing, then one batch of exactly the names the
// command reads that the node lists, under the operator's own token. Whether
// the node's broker has run or not: a running engine's broker has, so that is
// the node an operator meets, and there the lock is asked of alone, before the
// file is opened; where the broker has not, the open meets it.
//
// Mutation: refuse a reader on a node that has run whatever its lock says, and
// the run on a running node that has run is refused as though it were
// stopped.
func TestARunOnARunningNodeReadsTheFleetThroughIt(t *testing.T) {
	t.Parallel()
	for _, ran := range []bool{false, true} {
		t.Run(fmt.Sprintf("broker has run %v", ran), func(t *testing.T) {
			t.Parallel()
			node := newRevealNode(t, map[string]string{"GITLAB_SIGNING_SECRET": storedSigningSecret,
				"UNREAD": "x"})
			cfg := bootstrapForURL(t, node.server.URL)
			seedTheStore(t, cfg)
			if ran {
				markBrokerRun(t, cfg)
			}
			holdTheStore(t, storePathOf(t, cfg), store.EstateNode)

			out, errs, err := cli(t, "gitlab", "provision", gitlabCompanyFile(t), "-dry-run",
				"-config", cfg, "-public-url", "https://crewlet.example.com")
			if err != nil {
				t.Fatalf("a dry run on a running node failed: %v\n%s", err, errs)
			}
			if !strings.Contains(out, "reused — the configured one already resolves") {
				t.Errorf("the plan did not reuse the fleet's signing secret:\n%s", out)
			}
			asked, gets := node.batches()
			if want := []string{"GITLAB_SIGNING_SECRET"}; len(asked) != 1 ||
				!slices.Equal(asked[0], want) || len(gets) != 0 {
				t.Errorf("asked %v and %v, want one batch of %v", asked, gets, want)
			}
			for _, token := range node.tokens {
				if token != "Bearer "+cliFixtureToken {
					t.Errorf("a request carried %q, not the operator's own token", token)
				}
			}
		})
	}
}

// A READ THE NODE REFUSES SAYS BOTH WHY, AND WHAT THE READ NEEDS.
//
// A machine token is refused every reveal, whatever it carries, and the
// command has no other way to the values while the engine runs. So the lock
// and the refusal are both in the error, with the token a reveal takes — and
// stopping the engine is NOT offered: once the node has run, its own table is
// not where the fleet's values are. Nothing is printed: the plan the refused
// values would have decided is not.
func TestARefusedReadThroughTheNodeSaysBothWhyAndWhatItTakes(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"GITLAB_SIGNING_SECRET": storedSigningSecret})
	node.refuse = http.StatusForbidden
	cfg := bootstrapForURL(t, node.server.URL)
	seedTheStore(t, cfg)
	holdTheStore(t, storePathOf(t, cfg), store.EstateNode)

	out, _, err := cliIn(t, tokenEnviron("pat-of-a-person"), "gitlab", "provision",
		gitlabCompanyFile(t), "-dry-run", "-config", cfg,
		"-public-url", "https://crewlet.example.com")
	if err == nil {
		t.Fatalf("a refused reveal resolved anyway:\n%s", out)
	}
	if !errors.Is(err, store.ErrLocked) {
		t.Fatalf("the lock's sentinel was lost: %v", err)
	}
	for _, want := range []string{storePathOf(t, cfg), cfg, "403", apiTokenEnv, "Tier A"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "stop `crewlet run`") {
		t.Errorf("the refusal offers stopping the engine: %v", err)
	}
	if out != "" {
		t.Errorf("a run whose read was refused printed:\n%s", out)
	}
}

// A RUN THAT CANNOT GET THROUGH A RUNNING NODE IS TOLD ITS OWN WAY PAST.
//
// The refusal used to be the one written for `crewlet secrets set`: stop the
// engine, use "an access token" (which the reveal every one of these runs
// makes refuses), or let the environment stand in (which a routed read never
// does). Through the lock and through -api alike, it now names the bearer the
// reveal takes and the node to take it to instead, and asks the node nothing.
func TestARunReadingTheCompanysValuesIsToldItsOwnWayPast(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, nil)
	cfg := bootstrapForURL(t, node.server.URL)
	seedTheStore(t, cfg)
	holdTheStore(t, storePathOf(t, cfg), store.EstateNode)
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
	if len(node.tokens) != 0 {
		t.Errorf("a run with no bearer asked the node %d time(s)", len(node.tokens))
	}
}

// COMMANDS ON A RUNNING NODE DO NOT DIE ON ITS LOCK.
//
// `slack provision` with no seat to provision reads none of the company's
// values and asks the store nothing, so against a running node it neither
// meets the lock nor needs a credential for the node. `llm list` reads the
// company's values and so reads them through the node, as every provisioning
// command does — the entry's conventional bundle among them, in one batch.
func TestCommandsOnARunningNodeDoNotDieOnItsLock(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"CREWLET_LLM_CLI_SUB_CREDENTIALS": aBundle(t)})
	cfg := bootstrapForURL(t, node.server.URL)
	seedTheStore(t, cfg)
	holdTheStore(t, storePathOf(t, cfg), store.EstateNode)

	// WITH NO CREDENTIAL FOR THE NODE AT ALL: a run that routed its store
	// would need one to reach the node it is going through, so succeeding
	// without one is what shows nothing was opened.
	if out, errs, err := cliIn(t, noEnviron, "slack", "provision", gitlabCompanyFile(t), "-print",
		"-config", cfg, "-ledger", filepath.Join(t.TempDir(), "ledger.json")); err != nil {
		t.Errorf("slack provision on a running node: %v\n%s%s", err, out, errs)
	}
	if len(node.tokens) != 0 {
		t.Errorf("a command that reads no value asked the node %d time(s)", len(node.tokens))
	}

	company, entry := cliAgentCompanyFile(t)
	if out, errs, err := cli(t, "llm", "list", "-company", company, "-config", cfg); err != nil {
		t.Errorf("llm list on a running node: %v\n%s%s", err, out, errs)
	}
	asked, gets := node.batches()
	if len(asked) != 1 || !slices.Equal(asked[0], []string{"CREWLET_LLM_CLI_SUB_CREDENTIALS"}) ||
		len(gets) != 0 {
		t.Fatalf("llm list asked %v and %v, want one batch holding the entry's bundle", asked, gets)
	}
	if has, _ := entry.Workspace().HasLogin(t.Context()); !has {
		t.Error("the fleet's bundle was not restored into the entry, which the engine would do")
	}
}

// A SCRATCH NODE IS REFUSED BY THE COMMAND THAT WAS RUN.
//
// A node without `data` keeps no rows to read or write offline, and the
// refusal used to name `crewlet secrets` whatever the operator ran — sending
// somebody running a provisioner off to a command they had not typed.
func TestAScratchStoreIsRefusedByTheCommandThatWasRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := seatsNodeTierA(t, dir)
	company, _ := cliAgentCompanyFile(t)
	for _, args := range [][]string{
		{"gitlab", "provision", gitlabCompanyFile(t), "-dry-run", "-config", cfg},
		{"llm", "list", "-company", company, "-config", cfg},
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

// A -secret-store SINK READS BACK THROUGH THE RUN'S ROUTE, AND FROM ITS FIRST
// BATCH.
//
// What a provisioning sink reads back — whether a seat holds a token — is
// answered from the store the run's reads went through, wherever that is:
// this node's rows before its first start, a node named with -api from a
// machine with or without a Tier A. Held means held and a miss means not held.
// Through a node the names the run read up front are answered from that batch
// with no request of their own, a held name it did not read is one name in one
// more batch, and an unheld one costs nothing — never a GET reveal per seat.
func TestASecretStoreSinkReadsBackThroughTheRunsRoute(t *testing.T) {
	t.Parallel()
	held := map[string]string{"GITLAB_TOKEN_SWE": "glpat-held", "GITLAB_TOKEN_CTO": "glpat-cto"}
	readBack := func(t *testing.T, sink provision.TokenSink) {
		t.Helper()
		if value, ok, err := sink.Value(t.Context(), "GITLAB_TOKEN_SWE"); err != nil ||
			!ok || value != "glpat-held" {
			t.Errorf("a held token read back as %q, %v, %v", value, ok, err)
		}
		if value, ok, err := sink.Value(t.Context(), "GITLAB_TOKEN_NEW"); err != nil || ok {
			t.Errorf("a token nothing holds read back as %q, %v, %v", value, ok, err)
		}
	}
	t.Run("this node's rows before its first start", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapWithKeyring(t, "k1")
		for name, value := range held {
			if _, errs, err := secretsCmd(t, cfg, "set", name, "-value", value); err != nil {
				t.Fatalf("seed: %v (%s)", err, errs)
			}
		}
		readBack(t, secretStoreSink(t, noEnviron, "-config", cfg))
	})
	for name, withTierA := range map[string]bool{
		"a named node, with a Tier A here": true,
		"a named node, with no Tier A":     false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node := newRevealNode(t, maps.Clone(held))
			fs := flag.NewFlagSet("provision", flag.ContinueOnError)
			sinks := addSinkFlags(fs)
			args := []string{"-secret-store", "-api", node.server.URL}
			if withTierA {
				args = append(args, "-config", bootstrapWithKeyring(t, "k1"))
			}
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			if !withTierA {
				*sinks.store.bootstrap = filepath.Join(t.TempDir(), "absent.yaml")
			}
			companySecrets, err := openCompanyStore(tokenEnviron(cliFixtureToken), sinks.store,
				"crewlet gitlab provision", readsTheCompany)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(companySecrets.Close)
			if _, err := companySecrets.resolver(t.Context(),
				[]string{"GITLAB_TOKEN_SWE", "GITLAB_TOKEN_NEW"}, io.Discard); err != nil {
				t.Fatal(err)
			}
			sink, err := sinks.open(t.Context(), companySecrets, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			readBack(t, sink)
			if asked, gets := node.batches(); len(asked) != 1 || len(gets) != 0 {
				t.Errorf("the read-backs asked %v and %v, want them answered from the run's "+
					"one batch", asked, gets)
			}
			if value, ok, err := sink.Value(t.Context(), "GITLAB_TOKEN_CTO"); err != nil ||
				!ok || value != "glpat-cto" {
				t.Errorf("a held token the run did not read read back as %q, %v, %v",
					value, ok, err)
			}
			if asked, gets := node.batches(); len(asked) != 2 ||
				!slices.Equal(asked[1], []string{"GITLAB_TOKEN_CTO"}) || len(gets) != 0 {
				t.Errorf("a read-back beyond the batch asked %v and %v, want one more batch "+
					"of that name", asked, gets)
			}
			if err := sink.Record(t.Context(), "GITLAB_TOKEN_NEW", "glpat-minted"); err != nil {
				t.Fatal(err)
			}
			if value, ok, _ := sink.Value(t.Context(), "GITLAB_TOKEN_NEW"); !ok || value != "glpat-minted" {
				t.Errorf("a token this run recorded read back as %q, %v", value, ok)
			}
			if stored, _ := node.holds("GITLAB_TOKEN_NEW"); stored != "glpat-minted" {
				t.Errorf("the record did not reach the named node: %q", stored)
			}
		})
	}
}

// A PROVISIONING RUN RECORDING INTO THE STORE ASKS FOR ITS SEATS' TOKENS IN
// ITS FIRST BATCH: the sink reads each one back to keep a working token, so a
// run through a node asks for them beside the values it resolves, before it
// does anything — and with its flag for a fresh value, which reads none back,
// it asks for none. Each run here stops after the reads, before it reaches the
// vendor: GitLab and Mattermost at the administrator token they were not
// given — the invocation's environment holds none, whatever the process
// exports ([operatorCredential]) — Slack at the deployment address no Tier A
// and no -public-url gives it.
func TestAProvisioningRunAsksForItsSeatsTokensInItsFirstBatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		args  func(vendor string) []string
		want  []string
		fresh string
		stop  string
	}{
		{"gitlab provision", func(vendor string) []string {
			return []string{"gitlab", "provision", companyAt(t, readerCompanyDoc, vendor),
				"-public-url", "https://crewlet.example.com"}
		}, []string{"GITLAB_SIGNING_SECRET", "GITLAB_TOKEN_CEO"}, "-rotate", "no administrator token"},
		{"mattermost provision", func(vendor string) []string {
			return []string{"mattermost", "provision", companyAt(t, readerCompanyDoc, vendor)}
		}, []string{"MM_TOKEN_CEO"}, "-rotate", "no administrator token"},
		{"slack provision", func(vendor string) []string {
			return []string{"slack", "provision", companyAt(t, readerCompanyDoc, vendor)}
		}, []string{"SLACK_BOT_TOKEN_CEO"}, "-reinstall", "no address for this deployment"},
	} {
		for _, fresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, fresh %v", tc.name, fresh), func(t *testing.T) {
				held := map[string]string{"GITLAB_SIGNING_SECRET": storedSigningSecret,
					"GITLAB_TOKEN_CEO": "glpat-ceo", "MM_TOKEN_CEO": "mm-ceo",
					"SLACK_BOT_TOKEN_CEO": "xoxb-ceo"}
				node := newRevealNode(t, held)
				args := append(tc.args(untouchedVendor(t)), "-secret-store", "-api", node.server.URL,
					"-config", filepath.Join(t.TempDir(), "absent.yaml"))
				if fresh {
					args = append(args, tc.fresh)
				}
				_, _, err := cli(t, args...)
				if err == nil || !strings.Contains(err.Error(), tc.stop) {
					t.Fatalf("err = %v, want the run to stop at %q", err, tc.stop)
				}
				want := tc.want
				if fresh {
					want = slices.DeleteFunc(slices.Clone(want), func(name string) bool {
						return strings.Contains(name, "TOKEN")
					})
				}
				asked, gets := node.batches()
				if len(gets) != 0 || len(want) == 0 && len(asked) != 0 ||
					len(want) > 0 && (len(asked) != 1 || !slices.Equal(asked[0], want)) {
					t.Errorf("asked %v and %v, want one batch of %v", asked, gets, want)
				}
			})
		}
	}
}

// AN ENVIRONMENT-ONLY RUN DECIDES NO ROUTE. `crewlet llm list` with no Tier A
// at -config and no -api — a machine that is not a node, every reference
// exported — resolves from the environment alone, says so, and is never held
// to the stopped-node rule: there is no node here to be stopped.
//
// Mutation: route a run with no Tier A and no -api, and it is refused.
func TestAnEnvironmentOnlyRunDecidesNoRoute(t *testing.T) {
	t.Parallel()
	company, _ := cliAgentCompanyFile(t)
	absent := filepath.Join(t.TempDir(), "absent.yaml")
	out, errs, err := cli(t, "llm", "list", "-company", company, "-config", absent)
	if err != nil {
		t.Fatalf("llm list with no Tier A: %v\n%s", err, errs)
	}
	if !strings.Contains(errs, "no "+absent+": resolving ${VAR} from the environment only.") {
		t.Errorf("the run does not say it read the environment alone:\n%s", errs)
	}
	if !strings.Contains(out, "PROVIDER") {
		t.Errorf("the listing did not print:\n%s", out)
	}
}

// A SLACK RUN READS BACK ONLY THE SEATS IT PROVISIONS. A seat whose
// signing_secret is a literal names a bot_token variable, but the run takes it
// no further than its plan's note — nothing is created or installed for it —
// so its token is never read back, and through a node never revealed for a
// run that does not use it.
//
// Mutation: read back every seat whose bot_token is a variable, and the
// half-literal seat's token is asked for.
func TestASlackRunReadsBackOnlyTheSeatsItProvisions(t *testing.T) {
	t.Parallel()
	plans := []slack.SeatPlan{
		{Handle: "ceo", TokenVar: "SLACK_BOT_TOKEN_CEO", SigningVar: "SLACK_SIGNING_SECRET_CEO"},
		{Handle: "cto", TokenVar: "SLACK_BOT_TOKEN_CTO"},
	}
	for _, tc := range []struct {
		only, want []string
	}{
		{nil, []string{"SLACK_BOT_TOKEN_CEO"}},
		{[]string{"cto"}, nil},
		{[]string{"ceo", "cto"}, []string{"SLACK_BOT_TOKEN_CEO"}},
	} {
		if got := slackTokenVars(plans, tc.only); !slices.Equal(got, tc.want) {
			t.Errorf("-handles %v reads back %v, want %v", tc.only, got, tc.want)
		}
	}
}
