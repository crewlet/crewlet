// Command crewlet is the engine binary.
//
// One binary is the whole product: it runs agents, serves the API and
// dashboard, and — in the default solo topology — embeds its own broker and
// database, so a company runs with no external services at all. What a node
// does is a config value (node.roles), not a different command.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcprelay"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/slack"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/version"
	"github.com/crewlet/crewlet/static"

	"gopkg.in/yaml.v3"
)

func main() {
	// THE LANDLOCK HELPER, before anything else runs: a confined host box's
	// every command is this binary re-executed to restrict itself and then
	// become the box's job (internal/sandbox's ConfineMain). It is no
	// operator's command, so it is answered here, ahead of the dispatch and
	// its usage, and it never returns on success.
	if len(os.Args) > 1 && os.Args[1] == sandbox.ConfineCommand {
		os.Exit(sandbox.ConfineMain(os.Args[2:]))
	}
	// WHERE THIS PROCESS'S LOGS GO, decided by the one function that owns
	// the process. Everything below adjusts only the level and the format
	// (logging.SetVerbosity), because a command that installed a
	// destination would be installing whatever writer its caller handed
	// it — which under `go test` is one test's buffer, shared with every
	// other test in the binary.
	//
	// Not redundant with logging's own init default: this is the
	// statement of intent, and an init that changed would otherwise
	// change the CLI silently.
	logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr)
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		// errSilent has already said everything it has to say, on stdout,
		// in the shape its caller asked for. Printing a second copy on
		// stderr is what makes a machine consumer's log unreadable.
		if errors.Is(err, errSilent) {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "crewlet:", err)
		os.Exit(1)
	}
}

// run is main's testable body: it takes its arguments and streams rather
// than reaching for globals, so the CLI surface can be exercised in tests —
// and it runs the invocation against THIS PROCESS'S environment, which is
// where an operator's credential comes from, and Slack's own Web API
// ([runIn]).
func run(args []string, stdout, stderr io.Writer) error {
	return runIn(invocation{environ: config.EnvSource{}}, args, stdout, stderr)
}

// invocation is what one run of a command is HANDED rather than reads from
// the process it runs in ([runIn]).
type invocation struct {
	// environ is where a command that talks to a running node reads its
	// credential ([nodeTokenOrEmpty]), and where a provisioning command
	// reads the operator's own ([operatorCredential]).
	environ config.Source
}

// runIn is [run] against an invocation it is HANDED: the environment every
// command that talks to a running node reads its credential from
// ([nodeTokenOrEmpty]), and a provisioning command the operator's own, and the
// client Slack's Web API is reached through.
//
// The process's own environment and the default client for every run of the
// binary, so the rule the credential follows — `CREWLET_API_TOKEN` and
// nothing else — is unchanged. They are parameters because the process is
// shared by everything running in it: a suite running its commands side by
// side could only hand each one a credential by setting the process's
// variable, which Go refuses to a parallel test and which would hand every
// concurrent command whichever case set it last; and a case that read the
// process's would read whatever the machine running the suite exports — an
// operator's own Slack refresh token among them, which a run that reached
// slack.com would spend. The engine is handed its environment for the same
// reason ([engine.Options.Environment]).
//
// THE CREDENTIALS, AND NOT THE LOGGING KNOBS: `CREWLET_LOG_LEVEL`,
// `CREWLET_LOG_FORMAT` and `CREWLET_LOG_FILE` configure the process's one
// logger, which is the process's state rather than the invocation's, so they
// are read from the process.
func runIn(inv invocation, args []string, stdout, stderr io.Writer) (err error) {
	// A READ OF A STORED VALUE THAT FAILED PART WAY THROUGH A COMMAND stops
	// it with a panic, because the lookup that made it has no error to return
	// and its only other answer, unset, is the one that mints over a working
	// secret ([readFailed]). It is the command's failure, and answered here
	// as one; any other panic is a bug, and goes on as it came.
	defer func() {
		if r := recover(); r != nil {
			failed, ok := r.(readFailed)
			if !ok {
				panic(r)
			}
			err = failed
		}
	}()
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no command given")
	}

	cmd, rest := args[0], args[1:]
	environ := inv.environ

	// THE OPERATOR COMMANDS ARE QUIET BY DEFAULT. They open a store, which
	// logs a migration line per schema file and an open line per call —
	// noise on a one-shot command whose stdout is meant to be piped, read
	// or diffed. `run` configures its own level from its own flag, and
	// nothing here silences a WARNING.
	//
	// QUIET IS A DEFAULT, NOT A CEILING. A half-applied migration or a
	// deploy gate that failed is exactly the run whose detail an operator
	// needs, and pinning every non-`run` command at warn with no override
	// left them nothing to turn up. $CREWLET_LOG_LEVEL is the escape
	// hatch, and it is an environment variable rather than a flag on nine
	// commands because it belongs to the INVOCATION rather than to any one
	// of them — a CI step exports it once and every command it runs
	// answers.
	//
	// NOR FOR THE RELAY, which sets up nothing at all. It is the process a
	// coding CLI starts as a seat's MCP server, so its stdout IS the protocol
	// stream and its environment is whatever the CLI chose to pass: the log
	// level, format and file below would read variables it must not depend on.
	// See internal/mcprelay.
	if cmd != "run" && cmd != mcprelay.Command {
		// THE LEVEL, NOT THE DESTINATION. This used to install `stderr`
		// as the process-wide sink, which is wrong twice over: `run`
		// takes that writer so it can be TESTED, and a function that
		// mutates process state out of one of its own arguments cannot
		// be called twice concurrently. Under `go test` it was 29
		// parallel tests pointing the global at their own buffers — a
		// data race, and one test's log lines landing in another's
		// output. See [logging.Configure].
		logging.SetVerbosity(operatorLogLevel(), operatorLogFormat())
		// THE FILE OPENS BEFORE THE FIRST THING THESE COMMANDS LOG, which
		// is the warning below: a misspelled $CREWLET_LOG_LEVEL is part of
		// the command's log, and a file attached afterwards would hold
		// every line of it except the one saying the invocation was wrong.
		//
		// AND THE SAME LEVER OVER WHERE THEY WRITE. $CREWLET_LOG_FILE is
		// the third sibling of the two above and exists for the one
		// reason they do: these commands take no logging flags, so a CI
		// step that wants a `crewlet migrate` in the same durable record
		// as the node it is migrating for has no other way to ask. The
		// rotation caps are not env knobs — they belong to a deployment's
		// file, and a one-shot command does not reach one of them.
		// STDERR STAYS ON for these: they read no `logging:` block, so
		// nothing has told them otherwise, and a one-shot command whose
		// output vanished into a file would be the opposite of what a
		// person at a terminal wants.
		detach, err := attachLogFile(config.LogFileSettings{
			Open: logging.FileOptions{Path: os.Getenv("CREWLET_LOG_FILE")},
		}, true, os.Stderr)
		if err != nil {
			return err
		}
		defer detach()
		warnUnrecognisedLogNames(logging.Get("cli"), "environment",
			"CREWLET_LOG_LEVEL", os.Getenv("CREWLET_LOG_LEVEL"),
			"CREWLET_LOG_FORMAT", os.Getenv("CREWLET_LOG_FORMAT"))
	}

	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "crewlet %s", version.String())
		if rev := version.Revision(); rev != "" {
			fmt.Fprintf(stdout, " (%s)", rev)
		}
		fmt.Fprintln(stdout)
		return nil
	case "help", "--help", "-h":
		usage(stdout)
		return flag.ErrHelp
	case "run":
		return runEngine(rest, stderr, bindTCP)
	case "validate":
		return validateConfigs(rest, stdout, stderr)
	case "schema":
		return emitSchema(rest, stdout, stderr)
	case "secrets":
		return runSecrets(environ, rest, stdout, stderr)
	case "config":
		return runConfig(environ, rest, stdout, stderr)
	case "iam":
		return runIAM(environ, rest, os.Stdin, stdout, stderr)
	case "migrate":
		return runMigrate(rest, stdout, stderr)
	case "budgets":
		return runBudgets(environ, rest, stdout, stderr)
	case "backup":
		return runBackup(environ, rest, stdout, stderr)
	case "retention":
		return runRetention(environ, rest, stdout, stderr)
	case "worklet":
		return runWorklet(environ, rest, stdout, stderr)
	case "objects":
		return runObjects(environ, rest, stdout, stderr)
	case "fleet":
		return runFleet(environ, rest, stdout, stderr)
	case "seats":
		return runSeats(environ, rest, stdout, stderr)
	case "artifacts":
		return runArtifacts(environ, rest, stdout, stderr)
	case "llm":
		return runLLM(environ, rest, stdout, stderr)
	case "search":
		return runSearch(rest, stdout, stderr)
	case "assistant":
		return runAssistant(environ, rest, stdout, stderr)
	case "mcp-relay":
		// The literal is mcprelay.Command, which the argv a CLI is handed is
		// built from; a case of the switch is read as the binary's command
		// list ([TestTheRelayIsDispatchedUnderItsOwnName] holds the two
		// together).
		return mcprelay.Run(rest, os.Stdin, stdout, stderr)
	case "gitlab", "github", "jira", "slack", "confluence", "mattermost":
		return runIntegration(inv, cmd, rest, stdout, stderr)
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `crewlet %s — an engine for hierarchically organized AI agent companies

Usage:
  crewlet run [flags]         Run the engine (API, agents and workers by default)
                              -roles data|ingress|seats|workers narrows what this node does
  crewlet validate [flags]    Check both config tiers without starting anything
  crewlet schema [tier]       Print a tier's JSON Schema (company by default)
  crewlet migrate [config]    Apply pending schema migrations (-check reports only)
  crewlet budgets show        Show the durable token counters: each scope's day,
                              week and month, its ceiling and its state
  crewlet backup -dir PATH    Copy this node's store and stream estate, through
                              the running engine, to a path on ITS host
  crewlet retention <cmd>     What the state log is holding, why it is not
                              shrinking, and the gestures that change it
  crewlet worklet <cmd>       The gestures on Worklet's work items that belong to
                              a person: purge, which destroys a task and every
                              row it produced and which nothing undoes
  crewlet objects status      Where the company's files are kept, and what the
                              object store's collector last found missing
  crewlet objects migrate     Move the company's files to an S3 bucket: copy on a
                              live fleet, then -final in maintenance mode
  crewlet fleet broker <cmd>  The fleet broker's members, as the nodes advertise them
                              and as its metadata group counts them: list, and
                              remove a member that is gone for good
  crewlet seats <cmd>         Pause a seat (-stop also ends its running turn and run)
                              or resume it: its holder, its lead, or an admin
  crewlet artifacts <cmd>     List, publish, version, share, trash and purge the
                              company's artifacts, as the token's principal
  crewlet secrets <cmd>       Read and rotate the encrypted secret store
  crewlet config <cmd>        Import, inspect and activate company revisions
  crewlet iam <cmd>           People, seats, invitations, permissions,
                              sessions and tokens
  crewlet llm <cmd>           Log in, verify and export the coding CLIs every model
                              runs through
  crewlet search eval         Measure the semantic search against the exact scan,
                              on the vectors a store file actually holds
  crewlet assistant eval      Run evaluation cases through the Dashboard Assistant,
                              on their own scripts or against a candidate model
  crewlet mcp-relay SOCKET    Not for people: the MCP server a seat's coding CLI
                              starts, relaying its stdio to the engine's tools
  crewlet gitlab <cmd>        Reconcile the company's seats into a GitLab instance
  crewlet github <cmd>        Report a GitHub deployment's seat accounts and hook it
  crewlet jira <cmd>          Report a Jira instance's seat accounts and projects
  crewlet slack <cmd>         Create, update and install one Slack app per seat
  crewlet confluence <cmd>    Register the inbound hooks; publish markdown and tool skills into spaces
  crewlet mattermost <cmd>    Reconcile a Mattermost team, and diagnose one
  crewlet version             Print the version
  crewlet help                Show this message

Config:
  -config   Tier A, this NODE: where its broker, store and API are (default %q)
  -company  Tier B, the COMPANY: its org, providers and integrations (default %q).
            A BOOTSTRAP seed: imported only where no company exists yet —
            none in this node's store and none the fleet runs. Otherwise it
            is ignored, loudly, and the node runs the company that exists,
            so a restart never reverts a live change. Not read at all where
            Tier A sets api.auth.company_writers.
  -import-company
            Tier B to make active NOW, over whatever the fleet is running.
            The deliberate "this file is the company again" gesture.
            Refused where Tier A sets api.auth.company_writers.
            To change a RUNNING fleet with no restart at all, use
            "crewlet config import" — it goes through the node's API.

Environment:
  %s
            The token for commands that talk to a running node (budgets,
            backup, retention, work, seats, artifacts, secrets, config,
            iam): one of the node's api.auth.tokens values, or an access
            token from "crewlet iam token". It's never a flag (flags end up
            in shell history) and never read from the config file.
`, version.String(), defaultBootstrapPath, defaultCompanyPath, apiTokenEnv)
}

// The two tiers are separate files because they answer to separate people. A
// node's broker address and store path are an operator's; the org chart and
// the model behind each seat are the company's.
//
// Tier A is read from its file on every boot and never from anywhere else —
// it is what tells the process where its store and broker are, so it cannot
// come from them.
//
// Tier B is different, and it is TWO flags because it was one job too many
// for one.
//
// A running node serves the revision the fleet's activation pointer names,
// not a file — that is what makes a PUT /config on one node reach every other.
// So a Tier B file on the command line is only ever a way of getting a
// document INTO that store, and there are two quite different reasons to want
// that:
//
//	-company        BOOTSTRAP. Imported only where no company exists yet:
//	                none in this node's store and none the fleet runs. Once
//	                the company exists, this file is ignored and the node
//	                boots on what the fleet is running.
//
//	-import-company OVERRIDE. Make this file the active revision now, over
//	                whatever the fleet is running.
//
// They were one flag with the override's behaviour, and it was the wrong
// default in the way that costs most: an operator edits their company live —
// through the dashboard, PUT /config, or `crewlet config import` — then
// restarts a node whose company.yaml still says what it said last month, and
// the file silently wins. A deleted role comes back, a changed model reverts,
// and nothing anywhere says so, because from the node's point of view it did
// exactly what it was told.
//
// Making the safe one the default does not resurrect the bug it was guarding
// against — an edited file being silently ignored — because the ignore is now
// LOUD (see reconcile.go) and the override is one flag away.
const (
	defaultBootstrapPath = "crewlet.yaml"
	defaultCompanyPath   = "company.yaml"
)

// configFlags is the pair every config-reading command takes.
type configFlags struct {
	bootstrap *string
	company   *string
}

func addConfigFlags(fs *flag.FlagSet) configFlags {
	return configFlags{
		bootstrap: fs.String("config", defaultBootstrapPath,
			"Tier A config: this node's broker, store and API"),
		company: fs.String("company", defaultCompanyPath,
			"Tier B config: the company's org, providers and integrations"),
	}
}

// tierBSeed is the Tier B document a `crewlet run` invocation carries, and
// what it is FOR.
//
// The store is authoritative at runtime, so a file on the command line is only
// ever a way of getting a document INTO it. Override says which of the two
// reasons this one is: bootstrap a company that does not exist yet, or replace
// what the fleet is running. Company is nil when there is no file to seed from
// at all.
//
// It is the seed as the command line RESOLVED it, and not yet what it does:
// that is [decideSeed]'s, whose [seedPlan] is the only thing the engine's boot
// and the import accept.
type tierBSeed struct {
	Path     string
	Override bool
	Company  *config.Company

	// Managed is a -company seed this deployment's Tier A rules out
	// (`api.auth.company_writers`, ADR-0030): Path names the file, which
	// was never read, and Company is nil. See [managedSeed].
	Managed bool
}

// loadForRun reads Tier A, and Tier B as the seed `run` will reconcile.
//
// It reports EVERY problem it can rather than the first, for the same reason
// [configFlags.load] does: an operator fixing a broker URL only to be told
// about their org chart on the next boot has been made to pay twice for one
// edit.
func (c configFlags) loadForRun(ctx context.Context, set *flag.FlagSet, importPath string) (
	*config.Bootstrap, tierBSeed, error,
) {
	// See [configFlags.load] for why Tier A resolves environment-only.
	boot, bootErr := config.LoadBootstrap(*c.bootstrap, config.EnvOnly())
	bootErr = nameTheNeighbour(*c.bootstrap, bootErr)

	// THE REVISION A REDACTED FILE WAS READ FROM is the company, which this
	// node's own store knows as the fleet's revision it last saw: a
	// company.yaml kept in sync from Copy as YAML or `GET /config` carries a
	// mask for every inline credential, and the file may start the node and
	// be imported over that company, so it is restored against it as
	// `crewlet config import` restores one ([config.LoadCompanyToRun],
	// [knownRevision]). Read only for a file with a mask in it, and never
	// from a scratch store, which holds the previous run's rows and is
	// deleted as the engine opens it. Read as the STORED form: a newer peer
	// may have written it.
	var prior config.PriorRevision
	if bootErr == nil && !boot.Store.Scratch {
		stored := storeAt(*c.bootstrap)
		prior = func() (*config.Company, error) {
			state, held, err := stored(ctx)
			// THE CLAIM GOES BACK AT ONCE: this read restores a redacted
			// file's masks, and the decision the engine boots on is the
			// one that holds the store across its own gap ([heldStore]).
			defer held.release()
			if err != nil || state.known == nil {
				return nil, err
			}
			if state.known.unopened != nil {
				return nil, state.known.unopened
			}
			company, err := config.DecodeCompany(state.known.document)
			if err != nil {
				return nil, fmt.Errorf("parse revision %s, the company this node knows: %w",
					state.known.revision.ID, err)
			}
			return company, nil
		}
	}
	// THE SEED IS RESOLVED AGAINST THE TIER A IT RUNS UNDER, when that
	// loaded: a managed deployment reads no seed at all ([managedSeed]).
	// When Tier A failed the run fails anyway, and the seed is resolved on
	// its own so that both documents' problems are reported at once.
	seedBoot := boot
	if bootErr != nil {
		seedBoot = nil
	}
	seed, seedErr := c.resolveSeed(set, importPath, seedBoot, prior)
	// TIER A COMES BACK EVEN WHEN THE SEED DID NOT. The two documents fail
	// independently, and a caller holding a valid Tier A can do something
	// with it before reporting the other one's error — `crewlet run` opens
	// the log file Tier A names, so a malformed company.yaml is recorded
	// there rather than only on a terminal. A nil Bootstrap beside a
	// non-nil error stays the contract when TIER A itself is what failed.
	if bootErr != nil {
		return nil, tierBSeed{}, errors.Join(bootErr, seedErr)
	}
	return boot, seed, seedErr
}

