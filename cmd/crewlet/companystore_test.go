package main

import (
	"bytes"
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
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
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

// storePathOf is the database the Tier A at cfg names.
func storePathOf(t *testing.T, cfg string) string {
	t.Helper()
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return boot.Store.Path
}

// fleetRowsOf is every value the fleet's store holds, read straight off it the
// way a command reaches it on the node the Tier A at cfg describes with its
// engine stopped ([openFleetStore]).
//
// It starts a broker of its own on the node's store directory, so a case
// closes the run it inspects first: a second broker there is refused, in this
// process as in any other — and a value read once the run has let the store
// go is one that outlived the run that wrote it.
func fleetRowsOf(t *testing.T, cfg string) map[string]string {
	t.Helper()
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fs, closeFleet, err := openFleetStore(t.Context(), boot, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFleet()
	values, err := fleetSecrets(fs, boot)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := values.All(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rows
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
  sandbox: {fake: true}
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
  sandbox: {fake: true}
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
  # A person's seat: the custom entry stands in for a CLI this suite
  # cannot install, and it can run no seat's executor.
  - name: CEO
    handle: ceo
    kind: human
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
		return readerRun{name: strings.Join(append([]string{"llm", sub}, extra...), " "),
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

// A CLUSTER MEMBER WITH ITS ENGINE STOPPED REFUSES EVERY READER OF THE COMPANY,
// before it reads, plans or changes anything.
//
// Started alone, a member cannot reach the quorum the fleet's store needs, and
// beside running peers it would rejoin the cluster for the sake of one value,
// so its command is sent to a running member ([engine.ErrNoFleetStoreHere]).
// A run that went on without the store would read every value the fleet holds
// as unset — and unset is the signal a provisioner mints on, or a report
// restores the configured bundle on. So each such run is refused, naming
// `-api` and this node's Tier A, whatever flag for a fresh value it was given,
// and:
//
//   - nothing is printed but the error, on either stream: not a plan, nor a
//     route line;
//   - no vendor is reached;
//   - no broker is started on the member's store directory;
//   - an llm entry's credentials directory is left as it was.
//
// Mutation: let a reader on, past a store it could not reach, and every row
// goes red.
func TestAClusterMemberWithItsEngineStoppedRefusesEveryReaderOfTheCompany(t *testing.T) {
	t.Parallel()
	cfg := clusterMemberConfig(t)
	for _, reader := range readersOfTheCompany() {
		t.Run(reader.name, func(t *testing.T) {
			t.Parallel()
			vendor := untouchedVendor(t)
			args, entry := reader.args(t, cfg, vendor)
			out, errs, err := cli(t, args...)
			if !errors.Is(err, engine.ErrNoFleetStoreHere) {
				t.Fatalf("err = %v, want the cluster member's refusal\n%s%s", err, out, errs)
			}
			for _, want := range []string{"-api", cfg} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal omits %q: %v", want, err)
				}
			}
			if out != "" || errs != "" {
				t.Errorf("a refused run printed:\n%s%s", out, errs)
			}
			if entry != nil {
				if has, _ := entry.Workspace().HasLogin(t.Context()); has {
					t.Error("a refused report wrote a login into the entry's " +
						"credentials directory")
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "stream")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a broker was started on the member's store directory: %v", err)
	}
}

// A RUN THAT READS NO STORED VALUE DECIDES NO ROUTE, and so runs where every
// route is refused — a cluster member with its engine stopped: a dry run that
// resolves nothing, a plan with nothing in it — a Slack run with no seat to
// provision among them, whatever its sink. Refusing them would refuse a
// question the store has no part in, and routing them on a solo node would
// start its broker for a store nothing reads.
//
// A Slack seat whose credentials are literals is no seat to provision: the run
// leaves it to the operator who wrote them, and records or reads back nothing
// for it.
//
// Mutation: route before the empty-plan or dry-run return, or decide a Slack
// run's plan is empty by its seats rather than by those it can provision, and
// these go red.
func TestARunThatReadsNoStoredValueDecidesNoRoute(t *testing.T) {
	t.Parallel()
	cfg := clusterMemberConfig(t)
	vendor := untouchedVendor(t)
	company := companyAt(t, readerCompanyDoc, vendor)
	empty := companyAt(t, "name: Nimbus\nintegrations:\n  mattermost:\n    enabled: true\n"+
		"    url: VENDOR\n    team: nimbus\n  confluence:\n    url: VENDOR\n    email: ops@example.com\n"+
		"    token: t\n    webhook_secret: \"${CONFLUENCE_WEBHOOK_SECRET}\"\nroles:\n"+
		"  - name: CEO\n    handle: ceo\n", vendor)
	literal := companyAt(t, "name: Nimbus\nproviders:\n  sandbox: {fake: true}\n  llm:\n    main:\n      model: claude-sonnet-5\n"+
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

// A STOPPED NODE READS AND RECORDS THE FLEET'S STORE ITSELF.
//
// With the engine stopped there is no node to go through, and the store is
// still the fleet's: the command reaches it through this node's own broker
// ([openFleetStore]). A provisioning run reads what the store holds — a value
// set while the engine was stopped, as here, or through a running node — and
// says on its first line which store it reached, and how; its -secret-store
// sink records into that store, where the engine reads it at its next start.
//
// Mutation: answer a stopped node from anywhere but the fleet's store, and the
// plan mints over the stored key and the record lands nowhere the engine reads.
func TestAStoppedNodeReadsAndRecordsTheFleetsStoreItself(t *testing.T) {
	t.Parallel()
	t.Run("the store answers, and the run says how it got there", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapWithKeyring(t, "k1")
		if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_SIGNING_SECRET",
			"-value", storedSigningSecret); err != nil {
			t.Fatalf("seed: %v (%s)", err, errs)
		}
		out, errs, err := provisionCmd(t, gitlabCompanyFile(t), "-dry-run", "-config", cfg,
			"-public-url", "https://crewlet.example.com")
		if err != nil {
			t.Fatalf("a dry run with the engine stopped: %v\n%s", err, errs)
		}
		first, _, _ := strings.Cut(out, "\n")
		for _, want := range []string{"the fleet's store", "engine is stopped",
			"1 of the stored values"} {
			if !strings.Contains(first, want) {
				t.Errorf("the first line does not say %q: %q", want, first)
			}
		}
		if !strings.Contains(out, "reused — the configured one already resolves") {
			t.Errorf("the stored signing key did not reach the plan:\n%s", out)
		}
	})
	t.Run("a -secret-store record lands on the fleet's store", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapWithKeyring(t, "k1")
		sink, closeRun := secretStoreSink(t, noEnviron, "-config", cfg)
		if err := sink.Record(t.Context(), "GITLAB_TOKEN_SWE", "glpat-minted"); err != nil {
			t.Fatalf("record into the fleet's store: %v", err)
		}
		closeRun()
		if got := fleetRowsOf(t, cfg)["GITLAB_TOKEN_SWE"]; got != "glpat-minted" {
			t.Errorf("a -secret-store write with the engine stopped did not land on the "+
				"fleet's store: %q", got)
		}
	})
}

// secretStoreSink is a `-secret-store` sink opened as a provisioning run opens
// it, under the store flags given, and the run's release, for a case that
// reads the store once the run is over ([fleetRowsOf]).
func secretStoreSink(t *testing.T, environ config.Source, flags ...string) (provision.TokenSink, func()) {
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
		"crewlet gitlab provision")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(companySecrets.Close)
	sink, err := sinks.open(t.Context(), companySecrets, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return sink, companySecrets.Close
}

// A LOGIN WITH THE ENGINE STOPPED STORES ITS TOKEN ON THE FLEET'S STORE, and
// says who reads it when: this node's engine at its start, and a node already
// running on the same store at its next apply ([secretsStoppedNote]).
//
// Mutation: write it anywhere but the store the run read the company from, and
// the fleet's store does not hold it.
func TestALoginWithTheEngineStoppedStoresOnTheFleetsStore(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	store := llmStoreAt(t, noEnviron, "login", "-config", cfg)
	company := writeFile(t, t.TempDir(), "company.yaml",
		"name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n"+healthyDoctorEntry(t, "claude", "m")+
			"roles:\n  - name: CEO\n    handle: ceo\n    kind: human\n")
	var notes bytes.Buffer
	doc, keys, resolver, err := loadLLMConfig(t.Context(), company, store, "login", "claude", &notes)
	if err != nil {
		t.Fatalf("a login with the engine stopped was refused: %v", err)
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
	if !strings.Contains(out.String(), secretsStoppedNote) {
		t.Errorf("the write does not say who reads it when:\n%s", out.String())
	}
	store.Close()
	if got := fleetRowsOf(t, cfg)["FAKE_CLI_TOKEN"]; got != "fake-pasted" {
		t.Errorf("the token did not land on the fleet's store: %q", got)
	}
}

// A BROKER LOGIN OR AN IMPORT NAMES THE DOCTOR AS IT CAN BE RUN FROM WHERE IT
// RAN, with the -company, -config and -api the run was given: the doctor reads
// the company document and the store the run did, and a bare one after a run
// through -api reads this machine's Tier A instead, which may be another
// node's, or another store altogether.
//
// Mutation: leave out a flag the run was given, and every row does, the -api
// one beside a Tier A among them.
func TestALoginNamesTheDoctorAsItCanBeRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  func(t *testing.T) string
		// api is whether the run names a node with -api.
		api bool
	}{
		{name: "-api beside a Tier A", cfg: func(t *testing.T) string {
			return bootstrapWithKeyring(t, "k1")
		}, api: true},
		{name: "a node whose engine is stopped", cfg: func(t *testing.T) string {
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
				if last := lines[len(lines)-1]; last != "Verify it with `"+doctor+"`." {
					t.Fatalf("the run does not end on the doctor with its own flags, `%s`:\n%s",
						doctor, out)
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
  sandbox: {fake: true}
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
  # A person's seat: the custom entry stands in for a CLI this suite
  # cannot install, and it can run no seat's executor.
  - name: CEO
    handle: ceo
    kind: human
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

// A MISTAKE IN THE COMMAND LINE IS ANSWERED AS ONE, before any route is
// decided: on a cluster member whose engine is stopped, where every route is
// refused, a mistyped subcommand, a missing name or a `get` without -reveal is
// told what is wrong with it — not to name a running member or start the
// engine, which would not fix it. (On a solo node, a route would start its
// broker for a command line that was never going to run.)
//
// Mutation: check the command line after the store is routed, and each of
// these gets the cluster member's refusal instead.
func TestASecretsUsageErrorIsAnsweredBeforeAnyRoute(t *testing.T) {
	t.Parallel()
	cfg := clusterMemberConfig(t)
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
			if err == nil || errors.Is(err, engine.ErrNoFleetStoreHere) ||
				!strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want %q", err, tc.says)
			}
		})
	}
}

// A PROVISIONING RUN'S CHOICE OF SINK IS ANSWERED AS A MISTAKE IN ITS COMMAND
// LINE, before any route is decided: on a cluster member whose engine is
// stopped, where every real provisioning run is refused, a run naming no sink,
// or two, is told to name exactly one — not to name a running member, which
// would not fix it — and reaches no vendor.
//
// Mutation: check the sink only where it is opened, after the route, and the
// GitLab, GitHub, Jira and Confluence rows get the cluster member's refusal
// instead.
func TestAProvisioningRunsChoiceOfSinkIsAnsweredBeforeAnyRoute(t *testing.T) {
	t.Parallel()
	cfg := clusterMemberConfig(t)
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
				if err == nil || errors.Is(err, engine.ErrNoFleetStoreHere) ||
					!strings.Contains(err.Error(), tc.says) {
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
// The engine holds this node's database for as long as it runs, so the lock
// routes the whole run through the node's API — a dry run included — and the
// values come from the fleet's store: the listing, then one batch of exactly
// the names the command reads that the node lists, under the operator's own
// token.
//
// Mutation: reach the store directly whatever the lock says, and the run is
// refused a broker on the directory the engine holds — or, here, reads no
// value the node holds.
func TestARunOnARunningNodeReadsTheFleetThroughIt(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"GITLAB_SIGNING_SECRET": storedSigningSecret,
		"UNREAD": "x"})
	cfg := bootstrapForURL(t, node.server.URL)
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
}

// A READ THE NODE REFUSES SAYS BOTH WHY, AND WHAT THE READ NEEDS.
//
// A machine token is refused every reveal, whatever it carries, and the
// command has no other way to the values while the engine runs. So the lock
// and the refusal are both in the error, with the token a reveal takes — and
// stopping the engine is NOT offered: it would let the command reach the store
// itself, but a node taken down to read a value is an outage to save a token.
// Nothing is printed: the plan the refused values would have decided is not.
func TestARefusedReadThroughTheNodeSaysBothWhyAndWhatItTakes(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"GITLAB_SIGNING_SECRET": storedSigningSecret})
	node.refuse = http.StatusForbidden
	cfg := bootstrapForURL(t, node.server.URL)
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
// engine (an outage to read a value), use "an access token" (which the reveal
// every one of these runs makes refuses), or let the environment stand in
// (which a routed read never does, and the store outranks). Through the lock
// and through -api alike, it now names the bearer the reveal takes and the
// node to take it to instead, and asks the node nothing.
func TestARunReadingTheCompanysValuesIsToldItsOwnWayPast(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, nil)
	cfg := bootstrapForURL(t, node.server.URL)
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
// A node without `data` has no store of the company's to reach — the fleet's
// is its members' — and the refusal used to name `crewlet secrets` whatever
// the operator ran, sending somebody running a provisioner off to a command
// they had not typed.
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
// the fleet's store reached directly with this node's engine stopped, a node
// named with -api from a machine with or without a Tier A. Held means held and
// a miss means not held.
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
	t.Run("the fleet's store, reached directly", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapWithKeyring(t, "k1")
		for name, value := range held {
			if _, errs, err := secretsCmd(t, cfg, "set", name, "-value", value); err != nil {
				t.Fatalf("seed: %v (%s)", err, errs)
			}
		}
		sink, _ := secretStoreSink(t, noEnviron, "-config", cfg)
		readBack(t, sink)
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
				"crewlet gitlab provision")
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
// it asks for none. Slack reads back only the token of an app its ledger
// records installed, which is the only one an install can spare, so a run
// whose app nobody has installed asks for none either, whatever the store
// holds. Each run here stops after the reads, before it reaches the vendor:
// GitLab and Mattermost at the provisioning token they were not given — the
// invocation's environment holds none, whatever the process exports
// ([operatorCredential]) — Slack at the deployment address no Tier A and no
// -public-url gives it.
func TestAProvisioningRunAsksForItsSeatsTokensInItsFirstBatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		args  func(vendor string) []string
		want  []string
		fresh string
		stop  string
	}{
		// GITLAB READS NO SEAT TOKEN: an agent's token is per-seat access,
		// minted by the engine, so the run asks for the signing secret alone.
		{"gitlab provision", func(vendor string) []string {
			return []string{"gitlab", "provision", companyAt(t, readerCompanyDoc, vendor),
				"-public-url", "https://crewlet.example.com"}
		}, []string{"GITLAB_SIGNING_SECRET"}, "-rotate", "no provisioning token"},
		{"mattermost provision", func(vendor string) []string {
			return []string{"mattermost", "provision", companyAt(t, readerCompanyDoc, vendor)}
		}, []string{"MM_TOKEN_CEO"}, "-rotate", "no administrator token"},
		{"slack provision", func(vendor string) []string {
			return []string{"slack", "provision",
				installedSlackApps(t, companyAt(t, readerCompanyDoc, vendor), "ceo")}
		}, []string{"SLACK_BOT_TOKEN_CEO"}, "-reinstall", "no address for this deployment"},
		{"slack provision of an app nobody installed", func(vendor string) []string {
			return []string{"slack", "provision", companyAt(t, readerCompanyDoc, vendor)}
		}, nil, "-reinstall", "no address for this deployment"},
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
// exported — resolves from the environment alone, says so, and reaches no
// store: there is no node here, and no broker to start.
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

// A SLACK RUN READS BACK ONLY THE INSTALLED APPS IT PROVISIONS. A seat whose
// signing_secret is a literal names a bot_token variable, but the run takes it
// no further than its plan's note — nothing is created or installed for it —
// so its token is never read back, and through a node never revealed for a
// run that does not use it. Nor is the token of a seat whose app the ledger
// does not record installed — recorded with no bot user, or not recorded at
// all: that app is installed whatever the store holds, so a reveal of its
// token answers nothing the run uses, and a refused one would refuse the run.
//
// Mutation: read back every seat whose bot_token is a variable, and the
// half-literal seat's token is asked for; read back every seat the run
// provisions, and the two uninstalled seats' tokens are.
func TestASlackRunReadsBackOnlyTheInstalledAppsItProvisions(t *testing.T) {
	t.Parallel()
	plans := []slack.SeatPlan{
		{Handle: "ceo", TokenVar: "SLACK_BOT_TOKEN_CEO", SigningVar: "SLACK_SIGNING_SECRET_CEO"},
		{Handle: "cto", TokenVar: "SLACK_BOT_TOKEN_CTO"},
		{Handle: "cfo", TokenVar: "SLACK_BOT_TOKEN_CFO", SigningVar: "SLACK_SIGNING_SECRET_CFO"},
		{Handle: "coo", TokenVar: "SLACK_BOT_TOKEN_COO", SigningVar: "SLACK_SIGNING_SECRET_COO"},
	}
	ledger := &slack.Ledger{Apps: map[string]slack.AppRecord{
		"ceo": installedSlackRecord("ceo"),
		"cto": installedSlackRecord("cto"),
		// CREATED AND NEVER INSTALLED: a -no-install run's, or one whose
		// operator skipped the click.
		"cfo": {AppID: "A0CFO", ClientID: "1.2", ClientSecret: "cs", SigningSecret: "ss"},
	}}
	for _, tc := range []struct {
		only, want []string
	}{
		{nil, []string{"SLACK_BOT_TOKEN_CEO"}},
		{[]string{"cto"}, nil},
		{[]string{"cfo", "coo"}, nil},
		{[]string{"ceo", "cto", "cfo"}, []string{"SLACK_BOT_TOKEN_CEO"}},
	} {
		if got := slackTokenVars(plans, ledger, tc.only); !slices.Equal(got, tc.want) {
			t.Errorf("-handles %v reads back %v, want %v", tc.only, got, tc.want)
		}
	}
}

// installedSlackRecord is the ledger's record of handle's app as a run that
// completed its OAuth exchange leaves it.
func installedSlackRecord(handle string) slack.AppRecord {
	id := strings.ToUpper(handle)
	return slack.AppRecord{AppID: "A0" + id, ClientID: "1.2", ClientSecret: "cs",
		SigningSecret: "ss", BotUserID: "U0BOT" + id, TeamID: "T0ACME"}
}

// installedSlackApps writes the ledger beside company, recording each of
// handles' apps installed.
func installedSlackApps(t *testing.T, company string, handles ...string) string {
	t.Helper()
	ledger := &slack.Ledger{Apps: map[string]slack.AppRecord{}}
	for _, handle := range handles {
		ledger.Apps[handle] = installedSlackRecord(handle)
	}
	if err := ledger.Save(slack.LedgerPathFor(company)); err != nil {
		t.Fatal(err)
	}
	return company
}

// A -secret-store RUN THROUGH A RUNNING NODE HAS NOTHING LEFT TO DO, AND ONE
// WITH THE ENGINE STOPPED STILL DOES.
//
// A run through the node re-activates the revision that reads the values it
// sealed once, at its flush (ADR-0070), so a run through it that said
// "re-activate the current revision" sent an operator to move the fleet onto
// one more epoch for nothing. With the engine stopped no node re-activated anything, and a peer
// running on the same store reads the values only at its next apply.
func TestASecretStoreRunsNextStepFollowsTheRouteItWrote(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{})
	through, _ := secretStoreSink(t, tokenEnviron(cliFixtureToken), "-api", node.server.URL)
	if step := through.NextStep(); !strings.Contains(step, "the node re-activates") ||
		strings.Contains(step, "/config/reload") {
		t.Errorf("through a node the next step is %q", step)
	}
	stopped, _ := secretStoreSink(t, noEnviron, "-config", bootstrapWithKeyring(t, "k1"))
	if step := stopped.NextStep(); !strings.Contains(step, "POST /config/reload") {
		t.Errorf("with the engine stopped the next step is %q", step)
	}
}