// resolveSeed decides which Tier B file this invocation carries and why, the
// file restored against prior where it is a redacted read.
//
// boot is the Tier A the run loaded, or nil when that failed: a managed
// deployment's seed is resolved without reading the file ([managedSeed]).
func (c configFlags) resolveSeed(set *flag.FlagSet, importPath string,
	boot *config.Bootstrap, prior config.PriorRevision) (tierBSeed, error) {

	named := strings.TrimSpace(importPath)
	if named != "" && isFlagSet(set, "company") {
		// REFUSED rather than resolved by precedence. The two flags ask
		// for opposite things — "only if the store is empty" and "even if
		// it is not" — so any winner is a guess about which one the
		// operator meant, made silently, about the flag that overwrites a
		// running company.
		return tierBSeed{}, errors.New(
			"-company and -import-company both name a Tier B document, and " +
				"they mean opposite things: -company bootstraps a company " +
				"that does not exist yet, -import-company replaces what the " +
				"fleet is running. Pass one")
	}
	if boot != nil && boot.API.Auth.CompanyManaged() {
		return managedSeed(boot, *c.company, isFlagSet(set, "company"), named)
	}
	// THE RUNNABLE RULES ONLY. Most boots import nothing from this file: see
	// [config.LoadCompanyToRun]. Where the file IS imported, seedCompany
	// holds it to the admission rules first.
	if named != "" {
		company, err := config.LoadCompanyToRun(named, prior)
		if err != nil {
			return tierBSeed{}, err
		}
		return tierBSeed{Path: named, Override: true, Company: company}, nil
	}

	path := *c.company
	company, err := config.LoadCompanyToRun(path, prior)
	if err == nil {
		return tierBSeed{Path: path, Company: company}, nil
	}
	// AN ABSENT DEFAULT IS NOT AN ERROR, but an absent NAMED file is.
	//
	// The store is authoritative, so a node whose company already lives
	// there needs no file at all — and the documented way to bootstrap one
	// is `crewlet run` followed by PUT /config, which cannot work if the
	// binary refuses to start without a document. But a path the operator
	// typed and that is not there is a typo, and booting past it would give
	// them a node quietly running something other than what they named.
	if isFlagSet(set, "company") || !errors.Is(err, fs.ErrNotExist) {
		return tierBSeed{}, err
	}
	return tierBSeed{Path: path}, nil
}

// nameTheNeighbour adds the one hint that answers the commonest first-run
// failure.
//
// `config.yaml` is the name this project's own guides used to give the Tier A
// document, and the bundled example still carries it
// (`examples/nimbus.config.yaml`), while the binary's default is
// `crewlet.yaml`. An operator with a file written against that guidance gets
// "no such file" about a name they never typed, with their file sitting right
// there. Naming it costs one stat and saves the whole diagnosis.
//
// A HINT, not a fallback: silently loading a file the operator did not ask
// for is how a node boots from the wrong document on a machine that has both.
func nameTheNeighbour(path string, err error) error {
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	neighbour := filepath.Join(filepath.Dir(path), "config.yaml")
	if filepath.Clean(neighbour) == filepath.Clean(path) {
		return err
	}
	if _, statErr := os.Stat(neighbour); statErr != nil {
		return err
	}
	return fmt.Errorf("%w\n  (%s is there — this build's Tier A default is %q; "+
		"pass it as `crewlet run %s` or with -config)",
		err, neighbour, defaultBootstrapPath, neighbour)
}

// Tier is which config document a validation is about.
//
// A NAMED TYPE with a closed set, because the value decides which validator
// runs and an unrecognised one must be a refusal rather than a silent fall
// through to "company" — a Tier A file validated as Tier B fails on every
// field it has, and the operator reads a wall of nonsense.
type Tier string

const (
	// TierAuto picks by reading the document — see [detectTier].
	TierAuto Tier = "auto"
	// TierCompany is Tier B: the company's org, providers, integrations.
	TierCompany Tier = "company"
	// TierBootstrap is Tier A: this node's broker, store and API.
	TierBootstrap Tier = "bootstrap"
)

// tiers is the closed set, for the flag's error and for validation.
var tiers = []Tier{TierAuto, TierCompany, TierBootstrap}

func (t Tier) Valid() bool {
	return slices.Contains(tiers, t)
}

// detectTier reads a document and says which tier it is.
//
// # By the keys it has, not by its filename
//
// A filename is a convention an operator can break and routinely does — and
// the one thing this must get right is the case where they named the file
// something else. The vocabularies come from the CONFIG STRUCTS themselves
// (config.CompanyKeys / config.BootstrapKeys), reduced to the keys unique to
// one tier.
//
// Restated by hand here, the list had drifted exactly as a restated list
// does: it counted `agents`, which appears nowhere in this tree, the schema,
// the examples or the docs, while omitting `roles`, `units`, `mcp_servers`,
// `turn_engine` and `scheduling`. A document carrying only `units:` therefore
// scored nothing on either side and was reported undecidable — the commonest
// authoring shape, and the one a `crewlet validate -json` fix loop is most
// often pointed at.
//
// An UNDECIDABLE document is an error naming -tier, never a guess. Guessing
// wrong reports every field of the file as invalid, and an operator reading
// that has no way to tell it from a genuinely broken document.
func detectTier(raw []byte) (Tier, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		// NOT AN ERROR HERE: whichever validator runs will report the
		// parse failure with its own line numbers. Company is the
		// likelier document by far, so it is the one that gets to speak.
		//
		//nolint:nilerr // Deliberate: see the paragraph above.
		return TierCompany, nil
	}
	company := 0
	bootstrap := 0
	companyKeys, bootstrapKeys := config.CompanyKeys(), config.BootstrapKeys()
	for key := range doc {
		switch {
		case slices.Contains(companyKeys, key):
			company++
		case slices.Contains(bootstrapKeys, key):
			bootstrap++
		}
	}
	switch {
	case company > bootstrap:
		return TierCompany, nil
	case bootstrap > company:
		return TierBootstrap, nil
	default:
		return "", fmt.Errorf(
			"cannot tell which tier this document is: it carries %d key(s) "+
				"only a company has and %d only a bootstrap has. Name it with "+
				"-tier company or -tier bootstrap", company, bootstrap)
	}
}

// validation is what one `crewlet validate` run found.
//
// THE SHAPE IS THE JSON, so the two output modes cannot drift: the text
// renderer reads this struct too, rather than being a second pass over the
// same data that eventually disagrees with it. Problems and warnings are the
// config package's own [config.Problem] and [config.Warning], the shape the
// configuration API reports too, so a loop written against one reads the
// other.
type validation struct {
	Valid    bool             `json:"valid"`
	Tier     Tier             `json:"tier"`
	File     string           `json:"file,omitempty"`
	Problems []config.Problem `json:"problems"`

	// Warnings are configurations that are VALID and worth knowing about
	// before applying: a declined fsync's window, a trim that will never
	// advance, a reference that resolves to nothing, a unit keyed on a name
	// somebody will rename.
	//
	// THEIR OWN LIST rather than problems with a flag: the exit code turns
	// on Problems alone, so a CI step gates on refusals and still prints
	// what its operator should read. A warning that could fail a build is
	// one somebody suppresses.
	Warnings []config.Warning `json:"warnings"`

	Summary map[string]any `json:"summary,omitempty"`
}

// newValidation is a run with nothing found yet. Both lists are empty rather
// than null, so a consumer iterates them without a nil check.
func newValidation(tier Tier, file string) validation {
	return validation{Tier: tier, File: file, Problems: []config.Problem{}, Warnings: []config.Warning{}}
}

// validateConfigs is `crewlet validate`.
//
// # Two shapes, because it has two callers
//
// An OPERATOR names one file and reads a sentence. An AUTHORING LOOP — a
// model editing a config until it is valid — names one file and reads
// -json, where every problem carries the exact path to the field. Those are
// the same command because they are the same question, and a loop that had
// to parse prose would converge on whatever the prose happened to say.
//
// The two-flag form (-config plus -company) checks BOTH tiers at once, which
// is what a CI step wants.
func validateConfigs(args []string, stdout, stderr io.Writer) error {
	file, args := splitSubject(args)

	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := addConfigFlags(fs)
	tier := fs.String("tier", string(TierAuto),
		"which document a positional file is: auto, company or bootstrap")
	asJSON := fs.Bool("json", false,
		"emit {valid, tier, file, problems, warnings, summary} instead of prose")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// LEFTOVERS ARE REFUSED, not ignored. Go's flag package stops at the
	// first non-flag token, so `crewlet validate company.yaml -json` used
	// to parse ZERO flags, discard both tokens and validate ./crewlet.yaml
	// and ./company.yaml instead — printing a success line about files it
	// never opened. A fix loop reading that converges on nothing.
	file, given := onePositional(fs, file)
	if given > 1 {
		fmt.Fprintln(stderr,
			"usage: crewlet validate [<file.yaml>] [-tier auto|company|bootstrap] [-json]\n"+
				"   or: crewlet validate [-config <tier-a.yaml>] [-company <tier-b.yaml>] [-json]")
		return errors.New("name at most one document")
	}
	if !Tier(*tier).Valid() {
		return fmt.Errorf("-tier %q is not one of %s", *tier, tierNames())
	}
	if file != "" {
		return validateOne(file, Tier(*tier), *asJSON, stdout)
	}
	return validateBoth(cfg, *asJSON, stdout)
}

func tierNames() string {
	out := make([]string, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, string(t))
	}
	return strings.Join(out, ", ")
}

// validateOne checks the single document an operator named.
func validateOne(file string, tier Tier, asJSON bool, stdout io.Writer) error {
	res := newValidation(tier, file)
	raw, err := os.ReadFile(file)
	if err != nil {
		res.Problems = config.Problems(err)
		return report(stdout, res, asJSON)
	}
	if tier == TierAuto {
		detected, detErr := detectTier(raw)
		if detErr != nil {
			res.Problems = config.Problems(detErr)
			return report(stdout, res, asJSON)
		}
		res.Tier = detected
	}

	if res.Tier == TierBootstrap {
		// TIER A RESOLVES FROM THE ENVIRONMENT ALONE, which is not a
		// default but a rule: it carries the store's address and the keys
		// that open it, so a resolver reaching the secret store would have
		// Tier A reading from the store it is describing.
		boot, bootErr := config.ParseBootstrap(raw, config.EnvOnly())
		if bootErr != nil {
			res.Problems = config.Problems(bootErr)
			return report(stdout, res, asJSON)
		}
		res.Valid = true
		// TIER A HAS WARNINGS OF ITS OWN: a declined fsync, a trim that
		// will never advance on its own, a peer entry no rule counts.
		// Every one of them describes a correct deployment for somebody,
		// so none is a refusal, and here is where the consequence can
		// still change the decision: before the file is deployed. They are
		// not said only here — `crewlet run` logs the same list on every
		// start ([logBootstrapAdvisories]), for every deployment nobody
		// ran this command for.
		//
		// APPENDED TO THE EMPTY LIST rather than assigned over it:
		// [config.Bootstrap.Warnings] answers nil when a document has
		// none, and assigning that would put `null` where the payload
		// promises an array.
		res.Warnings = append(res.Warnings, engine.BootstrapWarnings(boot)...)
		res.Summary = map[string]any{
			"stream": boot.Stream.Type, "coordination": boot.Coordination.Type,
			"store": boot.Store.Path, "roles": boot.Node.Roles,
		}
		return report(stdout, res, asJSON)
	}

	company, warnings, companyErr := checkCompany(raw)
	res.Warnings = warnings
	if companyErr != nil {
		res.Problems = config.Problems(companyErr)
		return report(stdout, res, asJSON)
	}
	res.Valid = true
	res.Summary = map[string]any{
		"company": company.Name, "seats": len(company.epoch.Seats()),
		"llm_providers": len(company.epoch.Models.Keys()),
	}
	return report(stdout, res, asJSON)
}

// checkedCompany is a company document that passed every check, with the
// epoch built from it.
type checkedCompany struct {
	*config.Company
	epoch *engine.Company
}

// checkCompany holds a Tier B document to every rule a submitted document is
// held to, and then builds its epoch.
//
// Building the epoch is the rest of the check, and it reaches nothing: no
// broker, no store, no provider is dialled. It is what catches the problems a
// schema cannot, such as a seat whose llm names no configured provider.
//
// A document that did NOT hold up reports its references alone: they are
// reported whenever it PARSED, valid or not, because a misspelled lead is
// worth seeing in the same pass as the problems and fixing the problems will
// not fix it. The admission half of [config.Company.Warnings] is deliberately
// not on that path, because this command refuses an admission rule as a
// PROBLEM, and the same sentence printed twice reads as two findings.
//
// Its advisories wait for the same reason the admission half does, from the
// other side: an advisory describes what a document that RUNS will do, and a
// paragraph about a unit's id printed beside a refusal is noise at the moment
// somebody is reading carefully.
//
// A document that held up carries the whole set instead: its references, plus
// every advisory about what it will do that its author should know before
// applying it.
func checkCompany(raw []byte) (*checkedCompany, []config.Warning, error) {
	company, err := config.ParseCompanyDocument(raw)
	if err != nil {
		return nil, []config.Warning{}, err
	}
	warnings := company.ReferenceWarnings()
	if err = company.Validate(); err != nil {
		return nil, warnings, err
	}
	epoch, err := engine.NewCompany(company)
	if err != nil {
		return nil, warnings, err
	}
	return &checkedCompany{Company: company, epoch: epoch}, company.Warnings(), nil
}

// validateBoth is the two-flag form: check a Tier A and a Tier B together.
//
// BOTH, not just the first to fail: an operator fixing a broker URL only to be
// told about their org chart on the next boot has been made to pay twice for
// one edit. It is the same rule each tier's own validator follows internally.
func validateBoth(cfg configFlags, asJSON bool, stdout io.Writer) error {
	res := newValidation(TierAuto, "")
	// Environment-only resolution for Tier A: see validateOne.
	boot, bootErr := config.LoadBootstrap(*cfg.bootstrap, config.EnvOnly())
	bootErr = nameTheNeighbour(*cfg.bootstrap, bootErr)

	var company *checkedCompany
	raw, companyErr := os.ReadFile(*cfg.company)
	if companyErr == nil {
		company, res.Warnings, companyErr = checkCompany(raw)
	}
	if err := errors.Join(bootErr, companyErr); err != nil {
		res.Problems = config.Problems(err)
		return report(stdout, res, asJSON)
	}
	// NO RULE NEEDS BOTH DOCUMENTS any more. The one there was — a state
	// log on an in-memory stream — is Tier A's alone now that every node
	// runs its logs from boot, company or none ([config.Stream.Durable]),
	// so the Tier A pass above already refused it.
	res.Valid = true
	// BOTH TIERS' WARNINGS, for the reason both tiers' problems are
	// reported: an operator who fixes one file and is told about the other
	// on the next run has been made to pay twice for one edit. The
	// company's are already here, put there by checkCompany so that a
	// refused document still reports them, so Tier A's go in FRONT of them
	// rather than after, which is the order the two files are named in.
	//
	// Inserted rather than appended onto Tier A's own list, which is nil
	// for a document with no warnings at all: the payload promises both
	// lists are arrays, and `append(nil)` of nothing stays nil.
	res.Warnings = slices.Insert(res.Warnings, 0, engine.BootstrapWarnings(boot)...)
	// AND WHAT ONLY THE TWO TOGETHER SAY: a Tier A bind no container box on
	// this host can reach, for a company that runs code work in one. Judged
	// for the host validating, which is the host a single-node install runs.
	res.Warnings = append(res.Warnings,
		engine.ContainerReachWarnings(boot, company.Company, runtime.GOOS)...)
	res.Summary = map[string]any{
		"company": company.Name, "seats": len(company.epoch.Seats()),
		"llm_providers": len(company.epoch.Models.Keys()),
		"stream":        boot.Stream.Type, "coordination": boot.Coordination.Type,
	}
	return report(stdout, res, asJSON)
}

// report renders a validation and returns the run's exit disposition.
//
// A FAILED VALIDATION IS A NON-ZERO EXIT IN BOTH MODES. The -json caller is a
// CI step as often as it is a model, and `crewlet validate x.yaml -json ||
// exit 1` has to be able to fail — a mode that printed {"valid": false} and
// exited 0 would pass every gate built on it.
//
// A WARNING NEVER FAILS IT: the engine runs a document whose reference
// resolves to nothing, and a gate that refused one would refuse a company
// being assembled in pieces. In prose, warnings are printed before the
// outcome, so a reader sees them whichever way it went.
func report(stdout io.Writer, res validation, asJSON bool) error {
	if asJSON {
		raw, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(raw))
		if res.Valid {
			return nil
		}
		// SILENT, because the payload already carries every problem and a
		// second copy on stderr is what makes a JSON consumer's log
		// unreadable.
		return errSilent
	}
	// ON STDOUT WITH THE ANSWER rather than on stderr, and before the
	// outcome: a warning is part of what the run found rather than a
	// diagnostic about the run, and a reader sees it whichever way the
	// validation went.
	for _, w := range res.Warnings {
		fmt.Fprintln(stdout, warningLine(w))
	}
	if !res.Valid {
		return errors.New(strings.Join(problemLines(res.Problems), "\n  "))
	}
	fmt.Fprintln(stdout, summaryLine(res))
	return nil
}

// problemLines is the problems as the lines an operator reads: one per
// message, led by every path it was placed at. A duplicate is one message and
// a problem beside each entity sharing the name, and printing that message
// once per entity would repeat a paragraph to say where the second one is.
func problemLines(problems []config.Problem) []string {
	var order []string
	paths := map[string][]string{}
	for _, p := range problems {
		if _, seen := paths[p.Message]; !seen {
			order = append(order, p.Message)
		}
		if p.Path != "" {
			paths[p.Message] = append(paths[p.Message], p.Path)
		} else if paths[p.Message] == nil {
			paths[p.Message] = []string{}
		}
	}
	lines := make([]string, len(order))
	for i, message := range order {
		lines[i] = prose(strings.Join(paths[message], ", "), message)
	}
	return lines
}

// prose is a message as a line an operator reads, led by where it is when the
// message does not already open with that. A rule the org model reports about
// several seats names them in words, and the paths are what the operator can
// search their file for.
func prose(where, message string) string {
	if where == "" || strings.HasPrefix(message, where+": ") {
		return message
	}
	return where + ": " + message
}

// warningLine is one warning as an operator reads it, led by WHICH KIND of
// warning it is.
//
// The list holds sentences that ask for different things: a dangling
// reference is broken and somebody has to correct it, an advisory is a valid
// choice reporting what it will cost, an admission rule is a stored revision
// a new write would be refused for. Printed under one undifferentiated
// `warning:` they all read as one severity, and a healthy document carries
// more advisories than references, so the broken one is the line that gets
// skimmed past.
func warningLine(w config.Warning) string {
	if kind := warningKind(w.Kind); kind != "" {
		return "warning (" + kind + "): " + prose(w.Path, w.Message)
	}
	return "warning: " + prose(w.Path, w.Message)
}

// warningKind is a [config.Warning]'s kind as the words an operator reads. A
// kind this build does not classify renders as its wire value rather than as
// a blank, because the label is the only thing on the line that says how much
// the warning matters and an empty pair of brackets says nothing at all.
func warningKind(kind string) string {
	switch kind {
	case config.WarningDanglingReference:
		return "dangling reference"
	case config.WarningAdmission:
		return "admission"
	case config.WarningAdvisory:
		return "advisory"
	default:
		return kind
	}
}

// summaryLine is the one prose line a successful validation prints.
func summaryLine(res validation) string {
	if name, ok := res.Summary["company"].(string); ok {
		line := fmt.Sprintf("%s: %d agent seats, %d LLM providers",
			name, res.Summary["seats"], res.Summary["llm_providers"])
		if stream, both := res.Summary["stream"]; both {
			line += fmt.Sprintf(", stream %q, coordination %q",
				stream, res.Summary["coordination"])
		}
		return line
	}
	// THE PROSE LINE CARRIES EVERY FIELD THE JSON DOES. Most people run
	// this without `-json`, so a summary key only the machine-readable
	// shape prints is one almost nobody reads.
	return fmt.Sprintf("%s: stream %q, coordination %q, store %q, roles %v",
		res.File, res.Summary["stream"], res.Summary["coordination"],
		res.Summary["store"], res.Summary["roles"])
}

// errSilent asks the caller to exit non-zero without printing anything more.
var errSilent = errors.New("")

// runEngine is `crewlet run`. bind opens every listener the node serves on,
// before the boot ([bindListeners]): [bindTCP] for the binary, and a socket
// the caller already holds for a test that starts a whole node (see
// [bindFunc]).
func runEngine(args []string, stderr io.Writer, bind bindFunc) (err error) {
	file, args := splitSubject(args)

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := addConfigFlags(fs)
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "console",
		"log format: console (columns and colour for a person), text or json")
	debug := fs.Bool("debug", false, "shorthand for -log-level debug")
	logFile := fs.String("log-file", "",
		"also write the log to this file, overriding logging.file.path; "+
			"an empty value writes no file")
	roles := fs.String("roles", "",
		"what this node runs, overriding node.roles: ingress, seats, workers")
	mode := fs.String("mode", string(statelog.ModeNormal),
		"normal (default), maintenance or seal. The two maintenance modes start "+
			"this node's broker and NO publisher, for a stream capacity change; "+
			"every node of the fleet has to be in the same one. See "+
			"`crewlet retention set-capacity`")
	apiHost := fs.String("api-host", "", "bind address, overriding api.host")
	apiPort := fs.Int("api-port", -1,
		"bind port, overriding api.port; 0 serves no HTTP at all")
	// ON run's OWN SET, not addConfigFlags: `validate` checks documents and
	// has nothing to override, so offering it there would be a flag that
	// parses and means nothing — the thing the config subcommands were just
	// cured of.
	importCompany := fs.String("import-company", "",
		"Tier B config to make the active revision NOW, over whatever the "+
			"fleet is running; -company only bootstraps a company that does "+
			"not exist yet")
	// A FLAG AND NEVER A CONFIG FIELD, for the reason -mode is one and for
	// a sharper one: `api.auth.disabled` was a field, and a field reaches
	// production by being copied into an image. See auth.NewDevPrincipal
	// for the two refusals behind it.
	devPrincipal := fs.String("dev-principal", "",
		"Development only: treat every request without a token as this "+
			"login, with every app (and admin when api.auth.admin is "+
			"serve). Only works on a development build bound to loopback")
	// ASSIGNED, NOT DECLARED: this function's error is NAMED, so that a
	// deferred recorder can put a boot failure in the log file before the
	// file closes (see below), and a `:=` here would shadow it — harmless
	// today because nothing is deferred yet at this point, and exactly the
	// kind of thing that stops being harmless when a line moves.
	if err = fs.Parse(args); err != nil {
		return err
	}
	// THE POSITIONAL IS THE TIER A PATH, and a leftover is REFUSED rather
	// than ignored. Go's flag package stops at the first non-flag token,
	// so `crewlet run /etc/crewlet.yaml -debug` used to parse no flags at
	// all, discard the path, and boot from ./crewlet.yaml — or from
	// nothing — without ever mentioning the file the operator named.
	file, given := onePositional(fs, file)
	if given > 1 {
		// EVERY FLAG run REGISTERS BAR -config, whose value is the
		// leading positional here and which naming a second time is the
		// error being reported. This is the only synopsis an operator
		// sees at the moment they got the arguments wrong, and a flag
		// missing from it is one they do not reach for. It has been short
		// three times: first the three logging flags, which are what
		// somebody diagnosing exactly this reaches for, then
		// -import-company, which is the flag an operator confused about
		// which Tier B document wins most needs to be told exists, and
		// then -log-file.
		fmt.Fprintln(stderr, "usage: crewlet run [<config.yaml>] "+
			"[-company <company.yaml> | -import-company <company.yaml>] "+
			"[-log-level …] [-log-format …] [-log-file …] [-debug] "+
			"[-roles …] [-mode …] [-api-host …] [-api-port …] "+
			"[-dev-principal …]")
		return errors.New("name at most one config document")
	}
	if file != "" {
		if isFlagSet(fs, "config") {
			return errors.New(
				"the config document is named twice, as a positional argument " +
					"and as -config; they would have to agree and nothing " +
					"checks that they do")
		}
		*cfg.bootstrap = file
	}

	// THE FLAGS ALONE, FIRST, because the file has not been read yet and
	// reading it is exactly what `-debug` is most often turned on to watch:
	// an unresolved ${VAR}, a refused enum, a path that is not there. The
	// second call below re-applies this on top of the file's own settings.
	//
	// The level and format this invocation asked for, on the process's own
	// sink — see the note in [run] for why the writer is not this
	// command's to install.
	logging.SetVerbosity(flagLogLevel(*logLevel, *debug), logging.ParseFormat(*logFormat))
	log := logging.Get("cli")
	warnUnrecognisedLogNames(log, "flag", "-log-level", *logLevel, "-log-format", *logFormat)

	// A BACKGROUND CONTEXT, because the run's own — cancelled by a signal —
	// is made below, once the log file this load names is open. What the
	// load reads is this node's own files, bounded by the store's busy
	// timeout.
	boot, seed, loadErr := cfg.loadForRun(context.Background(), fs, *importCompany)
	if boot == nil {
		// Tier A itself is what failed, so there is no document naming a
		// log file and nothing to open. stderr is the whole record.
		return loadErr
	}
	// THE LOGGING FLAGS FOLD INTO THE DOCUMENT, as -roles and the API flags
	// do below ([overrideNode]), so everything that reads Tier A's
	// `logging:` block from here on reads the one this run applies: the
	// verbosity, the log file, and the advisory about a verbose broker
	// nothing records, which a `-debug` run would otherwise be warned about
	// by the line that tells it to pass `-debug`.
	if err = overrideLogging(boot, fs, *logLevel, *logFormat, *debug, *logFile); err != nil {
		return err
	}
	// AND NOW THE FILE, which is what makes Tier A's `logging:` block mean
	// anything: a declared setting nothing reads is one an operator writes
	// and that does nothing at all, so this line is what makes
	// `logging.level` take effect. Lines emitted BEFORE this point came out
	// under the flags alone, which is the best a process can do about a
	// file it has not opened yet.
	logging.SetVerbosity(boot.LogSettings())
	// AND THE LOG FILE, the moment the document that names it has been
	// read. Everything that can still fail below — a keyring that will not
	// build, a store that will not open, a company that will not
	// activate — is exactly what an operator goes to that file to read
	// afterwards, so it is opened before any of them rather than once the
	// engine is up.
	//
	// ITS DETACH IS DEFERRED FIRST, which under LIFO means it runs LAST.
	// The trace flush below and the engine drain further down both log,
	// and a file closed ahead of them would lose the shutdown it was
	// opened to record.
	logFileSet, _ := boot.Logging.File.LogFileSettings()
	detachLogFile, err := attachLogFile(logFileSet, boot.Logging.StderrEnabled(), os.Stderr)
	if err != nil {
		return err
	}
	defer detachLogFile()
	// AND THE FAILURE THAT ENDS THIS FUNCTION IS RECORDED BEFORE THE FILE
	// CLOSES. Registered after the detach, so LIFO runs it first.
	//
	// runEngine returns its errors for main to print, and main prints them
	// after this function has returned — by which time the log file would
	// already be shut. Every boot failure below (a keyring that will not
	// build, a store that will not open, a company that will not activate)
	// would reach a terminal and never the durable record, which is the
	// one place an operator looks for a boot that failed while nobody was
	// watching.
	defer func() {
		if err != nil {
			log.Error("run_failed", "error", err)
		}
	}()

	// THE SEED'S OWN FAILURE, now that it can be recorded.
	if loadErr != nil {
		return loadErr
	}
	// THE FLAGS OVERRIDE THE FILE, and are applied AFTER it loads so a
	// validation failure names the file's own value rather than one the
	// command line put there. Each is applied only when it was actually
	// given: an unset -api-port is not "port 0", which would serve no
	// HTTP at all and make every integration go deaf.
	if err = overrideNode(boot, fs, *roles, *apiHost, *apiPort); err != nil {
		return err
	}
	// AND EVERY OTHER TIER A RULE, over the document the flags left, once all
	// of them are in — see [validateFolded] for why the engine's own check is
	// too late for this command.
	if err = validateFolded(boot, fs); err != nil {
		return err
	}
	// AFTER THE OVERRIDES, because -api-host is one of them and the
	// loopback refusal reads `api.host`: built before, it would judge the
	// file's bind on a run that moved it, in either direction — refusing a
	// legitimate `-api-host 127.0.0.1` and, far worse, permitting the flag
	// on a run whose -api-host opened the node to the network.
	//
	// AND BEFORE ANYTHING STARTS. Its refusals are about this build and
	// this bind, so a run that cannot have what it asked for fails here
	// rather than booting an engine and then serving without it.
	dev, err := auth.NewDevPrincipal(*devPrincipal, boot)
	if err != nil {
		return err
	}
	if dev != nil {
		// LOUD, EVERY BOOT. The whole surface is open to this machine,
		// and a line nobody sees is how a developer leaves it on for a
		// week and forgets what they are looking at.
		log.Warn("api_dev_principal_enabled",
			"login", *devPrincipal, "bind", boot.API.Host,
			"hint", "every request without a token acts as this login. "+
				"This only works on a development build bound to loopback")
	}
	// THE MODE IS A FLAG AND NEVER A CONFIG FIELD, deliberately. Every
	// node of the fleet restarts into it and out of it again, three times
	// over one capacity change — so it is a property of THIS RUN rather
	// than of the deployment, and a file that carried it would leave a
	// node that came back after an unrelated restart still refusing to
	// publish, with the reason sitting in a file nobody re-read.
	nodeMode := statelog.MaintenanceMode(strings.TrimSpace(*mode))
	if !nodeMode.Valid() {
		return fmt.Errorf("-mode %q is not one of %v: the two maintenance modes "+
			"start no publisher, so a typo here would be a node that boots, "+
			"reports healthy and runs nothing", *mode, statelog.MaintenanceModes)
	}
	// WHAT IS VALID ABOUT TIER A AND WORTH KNOWING, said on every start (see
	// [logBootstrapAdvisories] for why). At this point and no other: after
	// every refusal Tier A's rules can draw from the file and the flags
	// ([validateFolded]), and this command's own about the bind and the
	// mode, because a warning printed beside a refusal is noise at the
	// moment somebody is reading carefully; after the log file is attached,
	// so the lines reach the durable record; and after every flag has been
	// folded into the document, so they describe the node this run starts
	// rather than the file it was started from. What can still fail after
	// them is not Tier A's to refuse: the environment's node id, a port
	// somebody else holds, a company file the seed writes into the store and
	// holds to the admission rules then, a store or a broker that will not
	// open.
	logBootstrapAdvisories(log, boot)

	// The engine owns the process signals exclusively — one handler per
	// signal — because a graceful drain is the difference between a
	// restart that resumes cleanly and one that redelivers half-finished
	// turns. Nothing else in the process may install a handler.
	ctx, stop := interruptContext()
	defer stop()

	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return err
	}
	// THE LISTENERS, BOUND NOW: before the first step of the boot that can
	// take time, which is everything from [decideSeed] on. What they answer
	// until the node hands them its own surface is internal/api's boot
	// surface ([api.Boot]): /health 503 while the node recovers what it holds
	// and 200 once it has — so a startup probe bounds the recovery exactly as
	// it did, and nothing the company configures, which all starts after it,
	// can hold the probe — /ready 503 throughout, and every route the node
	// will serve a 503 `starting` with a Retry-After rather than a refused
	// connection.
	//
	// AND A PORT ALREADY TAKEN FAILS THE BOOT HERE, at once, rather than
	// after a recovery that can take minutes and is then thrown away.
	//
	listeners, err := bindListeners(ctx, boot, nodeID, bind, log)
	if err != nil {
		return err
	}
	// CLOSED ON EVERY FAILURE BEFORE THE HANDOVER, which disarms this: from
	// [serveAPI]'s return the surface owns both sockets and [shutdown] closes
	// them. Deferred, so it runs after the inline engine stops below — the
	// probes answer through the teardown, as they do through a drain.
	handedOver := false
	defer func() {
		if !handedOver {
			listeners.close(context.WithoutCancel(ctx), log)
		}
	}()
	// ONE RECORDER FOR THE PROCESS, built here and handed to both halves.
	// The engine writes into it from its apply loops and its write paths;
	// the MeterProvider tracing installs is a READER of it. Two recorders
	// would make a collector's dashboard and `crewlet retention status`
	// disagree about the same event, which is the drift a single generated
	// catalogue exists to prevent — and building it in neither place is
	// how every instrument stayed declared and unobserved.
	recorder, err := metrics.New()
	if err != nil {
		return err
	}
	// TRACING BEFORE THE ENGINE, because engine.New starts subscriptions
	// and registers the publish listener, and every one of those can open a
	// span. A provider installed after them would leave the boot — the one
	// stretch an operator most often wants a trace of — reporting nothing.
	//
	// The flush is DEFERRED rather than written at each return. Nearly
	// every way out of this function from here is a boot failure, and a
	// deferred call registered here runs after all of them AND after the
	// drain below, which is exactly the ordering the batch processor needs:
	// the spans the drain itself emits are in the buffer when it flushes.
	// Writing it at each return is how most of them quietly stop doing it.
	flushTraces, err := tracing.Configure(ctx, tracing.Options{
		NodeID:   nodeID,
		Version:  version.String(),
		Recorder: recorder,
	})
	if err != nil {
		return err
	}
	defer func() {
		if flushErr := flushTraces(ctx); flushErr != nil {
			// Losing telemetry is not worth failing a shutdown that has
			// otherwise succeeded — the drain has already released the
			// seats and closed the backends by the time this runs.
			log.WarnContext(ctx, "trace_flush_failed", "error", flushErr)
		}
	}()

	// WHAT THE TIER B FILE IS FOR, AND WHAT THE ENGINE STARTS ON: one
	// decision, taken here and handed to both the engine's boot and the
	// import below, so the two cannot disagree about one file. See
	// [decideSeed]. After the log file and Tier A's last refusals, so an
	// ignored seed's warning is in the durable record and never beside those
	// refusals — nor beside the decision's own, being said only once it
	// holds; and after the listeners, because it opens the store.
	//
	// AND THE DECISION HOLDS THE STORE until the engine has opened it, so
	// that no offline command can replace what the engine is about to boot
	// on in between: the decision and the open are one step, which gives the
	// claim back on every way out ([bootEngine]).
	e, plan, err := bootEngine(ctx, boot, seed, storeAt(*cfg.bootstrap),
		func(ctx context.Context, opts engine.Options) (*engine.Engine, error) {
			opts.Metrics, opts.Mode = recorder, nodeMode
			// AND THE MOMENT /health TURNS 200: the engine says when it
			// has recovered what this node holds — see
			// [engine.Options.Recovered].
			opts.Recovered = listeners.recovered
			// AND WHERE A DIRECT BOX DIALS THE BRIDGE AND THE TELEMETRY
			// RECEIVER: the listener this process bound for them, on
			// loopback — see [engine.Options.BoxHostURL].
			opts.BoxHostURL = listeners.boxHostURL(boot, nodeID)
			return engine.New(ctx, opts)
		}, log)
	if err != nil {
		return err
	}

	// ONE keyring for the process, and it is the ENGINE's: the reconciler
	// opens and authenticates revisions under it, so the config surface
	// seals them with the same one and the seed seals the file with it.
	// Built a second time here from the same Tier A, it matched only
	// because two constructors happened to agree.
	cipher := e.Cipher()

	// THE STORE IS AUTHORITATIVE AT RUNTIME; the file is a seed. Both
	// halves matter: without the seed a first run has nothing to activate
	// and the node serves a company no peer can see, and without the store
	// a PUT /config on one node would be invisible to every other.
	//
	// Converged BEFORE Start, so seats are claimed under the epoch this
	// node will actually serve — on a node the plan booted unconfigured to
	// import its file, the first reconcile is what builds that company.
	reconciler, err := startReconciler(ctx, e, boot, plan, cipher, log)
	if err != nil {
		e.Stop(context.WithoutCancel(ctx))
		return err
	}

	// THE POSTURE REACHES PEERS from here, because the reconciler owns it
	// and is only built once the engine exists. Set before Start, so the
	// first heartbeat already carries it.
	e.SetPosture(func(ctx context.Context) string {
		return string(reconciler.Posture(ctx))
	})

	// AND THE GATE THAT ACTS ON IT. Reporting the posture is not applying
	// it: without this the inbound edge, the scheduler and every seat's
	// own inbox admitted work whatever this node had concluded, so a
	// `shed` reached the dashboard and the readiness probe and changed
	// nothing else. Set beside the reporter and before Start, so the
	// refusal is live from the first delivery this node could take.
	e.SetAdmits(reconciler.Admits)

	// THE CONFIG SURFACE IS BUILT HERE, NOT INSIDE serveAPI, and the
	// difference is a node with `api.port: 0`.
	//
	// It used to be constructed after that function's early return, so a
	// worker-only node — a documented posture, validated as "0 (no HTTP
	// surface)" — never installed a config writer at all. The reconcile
	// loop is armed regardless: it is a FLEET SINGLETON, so on a split
	// deployment it lands on exactly that node as readily as on any other,
	// and every path needing the writer then failed for the life of the
	// process. A disconnect answered ErrDisconnectUnavailable whose own
	// comment calls the missing writer "the window between the loop arming
	// and the API wiring, which resolves on its own within a second"; a
	// GitHub seat's discovered installation could never be recorded; and
	// the Atlassian pass emitted its "set cloud_id by hand" note on every
	// tick, for ever.
	//
	// Nothing about writing the company document needs an HTTP listener —
	// the surface takes a store, a cipher, the plane and the queue, all of
	// which exist here — so it is built on every node that HOLDS THE
	// ESTATE, with a port or without one, and handed to serveAPI, which
	// shares the ONE instance with the REST routes and the socket queries.
	// A node without `data` builds none, because nothing on it writes the
	// document: the reconcile loop is a `workers` duty and the API an
	// `ingress` surface, and config refuses either without `data`.
	configSurface, err := companySurface(e, boot, cipher)
	if err != nil {
		e.Stop(context.WithoutCancel(ctx))
		return err
	}
	// The surface a DISCONNECT removes a block through. Until it is set
	// the loop refuses a disconnect rather than running the teardown at
	// the third-party app and leaving the block behind.
	if configSurface != nil {
		e.UseConfigWriter(engineConfigWriter{surface: configSurface})
		// THE COMPANY MOVES ONTO PER-SEAT ACCESS before this node starts its
		// seats, on every node that holds the estate: the first to write
		// wins the revision's compare-and-set and the rest find nothing left
		// to do. What makes it safe is not this order but the gate the
		// revision carries: the access pass for an integration acts on no
		// seat until the document marks the integration migrated, so a pass
		// that runs first, or an integration whose vendor did not answer,
		// changes nothing at any vendor.
		migrateSeatAccessOnce(ctx, e, configSurface,
			fleetsecrets.New(e.Backends().Fleet, cipher), log)
	}

	// ONE PROCESS IS BOTH ENGINE AND API, sharing one broker and one store,
	// and it is the only shape an API is served in. The API half is what
	// makes the node reachable at all (every inbound webhook arrives
	// through it), so an engine that ran without it would hold seats and
	// hear nothing.
	//
	// SERVED BEFORE THE ENGINE STARTS — the listeners were bound before the
	// boot, and this is where they are handed the node's own surface — and
	// the order is a fix rather than a preference. Starting the engine first
	// means claiming seats first, and a seat is not claimed until its
	// per-role MCP children are up (one subprocess per server per seat, each
	// a spawn and a handshake and a tools/list). On the Nimbus example that
	// is 21 children, and the whole inbound edge (dashboard, REST, every
	// third-party app's webhook) went unserved for as long as they took —
	// dark, before the listeners were bound early, and `503 starting` now.
	// Measured at 37 seconds with four seats and every vendor failing FAST;
	// a company whose vendors actually answer takes minutes, and it scales
	// with seats times servers.
	//
	// Nothing here needs a started engine: the node exists, /health's seat
	// list reports honestly that it holds no seats yet (/ready carries none),
	// and a webhook that arrives in the window is retained rather than
	// dropped because the mailboxes are created before any claiming (see
	// Node.Start).
	// THE CONFIG FILES THIS NODE WAS STARTED FROM, which a deep-work run
	// confined on this host is kept from beside the engine's own paths.
	host := engine.DeepHost{Config: []string{*cfg.bootstrap, *cfg.company, *importCompany}}
	surface, err := serveAPI(ctx, boot, e, reconciler, cipher, configSurface, dev, listeners,
		host, log)
	if err != nil {
		e.Stop(context.WithoutCancel(ctx))
		return err
	}
	handedOver = true

	if err = e.Start(ctx); err != nil {
		// Everything the engine opened comes down with it. Returning
		// without this leaves a broker, a store and a set of held seat
		// leases behind, and the leases are the expensive half, because
		// a peer cannot take those seats until they lapse at the TTL.
		//
		// In the ONE order every stop takes rather than a second one for
		// a boot that failed: the listener is already accepting, and it
		// is the drain, not a closed port, that refuses new work on it.
		// See [shutdown].
		shutdown(context.WithoutCancel(ctx), e, surface, log)
		return err
	}

	// The poll loop, started only once the node is serving: a reconcile
	// that landed mid-boot would apply an epoch to a node that has not
	// claimed anything yet, which is work with nowhere to go.
	go reconciler.Run(ctx)

	// AND IT KEEPS LOOKING: an integration left gated because its vendor did
	// not answer, or one a person connected since, is migrated by a later
	// attempt.
	if configSurface != nil {
		go keepMigratingSeatAccess(ctx, e, configSurface,
			fleetsecrets.New(e.Backends().Fleet, cipher), log)
	}

	// A NODE STOPS FOR ONE OF TWO REASONS, and both take the one shutdown
	// below: a signal, which is somebody else's decision, or the engine
	// reporting it has lost something it cannot run without — a broker
	// connection NATS closed for good (see [engine.Engine.Fatal]). That one
	// used to be noticed by nothing: the process went on with no connection
	// to publish, consume or renew a lease over, healthy to a liveness probe,
	// until a person restarted it. Now it drains, tears down in the ordinary
	// order and RETURNS the cause, which main prints and exits non-zero on,
	// so whatever supervises the process restarts it — the seat watchdog's
	// rule for a wedged event loop, reached by the graceful path because
	// nothing here is wedged.
	var fatal error
	select {
	case <-ctx.Done():
		// The signals are already handed back by now (see
		// [interruptContext]), so the second interrupt the drain line below
		// promises does what it says.
	case <-e.Fatal():
		fatal = e.FatalCause()
		// NO SIGNAL ARRIVED, so interruptContext still holds both: handed
		// back here, an operator's FIRST interrupt during this drain ends it.
		// Left installed, that press would only cancel a context the drain
		// is already detached from, and be swallowed.
		stop()
		log.ErrorContext(ctx, "engine_fatal", "error", fatal,
			"detail", "this node cannot go on running: it stops the way a signal "+
				"stops it and exits non-zero, for whatever supervises it to start "+
				"it again")
	}

	// Said BEFORE the drain, which can take minutes: this line is what an
	// operator at a terminal reads to learn what the node is waiting for and
	// how to stop waiting.
	//
	// WHAT THE PROBES SAY IS CONDITIONAL on there being a listener to say it
	// on. A node with api.port 0 is a real posture (see [serveAPI]'s
	// api_disabled line), and promising such an operator a /health that
	// stays 200 sends them to a refused connection, which is exactly what a
	// node that died mid-drain looks like — the confusion the rest of this
	// change exists to remove.
	//
	// And what the WORK ROUTES say is conditional on there being any: a node
	// without the ingress role serves only its probes (and a bridge whose
	// calls belong to runs already under way), so it has no route that would
	// start new work to refuse.
	probes := "this node binds no HTTP listener, so its drain is visible " +
		"only in this log"
	switch {
	case surface != nil && surface.app != nil:
		probes = "/health stays 200, /ready answers 503 and every route " +
			"that would start new work answers 503"
	case surface != nil:
		probes = "/health stays 200 and /ready answers 503"
	}
	//
	// AND WHAT IT WAITS FOR IS CONDITIONAL on which connection a fatal lost.
	// With the queue's own gone the drain waits for nothing — the turns are
	// cancelled, see the shutdown below — and a line promising they finish
	// first would describe a drain this node is not running.
	//
	// AND WHICH PRESS ENDS IT on what stopped the node: after a signal it is
	// the SECOND interrupt, the first having started this drain, while a
	// fatal started it with none, so the operator's first press is the one
	// that exits.
	press := "a second interrupt"
	if fatal != nil {
		press = "an interrupt"
	}
	draining := []any{"in_flight", e.Backends().Queue.InFlightCount(),
		"detail", "the turns already running finish before this node stops; " +
			"until they have, " + probes,
		"hint", press + " exits at once, leaving those turns to be " +
			"redelivered once their ack window elapses"}
	select {
	case <-e.AcksLost():
		draining = []any{"in_flight", e.Backends().Queue.InFlightCount(),
			"detail", "this node's own broker connection is closed for good, so " +
				"nothing the turns still running conclude can be acknowledged and " +
				"whichever node takes each seat runs it again: they are cancelled " +
				"rather than waited for, and the drain ends at once"}
	default:
	}
	log.InfoContext(ctx, "engine_draining", draining...)

	// Detached from the signal context, which a signal has already cancelled
	// when that is what woke us. The drain waits for in-flight turns, bounded only by the
	// context it is given, so handing it this one would abandon every turn
	// already running and make the shutdown the opposite of graceful.
	//
	// The bound belongs to whatever supervises the process: a container
	// runtime's kill grace, or the operator's next interrupt, which the
	// signal that woke us handed back to the operating system (see
	// [interruptContext]) and a fatal hands back above. Both already have one
	// and can see things this process cannot.
	//
	// THE SAME DRAIN ON A FATAL, with no signal behind it and so no kill
	// grace running, and it still ends — for a reason that depends on which
	// connection went. With the QUEUE'S OWN gone the drain stops waiting at
	// once and cancels the turns still running ([node.Node.Drain]): nothing
	// they conclude can be acknowledged, so a peer runs each of them again
	// and finishing them here would only make every side effect twice. With
	// only the COORDINATION STORE'S gone every ack still lands, so the drain
	// waits for the turns as a signal's does, and they end on their own:
	// every lease and ledger write they make over the closed connection
	// fails at once rather than waiting on a broker. Either way the order
	// after the drain is the one a signal takes, and a second shutdown path
	// that skipped it would be one more order to keep correct.
	shutdown(context.WithoutCancel(ctx), e, surface, log)
	if fatal != nil {
		return fmt.Errorf("the node stopped itself: %w", fatal)
	}
	return nil
}

// interruptContext is the context `crewlet run` lives under: cancelled by the
// first SIGINT or SIGTERM, and HANDING BOTH SIGNALS BACK to the operating
// system the moment it is, so a SECOND one kills the process.
//
// At the first signal rather than once the node is serving, because a
// serving node's drain is not the only unbounded wait. A boot has them too:
// a boot that fails once the engine is up runs the same drain, and an
// embedded broker still recovering a large store cannot be interrupted
// (internal/queue/jetstream's startEmbedded waits the recovery out). While
// signal.NotifyContext is still the installed handler, every further press is
// swallowed, which left an operator with no way to abort from the terminal
// they started the node in short of SIGKILL from somewhere else. The escape
// hatch is what makes those unbounded waits safe to offer: the bound belongs
// to whoever supervises the process, and for an operator at a terminal the
// second press IS their bound.
//
// The returned stop releases the signals without one having arrived; calling
// it again is harmless.
func interruptContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

// shutdown is the one order a node comes down in, whatever stopped it.
//
//  1. DRAIN WITH THE LISTENER UP. /health and /ready are what an orchestrator
//     reads while the turns finish, and they keep answering: 200 for liveness
//     and 503 for readiness. The drain is also what shuts the door, because
//     from its first moment the HTTP surface refuses every route that would
//     start new work (see internal/api's drain gate), so a listener still
//     accepting is no longer a listener still minting turns.
//  2. CLOSE THE LISTENER, once the drain has completed or its context has
//     ended, and not before: a node that closed it first answered neither
//     probe for the whole drain, so its liveness probe failed while it was
//     doing exactly what it should.
//  3. TEAR THE ENGINE DOWN, after the listener, because both probes read the
//     broker and the coordination store the teardown closes.
//
// A nil surface is a node that bound no listener — api.port 0, or a node
// without the ingress role and with no tool bridge to serve — and it drains
// and stops the same way with nothing to close between the two.
func shutdown(ctx context.Context, e *engine.Engine, surface *httpSurface, log *slog.Logger) {
	e.Drain(ctx)
	if surface != nil {
		surface.stop(ctx, log)
	}
	e.Stop(ctx)
}

// httpSurface is the HTTP listeners a node binds: the whole API on a node with
// the ingress role — on api.port, with the routes outside parties call on a
// public listener of their own when api.public is set — or, on a node without
// it, its probes on api.port alone. The app, the projector and the public
// server are nil in the second shape. The sign-in
// surface is nil there too, and on a node that serves no sign-in (see
// [api.NewHumanSurfaces]); it is never mounted on the public listener, whose
// routes are the ones outside parties call.
type httpSurface struct {
	app       *api.App
	server    *apiListener
	public    *apiListener
	projector *observe.Projector

	// artifacts is the artifact origin's listener (api.artifacts), nil on a
	// node that sets no port for it, and stopArtifacts ends its handler's
	// subscription to this node's artifacts movements.
	artifacts     *apiListener
	stopArtifacts func()

	// auth is the sign-in surface, held here for the work it runs after
	// its answers — a stale password's rewrite — which is its own to stop
	// and must stop before the engine its writes go to does.
	auth *authapi.Service
	runs *observe.SandboxReconciler

	// assistant is the Dashboard Assistant's service, nil on a node without
	// the ingress role. Closed before any listener, so its open streams end
	// rather than holding a listener's grace.
	assistant *assistantNode

	// bridge is the engine's tool bridge, nil on a node that builds none.
	// Shut down before any listener, for the assistant's reason: a coding
	// run's poll for its notes is held open for seconds
	// (internal/api/mcpbridge), and a held poll is a request a listener's
	// grace would otherwise wait out.
	bridge *mcpbridge.Bridge
}

// apiListener is one socket a node binds, the server serving it, and the slot
// holding what that server answers with.
//
// THE SOCKET IS KEPT BESIDE THE SERVER because the server does not hold it
// until Serve has registered it, and Serve runs in the background
// ([listenAPI]). Shutdown closes only the listeners Serve has registered, so a
// stop that came first — every stop a failed public bind makes, a moment after
// api.port was bound — returned with api.port still accepting connections,
// until the scheduler next ran that goroutine. [shutdownListener] closes the
// socket itself.
//
// ONE SERVER FOR THE LIFE OF THE PROCESS, and the handler is what changes
// ([handlerSlot]): the socket is bound before the boot and served the boot
// surface, then handed the node's own ([apiListener.serve]). Neither obvious
// alternative works. An http.Server's Handler must not be changed once Serve
// has started, and a second server Serving the same socket races the first
// one's Accept; and a Shutdown before the handover would close the very socket
// the handover needs.
type apiListener struct {
	server  *http.Server
	socket  net.Listener
	handler *handlerSlot
}

// serve hands this listener the handler it answers with from now on. A request
// already being answered finishes under the handler it began under.
func (l *apiListener) serve(h http.Handler) { l.handler.store(h) }

// stop closes the HTTP surface, once the engine has drained. See [shutdown]
// for why it is then and not earlier.
//
// THE PUBLIC LISTENER FIRST, then api.port. The probes are on api.port, and
// an orchestrator reads them for as long as the process lives: closing that
// socket while the public one spends its grace — which an open bridge session
// spends whole, see [apiShutdownGrace] — would fail a liveness probe on a node
// that is doing exactly what it should. Each gets its own grace, so a bridge
// session holding the public listener open never cuts a dashboard request.
func (s *httpSurface) stop(ctx context.Context, log *slog.Logger) {
	// THE ASSISTANT BEFORE EVERY LISTENER: each turn it runs ends
	// interrupted and is recorded so, which ends every stream following it,
	// and a stream is a request a listener's grace would otherwise wait out
	// whole. The engine its ends are written to has not been torn down yet.
	closeAssistant(ctx, s.assistant, log)
	// AND EVERY HELD POLL FOR A RUN'S NOTES, for the same reason — one this
	// node serves from its own opening and one it relays to the session's
	// holder alike: released at once and turned away as a 503 the CLI pauses
	// on before it polls again, on this node's successor once it has the
	// seat. A relayed tool call or read is left to finish within the grace.
	s.bridge.Shutdown()
	// THE ARTIFACT ORIGIN FIRST: it serves no probe, and an artifact frame
	// is the request most able to wait for a node that is going away.
	if s.artifacts != nil {
		shutdownListener(ctx, s.artifacts, "artifacts", log)
	}
	if s.stopArtifacts != nil {
		s.stopArtifacts()
	}
	if s.public != nil {
		shutdownListener(ctx, s.public, "public", log)
	}
	if s.server != nil {
		shutdownListener(ctx, s.server, "api", log)
	}
	grace, cancel := context.WithTimeout(ctx, apiShutdownGrace)
	defer cancel()
	// After the listeners, so no socket can be reading the projection while
	// its feed is torn down, and after the drain, so a dashboard watching
	// the drain saw its turns finish. Both are nil on a node without the
	// ingress role, which has neither.
	if s.projector != nil {
		s.projector.Stop(grace)
	}
	// Nil-safe, and nil on a node without ingress: it has no panel to keep.
	s.runs.Stop()
	if s.app != nil {
		s.app.Stop()
	}
	// AFTER THE LISTENER, so no request can start another rewrite, and
	// before [shutdown] tears down the engine a rewrite writes through —
	// within what is left of the same grace, and cut after it.
	if s.auth != nil {
		s.auth.Stop(grace)
	}
	log.InfoContext(ctx, "api_stopped")
}

// shutdownListener closes one listener, waiting at most [apiShutdownGrace] for
// the requests it is serving.
func shutdownListener(ctx context.Context, l *apiListener, listener string, log *slog.Logger) {
	grace, cancel := context.WithTimeout(ctx, apiShutdownGrace)
	defer cancel()
	if err := l.server.Shutdown(grace); err != nil {
		// The grace is over and a request is still running. It is CUT,
		// which cancels its context, rather than left to run on into
		// the teardown that is about to close the store and the broker
		// it is reading.
		log.WarnContext(ctx, "api_shutdown_failed", "listener", listener, "error", err,
			"detail", "requests still running after the shutdown grace were cut")
		if closeErr := l.server.Close(); closeErr != nil {
			log.WarnContext(ctx, "api_close_failed", "listener", listener, "error", closeErr)
		}
	}
	// AND THE SOCKET, whether or not Serve had registered it: see
	// [apiListener]. Already closed is the ordinary case, where Serve had,
	// and a Serve that starts after this finds the server shutting down and
	// returns without accepting.
	if err := l.socket.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.WarnContext(ctx, "api_close_failed", "listener", listener, "error", err)
	}
}

// apiShutdownGrace bounds how long a listener waits for in-flight REQUESTS,
// and CUTS whatever is still running when it expires.
//
// Requests, not turns: the turns are the drain's to wait for, and that one is
// bounded by the process supervisor rather than by a constant here. Five
// seconds is sized for what a REST read or a webhook's signature-check-plus-
// publish takes, which is milliseconds, so on an ordinary shutdown this is
// spent only by a client that stopped reading its own response.
//
// IT IS NOT AN UPPER BOUND ON EVERY REQUEST THIS LISTENER CAN BE HOLDING, and
// the exception is deliberate on both sides. The drain keeps serving
// [mcpbridge.PathPrefix], whose sessions carry NO idle timeout on purpose (see
// [mcpbridge.Bridge.Handler]) because a coding agent legitimately goes quiet
// for as long as a build takes. A node stopped while such a session is open
// therefore spends the whole grace here and logs api_shutdown_failed, and that
// is the honest outcome rather than a fault: the run is one this process will
// never resume, its record is in the fleet's coordination store, and the
// alternative — a listener held open for a build — is the thing the grace
// exists to stop.
const apiShutdownGrace = 5 * time.Second

// seedPauses puts every paused seat on the live projection, by the agent id the
// projection keys seats by. A pause whose seat the company no longer has is
// skipped: the activation that removed the seat clears it.
func seedPauses(ctx context.Context, e *engine.Engine, live *livestate.LiveState) error {
	pauses, err := e.Backends().Fleet.ListSeatPauses(ctx)
	if err != nil {
		return err
	}
	company := e.Company()
	if company == nil || company.Org == nil {
		return nil
	}
	seeds := make([]livestate.SeedPause, 0, len(pauses))
	for _, p := range pauses {
		// BY THE SEAT'S ID, the agent id derived from its handle
		// (ADR-0013), which is what the record is keyed on. A seat the
		// company no longer holds has no row to draw a pause on.
		if company.Org.AgentSeatByID(p.Seat) == nil {
			continue
		}
		seeds = append(seeds, livestate.SeedPause{AgentID: p.Seat.String(), Paused: livestate.Paused{
			By: p.By, ByKind: p.ByKind, OperatorID: p.OperatorID,
			At: p.At.UTC().Format(time.RFC3339Nano), Reason: p.Reason, StopRunning: p.StopRunning,
		}})
	}
	live.SeedPauses(seeds)
	return nil
}

// companyConfig is the company this node is serving: the document a revision
// stores and the ORG the engine built from it, or nil for both.
//
// ONE READ OF THE EPOCH giving BOTH halves, which is the whole reason it is a
// pair rather than two accessors: two reads can straddle an activation, and a
// screen that took the integrations from one revision and the roster from the
// next would describe a company that never existed.
func companyConfig(e *engine.Engine) (*config.Company, *org.Organization) {
	if company := e.Company(); company != nil {
		return company.Config, company.Org
	}
	return nil, nil
}

// seatDisplayName is the name the running org gives the seat a handle
// addresses, or the handle itself for a seat it does not hold (a retired one,
// or a node serving no company yet), read at the moment of asking so a rename
// is exported as it now reads.
func seatDisplayName(e *engine.Engine, handle string) string {
	if _, o := companyConfig(e); o != nil {
		if seat := o.SeatByHandle(handle); seat != nil && seat.Name != "" {
			return seat.Name
		}
	}
	return handle
}

// artifactDataSurfaces builds the datastore's HTTP surface, its read
// questions' source and the socket's artifact rooms over the engine's estate
// router, or none of them on an engine built with no router (which serves no
// estate to route).
//
// THE ARTIFACTS' OWN GATE ([artifactsGate]), so a datastore route, a datastore
// question, an artifacts route, an artifacts question and a room's join decide
// who may touch one artifact by one admission.
func artifactDataSurfaces(e *engine.Engine, boot *config.Bootstrap) (
	*artifactdataapi.Service, *queries.ArtifactDataSource, *api.Rooms, error) {

	data, routed := engine.ArtifactDataEstate(e)
	heads, headsRouted := engine.ArtifactsEstate(e)
	if !routed || !headsRouted {
		return nil, nil, nil, nil
	}
	gate := artifactsGate(e, boot, heads)
	surface, err := artifactdataapi.New(artifactdataapi.Options{Gate: gate, Data: data, Versions: heads,
		Names: func(handle string) string { return seatDisplayName(e, handle) }})
	if err != nil {
		return nil, nil, nil, err
	}
	var rooms *api.Rooms
	// THE ROOMS NEED THE PRESENCE HUB, which every node with a broker
	// runs: a node with none serves no room rather than rooms that never
	// say who is there.
	if hub := e.Presence(); hub != nil {
		rooms = &api.Rooms{Gate: gate, Stores: data, Presence: hub, Feed: e}
		if m := e.Recorder(); m != nil {
			rooms.Metrics = m
		}
	}
	return surface, &queries.ArtifactDataSource{Gate: gate, Reads: data}, rooms, nil
}

// companySecrets reads the verification material out of the engine's CURRENT
// epoch, on every request.
//
// Not captured once: a config reload replaces the epoch, and a receiver holding
// the old one would keep rejecting deliveries signed with a rotated secret, a
// failure that looks exactly like an attack and resolves only on restart.
func companySecrets(e *engine.Engine) webhooks.Secrets { return e.WebhookSecrets() }

// companySurface is the config surface a node writes the company document
// through, or nil on a node that holds no estate (see [runEngine]).
//
// NIL RATHER THAN BUILT OVER NOTHING: the surface refuses a write that takes
// away a human seat somebody holds, and on a node without `data` there is no
// identity directory to ask who holds one — so the surface it could build
// there would be one [configapi.New] refuses, on a node none of whose roles
// writes the document.
func companySurface(e *engine.Engine, boot *config.Bootstrap,
	cipher secrets.Cipher) (*configapi.Service, error) {

	if !engine.HoldsEstate(boot) {
		return nil, nil
	}
	return configapi.New(configapi.Options{
		Store: e.Backends().Store, Cipher: cipher,
		// THE POINTER, without which the write routes have nothing to
		// activate against. It was missing, so every /config write on
		// this binary reached a nil plane: the live-edit path that is the
		// whole of Tier B.
		Plane: e.Backends().Fleet,
		// And the nudge, so an operator's change lands on every node in
		// milliseconds rather than at the next reconcile poll.
		Queue: e.Backends().Queue,
		// THE DIRECTORY, so a write that takes away a human seat somebody
		// is bound to is refused naming them. Every node holding the estate
		// runs the identity domain from boot, company or none.
		Holders: configapi.DirectoryHolders(e.IAM()),
		// AND WHO ALONE MAY CHANGE A MANAGED DOCUMENT (ADR-0030): this
		// node's Tier A writer list, the one field of Tier A this surface
		// reads. Left out, every node served a managed company's document
		// to every credential the authority table admits.
		CompanyWriters: boot.API.Auth.CompanyWriters,
		// THE ENGINE'S OWN NAME FOR THIS NODE, never boot.Node.ID, which is
		// empty on a node named through CREWLET_NODE_ID: a write whose
		// activation goes unanswered names it as the one node holding the
		// revision it stored, the way /health and the fleet answer name it.
		Node: e.Node().ID(),
	})
}

// serveAPI hands the listeners [bindListeners] bound the node's HTTP surface,
// or reports that this node serves none.
//
// IT BINDS NOTHING. The sockets were bound before the boot, answering what
// internal/api's boot surface answers; this builds the surface they serve
// from now on and stores it into each listener's handler slot, which is the
// whole of the handover. A failure here leaves both listeners with the
// caller, which closes them.
func serveAPI(ctx context.Context, boot *config.Bootstrap, e *engine.Engine,
	reconciler *engine.Reconciler, cipher secrets.Cipher,
	configSurface *configapi.Service, dev *auth.DevPrincipal, listeners *nodeListeners,
	host engine.DeepHost, log *slog.Logger,
) (*httpSurface, error) {
	if boot.API.Port == 0 {
		// A real posture: a node with no listener at all. Saying so is the
		// point — an operator who expected an integration to work, or an
		// orchestrator's probe to answer, should learn it here rather than
		// from a webhook that never arrives or a restart loop.
		//
		// WHAT IT GAVE UP DEPENDS ON ITS ROLES. A node with the ingress role
		// is the one integrations and a dashboard expect to find, so
		// without a port every integration is deaf here. A node without it
		// serves its probes, and where it runs seats the routes its own
		// direct boxes dial on loopback — which, given no listener, reach
		// their tools only at the fleet's address, through an ingress node
		// — and a satellite with nothing inbound is a shape an operator
		// chooses. The roles are read off Tier A, whose answer is the
		// presence lease's: the profile's id is the one field it does not
		// need.
		hint := "api.port is 0, so this node binds no listener: no " +
			"orchestrator can probe it (/health, /ready)"
		switch profile := boot.Profile(""); {
		case profile.RunsIngress():
			hint = "api.port is 0, so this node serves no dashboard, no REST " +
				"API, no webhook endpoint, no probe and no executor tool " +
				"bridge; every integration is deaf here"
		case profile.RunsSeats():
			hint += ", and its direct boxes have no listener of this node's to " +
				"dial, so they reach their tools at " + mcpbridge.BaseURLVar +
				" alone"
		}
		// A WARNING ONLY WHERE NOTHING ELSE WARNED. An ingress node whose
		// DECLARED roles name ingress was told this at WARN already, by the
		// `api.port` advisory at the start ([logBootstrapAdvisories]), and
		// one fact warned about twice is a log that teaches its reader to
		// skim. That advisory leaves an UNDECLARED node.roles out on
		// purpose — every role, which is also the single-process shape
		// before anybody chose a port — so that node, which serves nothing
		// any integration can reach, is warned here instead. Every other
		// node is told what it gave up, at info.
		level := slog.LevelInfo
		if boot.Node.Roles == nil {
			level = slog.LevelWarn
		}
		log.Log(ctx, level, "api_disabled", "hint", hint)
		return nil, nil
	}
	if listeners == nil {
		return nil, fmt.Errorf("api: api.port is %d and no listener was bound for it; "+
			"bindListeners binds it before the boot", boot.API.Port)
	}

	// THE NAME THE ENGINE RUNS UNDER, not a second resolution of it. The
	// engine resolved it once, from `node.id` or CREWLET_NODE_ID, and its
	// presence lease carries exactly this value, so /health, the fleet
	// answer and a backup's trim hold name this node the way its peers
	// already do.
	nodeID := e.Node().ID()
	// THE INGRESS ROLE DECIDES, not only the port. node.roles was validated,
	// written onto the presence lease and counted by fleet_role_unmanned,
	// and nothing consulted it here, so a node told to run only seats still
	// bound api.port and answered webhooks, the dashboard and the REST
	// surface: a satellite placed on a private host for exactly that reason
	// opened a listener, and one config file could not serve both shapes.
	// The port stays the hard off switch above; this is the role saying the
	// same thing for a node whose file sets a port for its peers' sake.
	//
	// Except for its PROBES, which such a node serves on api.port, because
	// an orchestrator runs every node and has to be able to restart one that
	// wedged and wait on one that has not joined — and, beside them, the two
	// routes its own direct boxes dial, the tool bridge and the telemetry
	// receiver, to this host's loopback alone: those boxes are processes of
	// this host, handed this listener on loopback ([nodeListeners.boxHostURL]),
	// so a seat pool with no address of its own still runs its seats. Every
	// other box dials the fleet's bridge address, and the ingress node its
	// call reaches relays it here over the broker (internal/api/mcpbridge).
	// See [api.Probes].
	if !boot.Profile(nodeID).RunsIngress() {
		return serveProbes(ctx, boot, e, reconciler, listeners, log)
	}
	// The config surface is the caller's, built before this function so a
	// node with no HTTP listener still has a config WRITER (see runEngine).
	// One instance, shared by the REST routes, the socket queries and the
	// engine's own disconnect path: sealing and opening with the SAME
	// keyring the reconciler applies through, because two ciphers over one
	// store would mean a revision written here is one no node can read.
	//
	// The fleet's secret store, sealed with the SAME keyring: a value
	// written here is one this node and every peer opens with the key
	// their Tier A names, and a second cipher would make a rotation
	// readable only on the node that served the request.
	//
	// THE IDENTITY ESTATE, which the surfaces below read — the rotation's
	// second half here, and who holds a seat for /config and /health. It is
	// the engine's CORE, opened from boot, company or none, on every node
	// holding the estate — which a node serving this API always is, since
	// `ingress` needs `data` — so an engine holding none is one engine.New
	// did not build: refused here by name rather than handed to each surface
	// as the nil it reads as "no directory to ask".
	// It used to be exactly that nil on a node that had met no company, and
	// each of those surfaces grew a third value for a node that no longer
	// exists.
	directory, keyring := e.IAM(), e.IdentityKeyring()
	if directory == nil || keyring == nil {
		return nil, errors.New("api: the engine opened no identity estate — " +
			"engine.New opens it on every node holding the estate, company or not")
	}
	// The assistant estate opens beside the identity estate (engine core),
	// so a node holding one holds the other.
	assistantValues := e.AssistantReader()
	if assistantValues == nil {
		return nil, errors.New("api: the engine opened no assistant estate: " +
			"engine.New opens it beside the identity estate on every node holding the estate")
	}
	secretSurface, err := secretsapi.New(secretsapi.Options{
		Fleet: e.Backends().Fleet, Cipher: cipher,
		ActiveKeyID: boot.Secrets.ActiveKeyID,
		// AND THE IDENTITY ESTATE'S HALF of a rotation, which only a
		// record can move, and the assistant's, which a rotation waits out.
		Identity: keyring, Assistant: assistantValues,
		// AND THE CONFIG SURFACE, so a write of a value the company reads
		// re-activates the revision that reads it (ADR-0070), through the
		// same Reload `POST /config/reload` performs.
		Config: configSurface,
	})
	if err != nil {
		return nil, err
	}
	// THE WAY IN, THE DIRECTORY AND THE HUMAN WRITE SURFACE, with the two
	// credential arms the guard resolves a cookie and a machine token
	// through. Built before the options below so a node that serves none
	// says so in its own log line rather than by a route that is quietly
	// absent — and built by internal/api rather than here, because the
	// end-to-end suite serves them through the same constructor: see
	// [api.HumanSurfaces].
	humans, err := api.NewHumanSurfaces(boot, e)
	if err != nil {
		return nil, err
	}
	// AND WHAT A COMPANY WITH NOBODY IN IT DOES NEXT: its first person is
	// invited under a Tier A token like everybody after them, onto a human
	// seat the company declares — and the log says so once at boot, naming
	// the seats nothing holds yet, or the step that has to come first where
	// there are none. The directory surface above refuses a node with no
	// identity reader, so one is here.
	announceUnclaimed(ctx, e.AnyPerson, engine.SeatViewOf(e).HumanSeats,
		e.IAM().SeatClaims)

	// THE SLACK INSTALL LINKS, signed under the fleet keyring and spent on
	// the fleet's claim registry, so the node a browser returns to finishes
	// a link any node minted. Built before the setup surface, which mints
	// them, and the webhook landing below, which finishes them.
	slackInstalls, err := e.SlackInstalls(boot.Secrets.TokenMaterial())
	if err != nil {
		return nil, err
	}

	// The fleet's integration status, which both the reconcile loop and a
	// pass run from the dashboard write.
	integrationStatus, err := e.IntegrationStore()
	if err != nil {
		return nil, fmt.Errorf("api: integration status: %w", err)
	}
	// Connecting an integration from the dashboard. It writes through the
	// TWO surfaces above rather than reaching for the store and the plane
	// itself: a credential is sealed by the same store /secrets serves,
	// and the pointer to it lands through the same merge, validation and
	// activation PATCH /config performs.
	fleetStore := fleetsecrets.New(e.Backends().Fleet, cipher)
	setupSurface, err := setupapi.New(setupapi.Options{
		Config: configSurface,
		// The fleet's own store, sealed with the same keyring — the one
		// every node holds, since Tier A refuses a file without it.
		Secrets: fleetStore,
		// WHAT EACH AGENT MAY DO AT EACH VENDOR: the Agents tab's answer,
		// plan and sync, over the engine's own plan, ledger and lease.
		Access: e.SeatAccessAPI(configSurface.FleetCompany, fleetStore, configSurface.Managed).
			UseSlackInstalls(slackInstallLink(slackInstalls), slack.AwaitsInstall),
		// THIS NODE'S resolution chain, so a requirement can say whether
		// a ${VAR} actually resolved rather than only whether somebody
		// wrote one down. That gap is the silent outage the whole
		// secret_usable family exists to surface.
		Resolve: e.LookupSecret,
		// WHERE A VENDOR REACHES THIS DEPLOYMENT — the public listener's
		// address where one is configured, `api.external_url` otherwise —
		// from Tier A rather than through the resolver above: it cannot
		// change under a running process, so this surface can never answer
		// two different addresses inside one request. And the key it is
		// read from, which every refusal about the address names.
		WebhookBase:    boot.API.WebhookBase(),
		WebhookBaseKey: boot.API.WebhookBaseKey(),
		// The third-party apps this build can provision over the API, the recorder
		// their minted credentials go through, and the fleet row a pass
		// writes its findings to. That last one is the SAME row the
		// reconcile loop writes: a pass an operator ran and a tick that
		// ran a minute later must not disagree about an integration.
		Passes: e.SetupRunner(),
		Sink:   e.SetupSink,
		Status: integrationStatus,
		// AND THE CHECK A PERSON CAN MAKE BEFORE ANY OF THAT: one
		// read-only identity call per vendor, with a credential typed and
		// not yet stored.
		Probes: engine.SetupProbes(),
		// WHICH SLACK APP EACH AGENT IS. Named nowhere in the company
		// document, because the app is what issues the token; the
		// running transport learned it from Slack when it wired the
		// seat, and this is the only process that holds it.
		SlackApps: e.SlackApps,
		// ONE AGENT'S OWN GITHUB APP, which is the one thing here a
		// reconcile loop cannot do alone: an app is created by POSTing a
		// manifest from a page carrying the operator's own GitHub session.
		// The state ties the browser that comes back to the seat that
		// started and to who started it, sealed under the SAME keyring
		// every node holds, so a fleet where the two halves land on
		// different nodes still agrees.
		StateCipher: cipher,
		// And the fleet's claim registry SPENDS each state, so a link that
		// reached a log or a browser history cannot be presented a second
		// time, on this node or any other.
		StateClaims: e.Backends().Fleet,
	})
	if err != nil {
		return nil, err
	}

	// What only the engine can answer (in flight, seats, the posture and
	// the applied epoch, the tool catalogue), read live on every probe
	// through the reconciler that owns this node's config posture.
	runtime, err := api.NewEngineRuntime(e, reconciler)
	if err != nil {
		return nil, err
	}
	backups, err := backup.New(backup.Options{
		Store: e.Backends().Store,
		// WHAT THIS NODE HOLDS, from the rule the engine opens it by —
		// never whether the replicated estate happens to be open, which it
		// is not while an adoption replaces it.
		Estate: backup.HoldingFor(engine.HoldsEstate(boot)),
		// Nil on a node that dialled an external NATS cluster, whose
		// streams are backed up at the cluster. See internal/backup.
		Conn: e.Backends().Conn(),
		API:  e.Backends().API(),
		// The trim-hold register. A backup is not a counted node, so
		// without this the fleet's own trim can delete exactly the records
		// the artefact's store-to-stream gap needs to be replayable, and
		// the backup would report success.
		Holds: e.Backends().Fleet,
		// And where a finished copy is announced. Without it the trim's
		// backup term has no input at all and refuses for ever, so a
		// fleet with a working nightly backup would still never trim its
		// log.
		Backups: e.Backends().Fleet,
		// The engine's own name for this node, never boot.Node.ID: the
		// raw field is empty on a node named through CREWLET_NODE_ID, and
		// this id keys the node's trim hold and its announced backup point.
		NodeID: nodeID,
		// Every object the store copy names, streamed from the one store
		// the fleet shares and checked against the row naming it — or,
		// where they ride the broker's own bucket, asked after once its
		// stream is snapshotted.
		Objects: &backup.Objects{Open: e.OpenObject, Stat: e.StatObject,
			Stream: e.ObjectsStream()},
		// THE PROCESS'S OWN RECORDER, never a second one: the copy's
		// duration is a catalogued instrument, and two recorders in one
		// process would be two sets of series for one fleet.
		Metrics: e.Recorder(),
	})
	if err != nil {
		return nil, err
	}
	workSource, pageSource, backlinkSource, searchSource, fileSource := api.NativeSources(e)
	// THE ARTIFACTS: their routes and their read questions, each deciding
	// through the one admission they share, over the estate router so a
	// node whose own copy is out of service answers through a peer's.
	artifactsSurface, artifactsSource, err := artifactsSurfaces(e, boot)
	if err != nil {
		return nil, err
	}
	// AN ARTIFACT'S DATASTORE: its write routes and its read questions,
	// each deciding through the one admission they share, over the estate
	// router's datastore and artifacts so a node whose own copy is out of
	// service answers through a peer's.
	artifactDataSurface, artifactDataSource, artifactRooms, err := artifactDataSurfaces(e, boot)
	if err != nil {
		return nil, err
	}
	// The contextcheck exemption is for the two PUSH TICKS this constructor
	// registers — the roster re-send and the health frame. Both manufacture
	// a bounded context of their own instead of inheriting one, which is
	// what [api.tickReadBudget] and [api.App.streamHealth] both state is
	// correct: a tick is a timer, not a request, so there is nothing to
	// inherit, and a read that outlived the interval firing the next tick
	// would cost a goroutine per tick for the life of the process. Nothing
	// else reached from here creates a context.
	opts := api.Options{
		Bootstrap: boot,
		// THE COMPANY'S HALF OF A TIER A PRINCIPAL: the identity
		// directory's binding for a token's login, resolved through the
		// SAME seat view a signed-in person's seat is — so a token bound
		// to a removed seat is refused it exactly as a cookie is. The seat
		// view on a node running no company yet answers 503, never
		// seatless.
		SeatBindings: auth.SeatBindings{
			Directory: e, Chart: engine.SeatViewOf(e),
		},
		// NIL ON EVERY ORDINARY RUN — `-dev-principal` is what builds
		// one, and it was refused at boot on anything but a loopback
		// bind of a development build.
		DevPrincipal: dev,
		Runtime:      runtime,
		// WHOSE INBOX MOVED, from this engine's own Worklet applier: how a
		// person at the dashboard learns they have work.
		Inbox: e,
		// WHOSE CREDENTIALS MOVED, from this engine's own identity
		// applier — what closes the open tab of a session that ended.
		Credentials: e,
		// THE ENGINE'S OWN RECEIVER, not a second one built here. The API
		// verifies tokens this process's engine minted, and a second
		// receiver is a second construction of one decision that nothing
		// compares with the first.
		OtelReceiver: e.OtelReceiver(),
		// THE ENGINE'S OWN, not a second one. A run's session lives in
		// the object that opened it, so an API holding a different
		// bridge would resolve every token to no session and answer 401
		// to a box whose run is perfectly healthy.
		Bridge: e.Bridge(),
		// THE ENGINE'S TRAIL, which the guard counts a refused bearer
		// through.
		AuthEvents:   e.AuthEvents(),
		QueueBackend: e.Backends().Queue.Backend(),
		// A question the read surface has no source for comes back
		// unknown rather than empty, which is the difference between
		// "this node has no event log" and "the company has done
		// nothing".
		Sources: queries.Sources{
			// THE FLEET'S turn-level history: every live node's own
			// store, asked at query time, with the nodes that did not
			// answer named on every answer (ADR-0021). The ENGINE's,
			// because the roster is its lease view and the answerer it
			// registered is the other half of the same protocol.
			Events: e.History(),
			// EVERY NODE'S company days, for every named spend window
			// and its series — the replicated usage domain (ADR-0020),
			// resolved per read so an adoption's reopened estate is the
			// one answered from.
			Usage: e.UsageEstate(),
			// Read through the ENGINE's epoch rather than a captured
			// company: an apply replaces it, and a screen bound to the
			// one this process booted on would describe a company that
			// is no longer running.
			Company: func() (*config.Company, *org.Organization) { return companyConfig(e) },
			// THE LEAD RELATIONS the personal questions are scoped by —
			// somebody's inbox, their day, their person record — asked
			// through the SAME seam every other surface asks, so a lead
			// is told the same thing by a question as by the tool that
			// writes the same record. Left out, every one of those
			// decisions was UNKNOWN for ever: a lead reading a report's
			// inbox was answered 503 on every retry, on every node.
			Chart: engine.ChartAuthorityOf(e),
			// AND WHOSE RECORD SOMEBODY ELSE'S LOGIN NAMES, through the
			// same directory and seat view a request of theirs resolves
			// through — so a lead reading a report's inbox by login reads
			// the record the report's own screen reads, and not an empty
			// one under the login.
			Holders: e,
			// WHOSE CONTACT IDENTITIES THIS NODE'S REGISTRY WITHHOLDS —
			// a suspended or departed person's — so a screen's roster
			// and colleague lookup leave out exactly the people an
			// inbound message would not be attributed to.
			WithheldContacts: e.WithheldContacts,
			// AND WHAT A `${VAR}` IN A SEAT'S CONTACT IDS RESOLVES TO:
			// this node's own chain, the one its routing resolves the
			// same fields through. Who a caller IS is the identity
			// directory's answer, never a field of the chart.
			Env:   e.LookupSecret,
			Coord: e.Backends().Coord,
			Plane: e.Backends().Fleet,
			// The object collector's last report, read from the store
			// every node reads it from, for the fleet view's card.
			Objects: e.Backends().Fleet,
			Runs:    sqlledger.New(e.Backends().Store.SQL()),
			// A SEAT'S MEMORY AND ITS CONVERSATION LEDGER, answered by
			// the node HOLDING the seat. The ENGINE's reader, because
			// the answerer it registered on every node is the other
			// half of the same protocol, and a node serving no API may
			// be the one holding the seat.
			Memory: memoryReads(e),
			// The fleet's agent-to-agent authorization record. The
			// FLEET's, not this node's: a channel is opened by whichever
			// node owns the requester's seat, so a per-node read would
			// draw a different set depending on which node answered.
			Channels: e.Backends().Fleet,
			// The company's ONE knowledge backend, behind the same seam a
			// seat's own turn searches through. Nil when none is
			// configured, which leaves the question unregistered rather
			// than answering an empty search as though it had run.
			//
			// RESOLVED PER CALL. An apply rebuilds the searcher, so a
			// value read once here would search with a rotated
			// credential's predecessor, against a retired wiki, or
			// answer "no backend" forever for a company whose backend
			// came up after this line ran.
			Knowledge: e.Knowledge,
			Config:    configSurface,
			Budget:    e.Backends().Fleet,
			// WHERE A THIRD-PARTY APP REACHES THIS DEPLOYMENT — the
			// public listener's `api.public.external_url` where one is
			// configured, `api.external_url` otherwise — which is what a
			// surface's recorded endpoint is compared with. The SAME
			// accessor the reconcile loop and the setup surface above
			// stamp that endpoint from, and never the person's
			// `api.external_url` alone: behind a public listener that is
			// another socket, and every registration read as moved.
			//
			// A function still, because this whole value is built once
			// and read for the life of the process, and Tier A is what it
			// reads: the address cannot change under a running node, so
			// unlike the company reads around it this one can never
			// answer twice.
			WebhookBase: func() string { return boot.API.WebhookBase() },
			// What the reconcile loop last found for each surface. The
			// FLEET's record, not this node's: the loop is a worker duty,
			// so on a split-role deployment the node answering the
			// request is never the one that wrote the answer.
			Reconciles: func(ctx context.Context) []integration.State {
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				states, err := e.IntegrationStates(ctx)
				if err != nil {
					// NIL, which the answer renders as "cannot say"
					// rather than as "nothing has been reconciled". The
					// two send an operator in opposite directions, and
					// an unreachable coordination store is not evidence
					// about anybody's integrations.
					log.Warn("integration_status_unreadable", "error", err)
					return nil
				}
				return states
			},
			// Which surfaces a pass converges, so the roll-up can tell a
			// surface waiting for its first report from one that will
			// never get one.
			Converges: e.Converges,
			// The DURABLE record of detached coding runs, whole: the
			// board needs the row's own facts (the branch, the pause
			// TTL, the bridge's call log) that the live panel, which
			// is reconciled against this same record, does not carry.
			//
			// The FLEET's record, so the screen shows every node's
			// runs rather than this one's. A run is recovered by
			// whichever node owns its seat, which is exactly why a
			// per-node read drew a dashboard that disagreed with
			// itself depending on which node answered.
			Sandbox: sandbox.NewCoordStore(e.Backends().Fleet),
			// WHO GAVE A RUN'S RECORDED ANSWER, which only the answer's
			// seal says: opened under this node's fleet keyring.
			RunAnswers: e.AnswerSeal(),
			// A RUNNING RUN'S LIVE OUTPUT, answered by the node that
			// owns the run. The ENGINE's reader, because the answerer
			// it registered on every node is the other half of the same
			// protocol, and a node serving no API may be the owner.
			SandboxTail: sandboxTails(e),
			// WORKLET AND DOCLET through the
			// estate router, exactly as a seat's tools reach them
			// (engine.OperatorWork), so an operator and an agent looking
			// at one item see one item: answered from this node's copy
			// while it serves and from another data node's while it does
			// not.
			//
			// RESOLVED PER QUESTION, never captured here: the native
			// halves come up with the node's first company — at an
			// apply, on a node that booted with none — and a value read
			// once here was nothing for the life of such a process. A
			// question on a node that has not met its company answers
			// `503 no_active_revision`, and one about a half the company
			// keeps elsewhere (Jira, Confluence) is the question this
			// node does not have. See [api.NativeSources].
			Work:  workSource,
			Pages: pageSource,
			// A PROJECT'S FILES, the rows only — resolved per question
			// for the same reason: the bytes stream from the human write
			// surface's routes, which read them through this node's
			// object client.
			Files: fileSource,
			// WHO LINKS TO A PAGE, from this node's lexical index, which
			// derives the links from the bodies it already reads —
			// resolved per question for the same reason, since the
			// index is the native half's.
			Backlinks: backlinkSource,
			// RANKED SEARCH, gated on its own index rather than on
			// the tracker: the rows are the fleet's and the lexical
			// index is this node's own, so a node still building one
			// answers every board question and cannot rank a word.
			WorkSearch: searchSource,
			// WHAT THIS NODE CAN SAY ABOUT THE LOG'S OWN HISTORY —
			// how far each domain may be trimmed, what is stopping
			// it, and what this node costs to replace. Assembled per
			// call, because half of it is coordination that changes
			// under the answer and the other half is this node's own
			// loops.
			Retention: retentionSource(ctx, e),
			// WHAT THE FLEET HAS BACKED UP: each owner's newest
			// announced point, marked by the policy the trim takes —
			// Tier A, fixed for the life of this process.
			Backups:     e.Backends().Fleet,
			BackupFloor: boot.Stream.TrackerRetention.Floor(),
			NodeID:      nodeID,
			// THE ARTIFACTS, decided per artifact and shaped by the
			// reader's role; nil on an engine with no router.
			Artifacts: artifactsSource,
			// AN ARTIFACT'S DATA, decided per artifact and shaped by each
			// collection's read rule; nil on an engine with no router.
			ArtifactData: artifactDataSource,
		},
		// The fleet's record of what the log may delete, for the one
		// retention gesture the engine cannot make on its own: an
		// operator's assertion that a copy has left the host.
		Retention: e.Backends().Fleet,
		// And the eviction gate, which is a RECORD on every
		// identity-claiming log rather than a coordination write —
		// judged once, written to each log, and answered per log with
		// the same three-valued outcome every write has.
		Nodes: nodeGate(e),
		// The capacity window. The engine itself refuses the verb when
		// this node is publishing, so the route exists in every mode and
		// answers "you are in the wrong one" rather than 404 — which is
		// the difference between an operator reading a procedure and an
		// operator looking for a version mismatch.
		Capacity: e,
		// THE FLEET BROKER'S MEMBERSHIP: what every node advertises
		// against what the metadata group counts, read through a member,
		// and the removal of one gone for good through a member's system
		// account — which is reachable only from inside a member.
		FleetBroker: e.FleetBroker(),
		// Both estates a node holds, reachable only from inside it: the
		// store is locked to this process and the broker binds no
		// socket. See internal/backup.
		Backup: backups,
		// The move of the company's files to an S3 bucket, which only
		// the engine can make: it reads the objects and the estate that
		// names them, and ends with the fleet's coordination record.
		Objects: e,
		// THE RUNTIME AUDIT goes onto this node's own queue, whose
		// publish listener writes the event store here — the same
		// publisher the operator surface audits through.
		Audit:   e.Backends().Queue,
		Config:  configSurface,
		Secrets: secretSurface,
		Setup:   setupSurface,
		// WHETHER THIS NODE CAN ANSWER FROM THE ESTATE AT ALL, for /ready.
		// The ENGINE's own verdict rather than a second one built here: it
		// is the same question that decides whether this node may keep the
		// seats it holds, so a node taken out of rotation is the node that
		// stopped running seats, and the two can never disagree.
		//
		// [engine.Engine.SeatsServiceable] rather than
		// [engine.Engine.StateLogHydrated], deliberately. Hydration is about
		// a copy that is BEHIND, which catches up on its own — refusing
		// traffic for the whole of a legitimate boot would take a node out
		// of rotation at exactly the moment a fleet wants it back. And a
		// copy that is WRONG — rows below a trim floor, an applier halted
		// on a record it cannot decode, an eviction — is no reason either:
		// it goes OUT OF SERVICE and answers nothing, and every request
		// this node takes is routed to a data node whose copy is sound.
		// What the seam refuses is a node that can no longer route: its own
		// copy out of service and its view of the fleet too stale to name
		// another, which no copy of its own makes up for.
		//
		// A METHOD VALUE, not a snapshot: every term under it moves while
		// the process runs, and a cached answer is a node reporting ready
		// through the whole window in which it stopped being so. It takes
		// the REQUEST's context, so a probe against a slow broker ends at
		// its own deadline rather than at the process's.
		Estate: e.SeatsServiceable,
		// WHETHER THIS COMPANY HAS ITS FIRST PERSON — the one thing an
		// unclaimed company's dashboard can read, since /health is the
		// probe every visitor reaches. Read off this node's own identity
		// rows, which every node holding the estate applies from boot, so a
		// node with no company answers for the fleet's estate — and a
		// "nobody" is proved against the log's end, so a node still catching
		// up answers `unknown` rather than an empty company.
		Identity: e.AnyPerson,
		// The inbound edge. It republishes onto THIS node's queue and
		// dedupes through the FLEET'S coordination store, which is what
		// makes a delivery that lands on any node wake the seat's owner
		// exactly once. A third-party app retrying reaches whichever node
		// the load balancer picks, so a claim only this node could see
		// would suppress nothing.
		Inbound: api.Inbound{
			Secrets:   func() webhooks.Secrets { return companySecrets(e) },
			Publisher: e.Backends().Queue,
			Claims:    e.Backends().Fleet,
			AppFlow:   setupSurface.AppFlow(),
			Recheck:   e,
			// AND EACH AGENT'S SLACK INSTALL, finished here when Slack
			// returns the person's browser: the same object the setup
			// surface mints install links with.
			SlackInstalls: slackInstalls,
		},
	}
	// HOW A PERSON BECOMES A PRINCIPAL, the other end of the cookie it
	// mints, the tokens /iam/credentials mints, the company's identity
	// directory, the human write surface and the ONE operator surface it
	// dispatches through (both of the operator's transports, and the
	// viewer's `acts`) — each absent on a node that serves none, and handed
	// over by the one method that keeps an absent surface absent rather
	// than mounting its routes over a nil service. See
	// [api.HumanSurfaces.Mount].
	humans.Mount(&opts)
	// THE DASHBOARD ASSISTANT, after the hand-over above: the writes a person
	// may propose are the operator surface's verdict. Its service is closed
	// by every path that returns from here without a surface, and by the
	// surface's stop once there is one.
	assistantSvc, err := mountAssistant(ctx, boot, e, host, &opts)
	if err != nil {
		return nil, err
	}
	// THE ARTIFACTS' ROUTES, each stating its authority where it is
	// mounted. Set only where it was built, so an absent surface is a nil
	// interface rather than a nil service mounted.
	if artifactsSurface != nil {
		opts.Artifacts = artifactsSurface
	}
	// THE ARTIFACT DATASTORE'S WRITES, its undo, its schema, the per-viewer
	// state and the CSV export, each deciding on the artifact's own shares
	// where it is mounted. Set only where it was built, so an absent
	// surface is a nil interface rather than a nil service mounted.
	if artifactDataSurface != nil {
		opts.ArtifactData = artifactDataSurface
	}
	// AND A DASHBOARD'S DATASETS, read from the artifacts surface's boards
	// and the datastore, each answered as its viewer (adr/0043). Set only
	// where both were built.
	if artifactsSurface != nil && artifactDataSource != nil {
		opts.Datasets = &api.DatasetsSource{Boards: artifactsSurface, Data: artifactDataSource.Reads}
	}
	// AND THE SOCKET'S ARTIFACT ROOMS, decided by that same admission and
	// fed by this engine's appliers and presence hub.
	opts.Rooms = artifactRooms
	// SETTINGS › MODELS: the model routes, writing through the config
	// surface and the fleet's store under the same keyring, audited on this
	// node's queue, and the questions that read every model's state.
	modelSurface, err := api.NewModelSurface(e, configSurface, cipher, e.Backends().Queue)
	if err != nil {
		return nil, err
	}
	opts.Models = modelSurface
	api.ModelSources(&opts.Sources, e, cipher)
	app, err := api.New(opts) //nolint:contextcheck // see the paragraph above the options
	if err == nil {
		err = assistantSvc.bindRoutes(app)
	}
	if err != nil {
		closeAssistant(ctx, assistantSvc, log)
		return nil, err
	}
	// NOTHING PUSHES "CONFIGURED" FROM HERE. [api.App.Configured] reads the
	// engine's live epoch through the same Sources.Company seam every other
	// question uses, so an apply that brings this node its first revision
	// flips it with nothing to remember to call. The flag that used to be
	// pushed at this point was set true unconditionally and never
	// corrected, which left the unconfigured posture unreachable in the
	// shipped binary: /health never said unconfigured and /ready never went
	// 503 for it.
	//
	// It is still not the POSTURE: an apply that FAILS leaves the node
	// serving the previous epoch, which is a configured node. /ready reads
	// the posture as well, and collapsing the two would take a
	// correctly-serving node out of rotation for being behind.
	app.Start(ctx)

	// The other half of the observability pipeline. The engine already
	// persists what THIS node publishes (see engine.Backends); this is what
	// puts the whole company's events on this node's dashboard, and it is a
	// broadcast subscription because a browser attached here must see turns
	// that ran anywhere — see observe.Projector.
	//
	// Started before the handover, so a socket cannot open onto a
	// projection that is not yet being fed.
	projector := observe.NewProjector(e.Backends().Queue, app.Stream())
	if err = projector.Start(ctx); err != nil {
		app.Stop()
		closeAssistant(ctx, assistantSvc, log)
		return nil, err
	}

	// AND THE HISTORY BEHIND IT, or every screen the projection feeds starts
	// at this process's boot: the activity feed, the spend rollup and the
	// per-agent rows folded from it. A restart, a deploy or a node joining a
	// fleet showed an operator a company that had apparently done nothing.
	//
	// After the subscription above, so an event published between the read
	// and the subscribe cannot fall into the gap; the projection counts one
	// that arrives both ways once. Before the handover, so the first socket
	// to open sees the seeded snapshot rather than an empty one it will never
	// be sent a correction for.
	//
	// THE FLEET'S HISTORY, read from every live node's store through the
	// engine's history reader: each node's store holds what that node
	// published, so a seed read from this one alone showed a restarted node
	// only its own share of the company.
	seedCtx, cancelSeed := context.WithTimeout(ctx, projectionSeedBudget)
	_, composed := companyConfig(e)
	err = observe.Seed(seedCtx, e.History(), composed, app.Stream().State())
	if err != nil {
		// NOT FATAL, and said out loud rather than swallowed: what a
		// failed seed costs is a feed, a spend window and a seat's last
		// turn that start now, which is invisible on the screen itself.
		log.WarnContext(ctx, "live_projection_not_seeded", "error", err,
			"hint", "the activity feed, the spend rollup and each seat's last "+
				"turn start at this process's boot; older history is still "+
				"answered by the events, turns and tokens queries")
	}
	// AND WHICH SEATS A PERSON PAUSED, from the record itself, for the
	// same reason: a pause taken last week is in no event this process
	// will hear, and a seat drawn as working while it is paused is the
	// one state a person pausing it must not be shown.
	if err = seedPauses(seedCtx, e, app.Stream().State()); err != nil {
		log.WarnContext(ctx, "seat_pauses_not_seeded", "error", err,
			"hint", "a seat paused before this process started shows as paused "+
				"from its next pause or resume; the pause itself is in force")
	}
	// AND THE RUNNING CODING RUNS, from the durable record every node
	// opens, before the handover for the seed's reason: a run parked on a
	// question for days is exactly the one the events of this process's
	// lifetime will never mention. Then every interval, which is what
	// corrects a panel whose completion event never arrived.
	runs := observe.NewSandboxReconciler(sandbox.NewCoordStore(e.Backends().Fleet), app.Stream())
	if err = runs.Reconcile(seedCtx); err != nil {
		log.WarnContext(ctx, "sandbox_panel_not_seeded", "error", err,
			"hint", "the running-runs panel starts from what this process "+
				"hears; the durable run record is read again every "+
				livestate.ReconcileInterval.String())
	}
	cancelSeed()
	runs.Start(ctx)

	surface := &httpSurface{app: app, projector: projector, runs: runs, auth: humans.SignIn,
		assistant: assistantSvc, bridge: e.Bridge()}
	// THE PUBLIC LISTENER WAS BOUND BY THE SAME RULE THE APP SERVES ONE BY
	// ([api.PublicListener]), and a disagreement is a node that would serve
	// its webhooks on no socket, or leave one bound that answers `starting`
	// for ever. Refused rather than papered over.
	if (app.Public() != nil) != (listeners.public != nil) {
		// THROUGH THE ONE STOP, with no listener on it: the projector is
		// already running, on a broadcast subscription to the engine's
		// queue, and the sign-in surface owns work of its own — nothing
		// else holds either once this returns. The listeners are the
		// caller's to close.
		surface.stop(context.WithoutCancel(ctx), log)
		return nil, fmt.Errorf("api: this node serves a public listener: %t, and bound one: %t",
			app.Public() != nil, listeners.public != nil)
	}
	// AND THE ARTIFACT ORIGIN, BUILT BEFORE THE HANDOVER, by the rule its
	// listener was bound by ([api.ArtifactsListener]): an origin this node
	// cannot build fails the boot with every socket still the caller's to
	// close, rather than serving a dashboard whose every artifact frame meets
	// a listener answering `starting` for ever.
	origin, err := artifactOrigin(boot, e, app, nodeID, listeners)
	if err != nil {
		surface.stop(context.WithoutCancel(ctx), log)
		return nil, err
	}
	// THE HANDOVER, api.port FIRST and then the public listener: the probes
	// are on api.port, so the node's own answers reach an orchestrator before
	// they reach a vendor.
	listeners.api.serve(app)
	surface.server = listeners.api
	addr, publicAddr := listeners.api.socket.Addr().String(), ""
	if listeners.public != nil {
		listeners.public.serve(app.Public())
		surface.public = listeners.public
		publicAddr = listeners.public.socket.Addr().String()
	}
	// AND THE ARTIFACT ORIGIN, the third listener, last: it serves no
	// probe and no route an outside party calls.
	if origin != nil {
		listeners.artifacts.serve(origin.handler)
		surface.artifacts, surface.stopArtifacts = listeners.artifacts, origin.stop
		log.InfoContext(ctx, "api_artifacts_listening",
			"addr", listeners.artifacts.socket.Addr().String(),
			"external_url", boot.API.Artifacts.ExternalBase(),
			// WHAT ISOLATES ONE ARTIFACT FROM ANOTHER on this deployment:
			// a real origin of its own, or an opaque one. Same-site
			// templates are served opaque, and `crewlet validate` says why.
			"mode", string(boot.ArtifactOriginMode()))
	}

	// KEPT AT THE HANDOVER, though the sockets were bound long before (see
	// api_bound): it is the moment this node serves, which is what an
	// operator timing a boot reads off it.
	log.InfoContext(ctx, "api_listening", "addr", addr,
		"external_url", boot.API.ExternalBase(),
		"second_factor", boot.API.Auth.SecondFactor(),
		// WHERE THE ROUTES OUTSIDE PARTIES CALL ARE SERVED: empty is
		// api.port itself, which is every deployment without api.public.
		"public_addr", publicAddr,
		"tokens", app.Guard().Tokens(),
		// THE BROWSER POSTURE BESIDE THE CREDENTIAL ONE. Zero is
		// same-origin only, which is what the dashboard this process
		// serves needs and what every other site gets.
		"cross_origin_sites", app.CORS().Origins())
	// THE COMPANY-DERIVED SURFACES, re-sent whenever a published company
	// changes them.
	//
	// The roster, the org tree and the tool catalogue all come from the
	// company, so no event will ever correct them: a change that adds,
	// edits or removes a seat produces nothing a projection could learn
	// from, and an overlay merge cannot express a deletion at all. Without
	// this an open dashboard renders the company it connected to until
	// someone reloads.
	//
	// So this fires on every company the engine publishes, at boot and on
	// every apply. WHOLE PAYLOADS, each replacing its predecessor: the hub
	// drops the oldest queued envelope without telling the client, so a
	// delta it dropped would be unrecoverable.
	//
	// Registered after the app exists, which is the whole reason it is a
	// setter — see Engine.SetOnCompanyPublished.
	e.SetOnCompanyPublished(func(context.Context) { app.Stream().CompanyPublished() })
	// AND THE TOOL CATALOGUE ALONE when it moves between publishes: a shared
	// MCP server that came back with other tools, or came up on a retry, is
	// refiled into the company already published — see
	// Engine.SetOnCatalogueChanged.
	e.SetOnCatalogueChanged(func(context.Context) { app.Stream().ToolsChanged() })

	return surface, nil
}

// builtOrigin is the artifact origin [artifactOrigin] built: the handler the
// api.artifacts listener serves, and the function that stops it hearing this
// node's artifacts movements.
type builtOrigin struct {
	handler http.Handler
	stop    func()
}

// artifactOrigin builds the artifact origin for the api.artifacts listener
// [bindListeners] bound, or answers nil on a node that binds none: the handler
// internal/artifactserve builds over this engine, whose ticket principals are
// re-composed through the API's own guard, so a frame's credential is decided
// by exactly the arms a request through it is.
//
// THE LISTENER WAS BOUND BY THE SAME RULE ([api.ArtifactsListener]), and a
// disagreement is a node that would serve the origin on no socket, or leave
// one bound that answers `starting` for ever. Refused rather than papered
// over, as the public listener's is.
func artifactOrigin(boot *config.Bootstrap, e *engine.Engine, app *api.App, nodeID string,
	listeners *nodeListeners,
) (*builtOrigin, error) {
	serves := api.ArtifactsListener(boot, nodeID)
	if serves != (listeners.artifacts != nil) {
		return nil, fmt.Errorf("api: this node serves an artifact origin: %t, and bound "+
			"a listener for one: %t", serves, listeners.artifacts != nil)
	}
	if !serves {
		return nil, nil
	}
	creds, err := auth.NewLineages(app.Guard(), e)
	if err != nil {
		return nil, fmt.Errorf("api: the artifact origin: %w", err)
	}
	handler, stop, err := engine.ArtifactOrigin(e, boot, creds, static.FS())
	if err != nil {
		return nil, fmt.Errorf("api: the artifact origin: %w", err)
	}
	return &builtOrigin{handler: handler, stop: stop}, nil
}

// serveProbes is serveAPI for a node whose roles leave out ingress: it hands
// api.port the node's probes, and, where it runs seats, the routes its own
// direct boxes dial, to this host's loopback alone ([api.Probes]).
//
// NOTHING ELSE, and nothing more to the network: a box off this host dials the
// fleet's bridge address, the nodes that serve the API, and the one its call
// reaches relays it to this node over the broker (internal/api/mcpbridge).
func serveProbes(ctx context.Context, boot *config.Bootstrap, e *engine.Engine,
	reconciler *engine.Reconciler, listeners *nodeListeners, log *slog.Logger,
) (*httpSurface, error) {
	opts, err := api.EngineProbeOptions(boot, e, reconciler)
	if err != nil {
		return nil, err
	}
	handler, err := api.Probes(opts)
	if err != nil {
		return nil, err
	}
	if boot.API.Artifacts.Enabled() {
		// THE ARTIFACT ORIGIN IS INGRESS'S, like the dashboard whose frames
		// it serves ([api.ArtifactsListener]): said here, at info, so a port
		// set in a file shared across roles is not read as a listener this
		// node failed to open.
		log.InfoContext(ctx, "api_artifacts_not_bound", "addr", boot.API.Artifacts.Addr(),
			"hint", "node.roles does not include ingress, so this node serves no "+
				"artifacts; api.artifacts is bound by the ingress nodes")
	}
	listeners.api.serve(handler)
	// The address this node's own direct boxes dial, empty where it hands
	// them none — a node that runs no seats, or one bound to a single address
	// loopback does not reach, whose boxes dial the fleet's address.
	own := listeners.boxHostURL(boot, opts.NodeID)
	hint := "node.roles does not include ingress, so this node serves only " +
		"/health and /ready; webhooks, the dashboard, the REST API and the " +
		"executors' tool bridge (" + mcpbridge.PathPrefix + "{token}, which relays a " +
		"box's call to this node when it holds the run's seat) are a peer's " +
		"with the ingress role"
	if own != "" {
		hint = "node.roles does not include ingress, so this node serves the " +
			"network /health and /ready alone, and this host's loopback the routes " +
			"its own direct boxes dial (" + mcpbridge.PathPrefix + "{token} and, with " +
			"a telemetry receiver, " + auth.OTLPPrefix + "); webhooks, the dashboard, " +
			"the REST API and the tool bridge every other box dials are a peer's " +
			"with the ingress role"
	}
	log.InfoContext(ctx, "api_probes_listening", "addr", listeners.api.socket.Addr().String(),
		"node", opts.NodeID, "roles", opts.Roles, "box_host_url", own, "hint", hint)
	// AND THE BRIDGE, for the held polls the surface's stop lets go of
	// first: a direct box's poll for its notes is held on this listener
	// now, as on an ingress node's ([httpSurface.stop]).
	return &httpSurface{server: listeners.api, bridge: opts.Bridge}, nil
}

// bindFunc opens the socket one of the node's listeners serves on, at the
// address the configuration names.
//
// The binary's is [bindTCP]. It is a parameter because a test that serves a
// node has to write the node's port into its configuration before the node
// binds it, and a port it found free and let go is one anything else on the
// machine can take in between: the rest of the suite opens sockets by the
// thousand, and a case that builds its engine before serving it binds only
// after that engine has booted, which under load takes seconds. So such a test
// HOLDS the socket from the moment it picks the port and hands it over here,
// where every listener the node opens is opened ([bindListeners]).
type bindFunc func(ctx context.Context, addr string) (net.Listener, error)

// bindTCP binds addr, which is how a running node opens a listener.
//
// Through a ListenConfig so a shutdown signal arriving while the bind is in
// flight aborts it, rather than leaving a listener nobody will serve from: the
// bind can block on a DNS lookup for the host.
func bindTCP(ctx context.Context, addr string) (net.Listener, error) {
	var listenCfg net.ListenConfig
	return listenCfg.Listen(ctx, "tcp", addr)
}

// listenAPI opens addr through bind and serves handler on it, in the
// background, until [apiListener.serve] hands it another.
//
// ONE PATH for every listener a node opens — api.port and the public listener,
// whichever surface each serves over its life — so they cannot drift apart on
// the timeouts that bound an unauthenticated client or on how a bind failure is
// reported.
func listenAPI(ctx context.Context, bind bindFunc, addr string, handler http.Handler,
	log *slog.Logger,
) (*apiListener, string, error) {
	listener, err := bind(ctx, addr)
	if err != nil {
		return nil, "", fmt.Errorf("api: bind %s: %w", addr, err)
	}
	slot := newHandlerSlot(handler)
	server := &http.Server{
		Handler: slot,
		// A read that never completes holds a connection open forever,
		// and the listener is the one surface an unauthenticated client
		// can reach.
		ReadHeaderTimeout: apiReadHeaderTimeout,
		IdleTimeout:       apiIdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.ErrorContext(ctx, "api_serve_failed", "error", err)
		}
	}()
	return &apiListener{server: server, socket: listener, handler: slot},
		listener.Addr().String(), nil
}

// apiReadHeaderTimeout bounds how long a client may take to send its request
// line and headers.
//
// Ten seconds is generous for a header on any real network and short enough
// that a connection opened and left silent — the cheapest denial there is
// against a listener — costs one slot for ten seconds rather than for ever.
const apiReadHeaderTimeout = 10 * time.Second

// projectionSeedBudget bounds the fleet reads that seed the live projection and
// the first read of the durable run record.
//
// The reads run side by side, so the budget is the slowest of them rather than
// their sum. On a healthy fleet each is milliseconds: indexed range scans of
// every node's own file, stopping at the projection's own bounds (the newest
// 400 event rows without their payloads, the newest 24 000 spend records of
// one day, three turns a seat), plus one listing of the run record. What sizes
// the budget is a peer that does NOT answer, which every scatter waits
// [eventfan.FleetReadBudget] for — and a seat's turn read is two scatters, so
// five seconds is those two and a second of headroom. The spend read is one
// scatter per [eventfan.PhaseTokenPage] records, six at the cap; with a peer
// silent each of them waits that budget, so the read stops where the budget
// ends and seeds the window's newest records, its coverage saying it is short.
// It stays a ceiling on the one case that matters: a fleet that will not
// answer must not hold the handover back, since until it every webhook is
// answered `503 starting` and nothing on this node accepts one. A seed that
// times out costs history on a screen, never a delivery, and the warning it
// logs says which reads it cost.
const projectionSeedBudget = 5 * time.Second

// apiIdleTimeout bounds how long a kept-alive connection may sit between
// requests.
//
// Without it net/http falls back to ReadTimeout, which is also unset here — so
// there was NO deadline at all between keep-alive requests, and a client that
// completed one request and then went quiet held its slot indefinitely. The
// neighbouring ReadHeaderTimeout makes the block look complete, which is
// exactly why this was easy to miss: it bounds the FIRST header, not the
// silence after a response.
//
// Sixty seconds, which is well above any real client's think time — the
// dashboard polls far faster and its live channel is a websocket, which leaves
// the keep-alive pool entirely once upgraded — and far below the cost of
// holding a slot for a client that has gone away without saying so.
//
// A blanket ReadTimeout is deliberately NOT the instrument: it caps the whole
// request including the body, which would put a ceiling on `crewlet backup`
// and on a large config import. Body reads are bounded per route instead.
const apiIdleTimeout = 60 * time.Second

// emitSchema writes a tier's JSON Schema.
//
// # Why the CLI carries this at all
//
// The schema is not read by the engine — the Go types in `internal/config`
// are the validator, and the schema is a SUBSET of them. It exists for the
// people and tools that author a config without running one: the
// `# yaml-language-server: $schema=` modeline at the top of every shipped
// example, a CI linter, an assistant writing a config from scratch.
//
// It is generated rather than hand-written for the reason every generated
// artifact is: the two would diverge, and a schema that red-underlines a
// config the engine would happily run teaches authors to ignore it. The
// checked-in copies under `schema/` are regenerated by this command and
// compared in CI, so a config field added without one is a failing build
// rather than a stale file nobody opens.
func emitSchema(args []string, stdout, stderr io.Writer) error {
	// THE TIER COMES OFF THE FRONT, before the flags are parsed. Go's flag
	// package stops at the first non-flag argument, so parsing first would
	// leave `-o` in the positional tail on the natural spelling —
	// `crewlet schema company -o path` — and silently write to stdout.
	name, args := splitSubject(args)
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// EXACTLY ONE tier, wherever it appeared. The tail after flag parsing
	// holds the trailing form (`schema -o path company`) and anything left
	// over in either form — a second tier, a typo — which must be an error
	// rather than a silently ignored argument.
	name, given := onePositional(fs, name)
	if given > 1 {
		fmt.Fprintf(stderr, "usage: crewlet schema [%s|%s] [-o path]\n",
			config.TierCompany, config.TierBootstrap)
		return errors.New("name at most one tier")
	}
	if name == "" {
		// THE COMPANY TIER IS THE DEFAULT because it is the one an author
		// edits: Tier A is a handful of URLs an operator writes once,
		// while Tier B is the org chart, the providers and every
		// integration — which is what an editor's completion is for.
		name = string(config.TierCompany)
	}

	tier := config.Tier(name)
	body, err := config.Schema(tier)
	if err != nil {
		return err
	}
	// A TRAILING NEWLINE, because the file is checked in and compared: a
	// generator that emits none leaves every editor and every `git diff`
	// reporting a change nobody made.
	body = append(body, '\n')

	if *out == "" {
		_, err = stdout.Write(body)
		return err
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(stdout, "wrote %s (%s tier, %d bytes)\n", *out, tier, len(body))
	return nil
}

// splitSubject peels a leading positional argument off a subcommand's args.
//
// Returns "" when the first argument is a flag, which is what lets both
// orders work: `schema company -o x` and `schema -o x company`.
func splitSubject(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// flagLogLevel is the level the command line alone asks for.
//
// The SHORTHAND WINS, because writing both is an operator asking for debug
// in the loudest way available to them.
func flagLogLevel(logLevel string, debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return logging.ParseLevel(logLevel)
}

// warnUnrecognisedLogNames says so when a level or format name did not
// resolve to what it spells.
//
// # The fallback stays; the silence does not
//
// Both of these fail SOFT on purpose — a misspelled log level must never be
// why a company will not boot, and a CI step must never lose a migration to
// a typo in an export. But a soft failure nobody is told about is exactly
// how `debug: true` went its whole life doing nothing: the operator sees
// behaviour they did not ask for, with nothing anywhere pointing at why.
// Warning costs one line on the runs that have a mistake in them and nothing
// at all on the runs that do not.
//
// It logs rather than returning an error because the FILE is where a bad
// value is refused — see config.Logging.validate. A flag and an environment
// variable belong to one invocation and may not fail it.
func warnUnrecognisedLogNames(log *slog.Logger, source,
	levelField, level, formatField, format string,
) {
	if resolved, ok := logging.ParseLevelName(level); !ok {
		log.Warn("log_level_unrecognised", "source", source, "field", levelField,
			"value", level, "using", resolved.String(),
			"want", strings.Join(levelNames(), ", "))
	}
	if resolved, ok := logging.ParseFormatName(format); !ok {
		log.Warn("log_format_unrecognised", "source", source, "field", formatField,
			"value", format, "using", string(resolved),
			"want", strings.Join(formatNames(), ", "))
	}
}

// levelNames and formatNames render the closed sets for the message above,
// FROM the sets themselves — a hand-written list here would tell an operator
// to write a value the next release stopped accepting.
func levelNames() []string {
	out := make([]string, 0, len(logging.Levels))
	for _, l := range logging.Levels {
		out = append(out, string(l))
	}
	return out
}

func formatNames() []string {
	out := make([]string, 0, len(logging.Formats))
	for _, f := range logging.Formats {
		out = append(out, string(f))
	}
	return out
}

// overrideLogging folds the run flags that override Tier A's `logging:` block
// into the document, so the verbosity, the log file and the advisories all
// read one answer: the block this run applies.
//
// # Folded, rather than layered on top where each reader looks
//
// It used to be two readers — one for the level and the shape, one for the
// file — each taking the document and the flags and deciding between them,
// and a third reader, [config.Bootstrap.Warnings], that saw the document
// alone. The third is the one that showed the cost: a run with `-debug` and
// `stream.debug: true` was told the broker's debug output had nowhere to go
// and to pass `-debug`. A rule judged on a document the run does not apply is
// a second answer to one question, so the flags are written into the
// document once, here, the way [overrideNode] writes `-roles` and the API's
// bind, and every reader after this line reads the result. The run holds the
// result to Tier A's rules ([validateFolded]) and engine.New does again, so
// what is written here has to be a document the validator accepts.
//
// # The command line wins, but only where it actually spoke
//
// A flag carries its default whether or not anyone typed it, so applying
// `-log-level` unconditionally would pin every node at info and make
// `logging.level: warn` in the file dead on arrival: a setting declared in
// Tier A and read by nothing. [isFlagSet] is what separates "the operator
// asked for info" from "nobody said anything", and it matters in both
// directions for `-log-file`, whose default is the empty string: an EXPLICIT
// `-log-file ""` is how a node with a file in its document is asked to write
// none for one invocation.
//
// `-debug` only ever RAISES: it is the shorthand for asking for debug, not a
// switch that turns the file's own setting off. An operator who wants a
// `logging.level: debug` file quieter for one run says so with
// `-log-level info`.
//
// `-log-file` moves the PATH. The file's shape, its level and its rotation
// caps stay the document's: they describe the disk this deployment runs on
// rather than this invocation, the line `-roles` and `-api-host` are drawn
// along too. Except that a run writing NO file keeps none of them, because
// a shape, a level or a cap with no path is a block the validator refuses —
// it reads as a node that logs to a file — and none of them describes
// anything this run writes.
//
// The path is TRIMMED, as Tier A trims every string it holds: validation
// normalizes the document it checks, so a path folded in untrimmed would be
// one file opened here and another named by the document the engine runs —
// and `-log-file " "` is the empty path, asking for no file.
//
// # The one combination neither half can see alone
//
// `logging.stderr: false` is checked against the file THE DOCUMENT names, so
// a `-log-file ""` that takes the file away afterwards would leave the node
// writing its log nowhere. That is refused here, where both halves are
// finally known, naming both.
func overrideLogging(boot *config.Bootstrap, fs *flag.FlagSet,
	logLevel, logFormat string, debug bool, logFile string,
) error {
	if isFlagSet(fs, "log-level") {
		boot.Logging.Level = flagLevel(logLevel)
	}
	if isFlagSet(fs, "log-format") {
		boot.Logging.Format = logging.ParseFormat(logFormat)
	}
	if isFlagSet(fs, "debug") && debug {
		boot.Logging.Level = logging.LevelDebug
	}
	if !isFlagSet(fs, "log-file") {
		return nil
	}
	if logFile = strings.TrimSpace(logFile); logFile != "" {
		boot.Logging.File.Path = logFile
		return nil
	}
	if !boot.Logging.StderrEnabled() {
		return errors.New(
			"-log-file \"\" removes the only destination this node has: " +
				"logging.stderr is false in the Tier A document, so there " +
				"would be nowhere left to write. Name a file, or set " +
				"logging.stderr back to true")
	}
	boot.Logging.File = config.LogFile{}
	return nil
}

// flagLevel is a `-log-level` value as Tier A spells a level.
//
// The flag resolves LENIENTLY ([logging.ParseLevel]: a typo is info and
// `warning` is warn), because a flag may not fail a run; what it folds into
// the document is the level it resolved TO, so the document holds one of
// [logging.Levels] — a value its own validator accepts — rather than the
// operator's spelling. slog names its four levels DEBUG, INFO, WARN and
// ERROR, which lowered are exactly that set.
func flagLevel(name string) logging.Level {
	return logging.Level(strings.ToLower(logging.ParseLevel(name).String()))
}

// attachLogFile installs the log file beside stderr and returns the teardown
// this process runs on its way out.
//
// # It fails the command rather than falling back to stderr alone
//
// Every other bad logging value in this binary resolves to a default,
// because a misspelled log level must never be why a company will not boot.
// A path is not one of those. An operator who configured a durable record
// and silently did not get one has nothing anywhere pointing at why. The
// error names the path and the permission that has to change.
//
// # Detached before it is closed, in that order
//
// A line emitted after the close would otherwise meet a closed descriptor,
// and the sink would report the process's own shutdown as a write failure on
// every one of them.
// # And the console is silenced only once the file is open
//
// Order, not taste: switching stderr off first and then failing to open the
// file would leave the process with no destination during the very failure
// it has to report.
// # report is where the two notices that cannot go through the logger go
//
// The sink's own write failures and the handover below. It is os.Stderr in
// both production call sites; it is a parameter for the same reason
// [logging.OpenFile] takes one — a notice nothing can observe is a notice
// nothing can check, and both of these exist precisely for the moments the
// logger is not doing its job. Note what is NOT happening here: this writer
// is never installed as the process-wide sink, which is the move
// [logging.Configure] exists to prevent.
func attachLogFile(settings config.LogFileSettings, stderr bool, report io.Writer) (func(), error) {
	if settings.Open.Path == "" {
		// Nothing to attach, and nothing to silence — the validator and
		// [overrideLogging] have already refused the two documents where
		// that combination could leave this node writing nowhere.
		return func() {}, nil
	}
	f, err := logging.OpenFile(settings.Open, report)
	if err != nil {
		return nil, err
	}
	sink := settings.Sink
	sink.Writer = f
	logging.SetFile(sink)
	// THE FIRST LINE IN THE FILE SAYS WHICH FILE IT IS. A log an operator
	// has to find by guessing at a relative path is one they read the
	// wrong copy of. It is ordinary telemetry, so it goes through the
	// logger and obeys the level like everything else.
	logging.Get("cli").Info("log_file_opened", "path", f.Path(), "stderr", stderr)
	if !stderr {
		// AND THE HANDOVER GOES STRAIGHT TO STDERR, not through the
		// logger, for the same reason the sink's own failure report
		// does: a logger cannot be trusted to announce that the logger
		// is about to stop writing here. The line above is emitted at
		// info, so on a `logging.level: warn` node it is filtered — and
		// an operator who ran `crewlet run` and got a terminal that
		// printed NOTHING AT ALL has no way to discover their log is in
		// a file. That silence is the exact failure this whole feature
		// is written against, and it was reachable through its own fix.
		fmt.Fprintf(report,
			"crewlet: logging to %s from here; stderr carries no further log "+
				"lines (logging.stderr is false)\n", f.Path())
		logging.SetConsole(false)
	}
	return func() {
		// THE CONSOLE COMES BACK FIRST. Detaching the file while stderr
		// was still off would leave the teardown itself — and anything
		// logging during it — with no destination at all.
		logging.SetConsole(true)
		logging.SetFile(logging.FileSink{})
		if closeErr := f.Close(); closeErr != nil {
			fmt.Fprintf(report, "crewlet: closing log file %s: %v\n",
				f.Path(), closeErr)
		}
	}, nil
}

// overrideNode applies the run flags that override Tier A.
//
// # Why these three and not a general mechanism
//
// They are the fields whose right value depends on WHERE the process is
// running rather than on what the company is: which job this node does in a
// fleet, and where its HTTP surface binds. Everything else in Tier A is a
// property of the deployment that belongs in the file, where it can be
// reviewed — a flag for each would be a second configuration surface with no
// history and no validation story.
func overrideNode(boot *config.Bootstrap, fs *flag.FlagSet,
	roles, apiHost string, apiPort int,
) error {
	if isFlagSet(fs, "roles") {
		names := splitRoles(roles)
		if len(names) == 0 {
			return errors.New("-roles was given with no role names")
		}
		// THROUGH THE SAME PARSER the file goes through, which fails
		// CLOSED: an unknown name is an error rather than a skipped
		// entry, because a typo like `-roles seat` would otherwise
		// produce a node that runs nothing and reports itself healthy.
		// The flag is applied after the file has loaded, so this is the
		// only thing that validates it.
		if _, err := placement.ParseRoles(names); err != nil {
			return fmt.Errorf("-roles: %w", err)
		}
		boot.Node.Roles = names
		// AND WHAT THE ROLES REQUIRE OF THE REST OF THE FILE. Whether a
		// node holds data decides whether its store is deleted at boot
		// and whether its broker joins as a leaf — so a flag that
		// removed `data` from a node whose file keeps a durable store
		// must be refused here, naming the file's setting, rather than
		// booting a node that is neither.
		if err := boot.ValidateRoles(); err != nil {
			return fmt.Errorf("-roles %s: %w", strings.Join(names, ","), err)
		}
	}
	if isFlagSet(fs, "api-host") {
		boot.API.Host = apiHost
	}
	if isFlagSet(fs, "api-port") {
		if apiPort < 0 || apiPort > 65535 {
			return fmt.Errorf("-api-port must be 0 (no HTTP surface) or "+
				"a port 1..65535, got %d", apiPort)
		}
		boot.API.Port = apiPort
	}
	// AND WHAT THE FILE'S LISTENERS REQUIRE OF THEM, for -roles' reason
	// above: the file was validated with its own api.port and its own roles,
	// and a flag that moved api.port onto the public listener's port, or to
	// 0 beneath one on an ingress node, would otherwise bind what validation
	// never saw. AFTER EVERY FLAG IS APPLIED, because the rule reads both: the
	// same `-api-port 0` is the documented shape of a node that binds nothing
	// under `-roles data,seats,workers` and a node answering no probe under
	// `-roles ingress`.
	if given := givenFlags(fs, "roles", "api-host", "api-port"); len(given) > 0 {
		if err := boot.ValidateListeners(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(given, " "), err)
		}
	}
	return nil
}

// validateFolded holds the document the run flags were folded into to every
// Tier A rule, refusing it naming the flags given.
//
// # Why here, when engine.New asks the same question
//
// The file passed these rules as it loaded, and [overrideNode] re-runs the
// two a node flag most often breaks — what the roles require of the file, and
// what its listeners require of each other. But a flag reaches every rule that
// reads what it moved: `-api-port 8080` on a document with no `api:` block
// turns the API on, and a node serving the API must name
// `api.external_url`. engine.New refuses that too, but only after this command
// has logged the document's advisories as though it were the one this node
// runs — a warning beside the refusal it should have come after — and without
// naming the flag that made the document wrong. Asked here, the refusal names
// the flags given and comes first.
//
// AND BEFORE ANYTHING ACTS ON IT: the listeners bind on its addresses soon
// after ([bindListeners]) and [decideSeed] opens and migrates the node's
// store, so a document a flag broke is refused as the rule it broke rather
// than as a bind error about an address, and before a store is opened for a
// configuration the engine would refuse.
//
// IT NORMALIZES in place, as [config.Bootstrap.Validate] always does, so what
// reads the document after it — the dev principal's bind check, the node id,
// the advisories — reads what the engine will run.
//
// Only when a flag that writes into the document was given: without one, the
// document is the file, which passed these rules as it loaded.
func validateFolded(boot *config.Bootstrap, fs *flag.FlagSet) error {
	given := givenFlags(fs, "roles", "api-host", "api-port",
		"log-level", "log-format", "debug", "log-file")
	if len(given) == 0 {
		return nil
	}
	if err := boot.Validate(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(given, " "), err)
	}
	return nil
}

// logBootstrapAdvisories logs every advisory Tier A raises
// ([config.Bootstrap.Warnings]), one `config_advisory` line at WARN each, in
// the order the document raises them.
//
// # Why every start, and not only `crewlet validate`
//
// validate is the moment before a deployment where the consequence can still
// change the decision, and it is a moment most deployments never have: a
// container image, a Helm chart or an operator renders the document and
// starts the process, and nobody runs a command against the file on the way.
// The log of the node running it is the one place every deployment has, and
// every advisory is about what this node does — a declined fsync, a trim
// nobody acknowledges, a member nobody counts — for as long as it runs, so a
// line written once at a validate nobody ran is one nobody read. Tier A
// changes only on a restart, so once per start is the complete record.
//
// # Its fields are the ones a person acts on
//
// `config` says which tier, the key `config_reference_unresolved` uses for
// the same question, so one query finds both; `path` is where the remedy is
// written; `detail` is the whole message, consequence first and remedy
// second, because a field cut from it would name a problem without its fix.
// No `kind`: every Tier A warning is an advisory.
func logBootstrapAdvisories(log *slog.Logger, boot *config.Bootstrap) {
	for _, w := range engine.BootstrapWarnings(boot) {
		log.Warn("config_advisory", "config", "bootstrap", "path", w.Path, "detail", w.Message)
	}
}

// givenFlags names, as `-name`, the flags among names the command line set.
func givenFlags(fs *flag.FlagSet, names ...string) []string {
	var given []string
	for _, name := range names {
		if isFlagSet(fs, name) {
			given = append(given, "-"+name)
		}
	}
	return given
}

// splitRoles reads a comma-separated role list, ignoring blank entries so a
// trailing comma is not a role named "".
func splitRoles(value string) []string {
	var out []string
	for name := range strings.SplitSeq(value, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// operatorLogLevel is the level every non-`run` command logs at.
//
// Warn unless $CREWLET_LOG_LEVEL names another. A TYPO RESOLVES TO WARN —
// this command's own default — rather than failing or drifting to info: a bad
// log level must never be why an operator cannot run a migration, and it must
// not quietly change the default either. `run` applies the same
// never-fail rule to its own flag; only the fallback differs, because its
// default is info.
//
// Recognised against the closed set explicitly, because logging.ParseLevel
// cannot report "I did not recognise that" — it answers info for everything
// it does not know, which is the right answer for `run` and the wrong one
// here.
func operatorLogLevel() slog.Level {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("CREWLET_LOG_LEVEL")))
	switch raw {
	case "debug", "info", "warn", "warning", "error":
		return logging.ParseLevel(raw)
	default:
		return slog.LevelWarn
	}
}

// operatorLogFormat is the shape every non-`run` command logs in.
//
// Console, so a migration that fails on a laptop reads like one, unless
// $CREWLET_LOG_FORMAT names another. It is the sibling of
// $CREWLET_LOG_LEVEL and exists for the same reason: these commands take no
// logging flags, and the CI step that ships a `crewlet migrate` run's output
// to a collector needs `json` from it exactly as much as `crewlet run` does.
// Without it the only lever over these commands' output was $NO_COLOR, which
// says nothing about the shape.
//
// A name this build does not know resolves to console — logging.ParseFormat
// applies the same never-fail rule to a format that operatorLogLevel applies
// to a level.
func operatorLogFormat() logging.Format {
	return logging.ParseFormat(os.Getenv("CREWLET_LOG_FORMAT"))
}

// engineConfigWriter is the engine's own way back onto the company: the same
// PATCH /config surface every other write uses — one merge, one validation,
// one compare-and-set onto the document.
type engineConfigWriter struct{ surface *configapi.Service }

func (w engineConfigWriter) Apply(ctx context.Context, patch []byte, summary string,
	by iam.Actor) error {

	_, err := w.surface.Apply(ctx, configapi.ApplyRequest{
		Patch: patch, Summary: summary, By: by, Engine: true,
	})
	return err
}

// Reload re-activates the current document, which is how a pass that sealed a
// credential gets everything built at apply time rebuilt against it.
//
// ON WHATEVER THE FLEET RUNS, expecting no revision: what it is for is every
// node rebuilding against the store as the pass left it, and a revision
// written since the pass read the company needs that rebuild as much as the
// one it read — unlike a /setup rotation, whose answer says the value it
// sealed reached the seats.
func (w engineConfigWriter) Reload(ctx context.Context, summary string, by iam.Actor) error {
	_, err := w.surface.Reload(ctx, summary, by, "")
	return err
}

// Seat and SetSeat are the per-seat write, through the entity route: a seat
// is addressed by its handle, because a merge patch cannot reach one element
// of a list without replacing the list.
func (w engineConfigWriter) Seat(ctx context.Context, handle string) ([]byte, error) {
	entity, err := w.surface.Entity(ctx, "roles", handle)
	if err != nil {
		return nil, err
	}
	return json.Marshal(entity)
}

func (w engineConfigWriter) SetSeat(
	ctx context.Context, handle string, body []byte, summary string, by iam.Actor,
) error {
	_, err := w.surface.ApplyEntity(ctx, configapi.ApplyEntityRequest{
		Kind: "roles", ID: handle, Body: body, Summary: summary, By: by, Engine: true,
	})
	return err
}

// nodeGate is the eviction gate over every identity-claiming log — the
// engine's core, which every node holding the estate runs from boot — or an
// UNTYPED nil where the engine holds none, which [api.New] then refuses by
// name. A typed nil would pass its check and panic on the first press.
//
// NOT THE TRACKER'S WRITER, which is what this was: that wrote the tracker's
// log alone, so an eviction lifted one log's pin and left the pages and
// identity logs counting the node for ever — and a company whose tracker is
// external, which still runs every log, could not evict anybody at all.
func nodeGate(e *engine.Engine) api.NodeGate {
	if g := e.NodeGate(); g != nil {
		return g
	}
	return nil
}

// retentionSource is the retention half of the API's node runtime, or nil on a
// node that armed no trim — a maintenance-mode node, which publishes nothing,
// its trim included.
//
// NIL RATHER THAN AN EMPTY DOCUMENT, on the rule every optional surface here
// follows: a process that runs no trim has no floor it published and no
// terms it evaluated, and a report of zeros would claim a fleet whose log is
// perfectly trimmed. The question is simply unregistered instead.
//
// IT TAKES THE CALLER'S CONTEXT FOR THE PROBE and not one of its own: the
// probe is a coordination read, so on a node whose store cannot be reached it
// is exactly as slow as every other one — and a `context.Background()` here
// made the ONE call that decides whether this surface exists at all the one
// call a shutting-down process could not abandon.
func retentionSource(ctx context.Context, e *engine.Engine) func(context.Context) any {
	if _, runs := e.RetentionReport(ctx); !runs {
		return nil
	}
	return func(ctx context.Context) any {
		report, _ := e.RetentionReport(ctx)
		return report
	}
}

// memoryReads is the engine's memory reader as the read surface's seam, or nil
// when the engine has none — a typed nil in the interface would register
// `agent_memory` and `conversations` over a reader that cannot answer.
func memoryReads(e *engine.Engine) queries.SeatMemory {
	if r := e.MemoryReads(); r != nil {
		return r
	}
	return nil
}

// sandboxTails is this node's run-output reader, as the read surface wants it
// — converted for [memoryReads]'s reason: a nil *TailReader in the interface
// would register a question whose every answer panics.
func sandboxTails(e *engine.Engine) queries.SandboxTails {
	if r := e.SandboxTails(); r != nil {
		return r
	}
	return nil
}
