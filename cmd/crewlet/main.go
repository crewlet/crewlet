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
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/insights"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcprelay"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/research"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/version"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/static"

	"gopkg.in/yaml.v3"
)

func main() {
	// THE PROC-RECEIVE HOOK, BEFORE ANYTHING ELSE THIS PROCESS DOES. The
	// engine's git starts this binary as `crewlet gitlet hook proc-receive`
	// inside a push; its stdout is git's protocol and its stderr the pushing
	// person's terminal, so nothing the CLI's own setup prints may reach
	// either — not a log line, not a usage line. host.HookArgs recognises the
	// spelling the engine starts it with (host.HookCommand), so the two cannot
	// drift.
	if args, ok := host.HookArgs(os.Args[1:]); ok {
		os.Exit(host.RunHook(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
	// THE LANDLOCK HELPER, next, before anything else runs: a confined host
	// box's every command is this binary re-executed to restrict itself and
	// then become the box's job (internal/sandbox's ConfineMain). It is no
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
		code := 1
		var exit *exitError
		if errors.As(err, &exit) {
			code = exit.code
		}
		if errors.Is(err, errSilent) {
			os.Exit(code)
		}
		fmt.Fprintln(os.Stderr, "crewlet:", err)
		os.Exit(code)
	}
}

// exitError ends a command with a status of its own, for a script to tell a
// result the command reports from a failure to ask.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

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
// ([nodeTokenOrEmpty]), and the client Slack's Web API is reached through.
//
// The binary passes the process's environment and default client, so the rule
// (`CREWLET_API_TOKEN` and nothing else) is unchanged. They are parameters
// because a parallel suite cannot hand each command a credential through the
// process's variable, and a case reading the process's would read what the
// machine exports (an operator's Slack refresh token a run would spend). The
// engine is handed its environment likewise ([engine.Options.Environment]).
// The `CREWLET_LOG_*` knobs configure the process's one logger and are read
// from the process.
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

	// THE OPERATOR COMMANDS ARE QUIET BY DEFAULT: they open a store, which logs a
	// migration line per schema file, noise on a one-shot command whose stdout is
	// piped or diffed. `run` sets its own level from its own flag; nothing here
	// silences a WARNING.
	//
	// QUIET IS A DEFAULT, NOT A CEILING: a half-applied migration or failed deploy
	// gate needs its detail. $CREWLET_LOG_LEVEL is the hatch, an environment
	// variable because it belongs to the invocation, not to nine commands.
	//
	// NOR FOR THE RELAY: a harness starts it as a seat's MCP server, so its stdout
	// IS the protocol stream and its environment is the harness's choice. See
	// internal/mcprelay.
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
		// THE FILE OPENS BEFORE THE FIRST THING THESE COMMANDS LOG, the warning
		// below: a misspelled $CREWLET_LOG_LEVEL belongs in the command's log, and a
		// file attached afterwards would miss that line.
		//
		// $CREWLET_LOG_FILE is the third sibling of the two above: these commands take
		// no logging flags, so a CI step wanting a `crewlet migrate` in the same
		// durable record as its node has no other way to ask. The rotation caps are
		// not env knobs; they belong to a deployment's file.
		// STDERR STAYS ON: these commands read no `logging:` block, and a one-shot
		// command whose output vanished into a file would surprise a person at a
		// terminal.
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
	case "gitlet":
		return runGitlet(environ, rest, stdout, stderr)
	case "fleet":
		return runFleet(environ, rest, stdout, stderr)
	case "feeds":
		return runFeeds(environ, rest, stdout, stderr)
	case "doctor":
		return runDoctor(environ, rest, stdout, stderr)
	case "seats":
		return runSeats(environ, rest, stdout, stderr)
	case "artifacts":
		return runArtifacts(environ, rest, stdout, stderr)
	case "chatlet":
		return runChatlet(environ, rest, stdout, stderr)
	case "llm":
		return runLLM(environ, rest, os.Stdin, stdout, stderr)
	case "search":
		return runSearch(rest, stdout, stderr)
	case "assistant":
		return runAssistant(environ, rest, stdout, stderr)
	case "mcp-relay":
		// The literal is mcprelay.Command, which the argv a harness is handed is
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
	fmt.Fprintf(w, `crewlet %s: an engine for hierarchically organized AI agent companies

Usage:
  crewlet run [flags]         Run the engine (API, agents and workers by default)
                              -roles data|ingress|seats|workers narrows what this node does
  crewlet validate [flags]    Check both config tiers, or one document (a repository's
                              .crewlet/checks.yaml among them), without starting anything
  crewlet schema [tier]       Print a tier's JSON Schema (company by default),
                              or the issue report catalogue
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
  crewlet gitlet <cmd>        Gitlet: status, verify a node's cache,
                              import a repository the node fetches (or -local, pushed
                              from this machine), export a bundle, purge-unreachable
                              (credential is git's helper)
  crewlet fleet broker <cmd>  The fleet broker's members, as the nodes advertise them
                              and as its metadata group counts them: list, and
                              remove a member that is gone for good
  crewlet feeds <cmd>         The wake feeds' dead letters, records no node could
                              read: dead-letters lists them, and discard gives
                              one's wake up for good
  crewlet doctor              The setup advisor's findings from a running node:
                              classes, subjects by ordinal and counts (-words
                              adds names and reasons); exits 2 when one blocks
  crewlet seats <cmd>         Pause a seat (-stop also ends its running turn and run)
                              or resume it: its holder, its lead, or an admin
  crewlet artifacts <cmd>     List, publish, version, share, trash and purge the
                              company's artifacts, as the token's principal
  crewlet chatlet <cmd>       Read, post and search the company's chat as your
                              seat. Admins can also delete messages and set how
                              long a room keeps them
  crewlet secrets <cmd>       Read and rotate the encrypted secret store
  crewlet config <cmd>        Import, inspect and activate company revisions
  crewlet iam <cmd>           People, seats, invitations, permissions,
                              sessions and tokens
  crewlet llm <cmd>           Log in, verify and export the harnesses every model
                              runs through
  crewlet search eval         Measure the semantic search against the exact scan,
                              on the vectors a store file actually holds
  crewlet assistant eval      Run evaluation cases through Ask Crewlet,
                              on their own scripts or against a candidate model
  crewlet mcp-relay SOCKET    Not for people: the MCP server a seat's harness
                              starts, relaying its stdio to the engine's tools
  crewlet gitlab <cmd>        Register a GitLab group's webhook and its signing secret
  crewlet github <cmd>        Report a GitHub deployment's seat accounts and hook it
  crewlet jira <cmd>          Report a Jira instance's seat accounts and projects
  crewlet slack <cmd>         Retired: each agent's Slack app is the reconcile loop's
  crewlet confluence <cmd>    Register the inbound hooks; publish markdown and tool skills into spaces
  crewlet mattermost <cmd>    Diagnose a Mattermost install
  crewlet version             Print the version
  crewlet help                Show this message

Config:
  -config   Tier A, this NODE: where its broker, store and API are (default %q)
  -company  Tier B, the COMPANY: its org, providers and integrations (default %q).
            A BOOTSTRAP seed: imported only where no company exists yet
            (none in this node's store and none the fleet runs). Otherwise it
            is ignored, loudly, and the node runs the company that exists,
            so a restart never reverts a live change. Not read at all where
            Tier A sets api.auth.company_writers.
  -import-company
            Tier B to make active NOW, over whatever the fleet is running.
            The deliberate "this file is the company again" gesture.
            Refused where Tier A sets api.auth.company_writers.
            To change a RUNNING fleet with no restart at all, use
            "crewlet config import", which goes through the node's API.

Environment:
  %s
            The token every command that talks to a running node sends,
            reads included, to whichever node its -api names: one of the
            node's api.auth.tokens values, or an access token from "crewlet
            iam token" (whose -login form signs in instead and reads none).
            It's never a flag (flags end up in shell history) and never
            read from the config file.
`, version.String(), defaultBootstrapPath, defaultCompanyPath, apiTokenEnv)
}

// The two tiers are separate files because they answer to separate people: a
// node's broker and store path are an operator's; the org chart and models the company's.
//
// Tier A is read from its file on every boot and nowhere else: it locates the
// store. Tier B is TWO flags, since a running node serves the revision the
// fleet's activation pointer names and a file only gets a document INTO it:
//
//	-company        BOOTSTRAP: imported only where no company exists yet.
//	-import-company OVERRIDE: make this file the active revision now.
//
// Override as the default let a stale company.yaml silently win over a live
// edit; bootstrap's ignore is LOUD (see reconcile.go).
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
	return fmt.Errorf("%w\n  (%s is there, and this build's Tier A default is %q; "+
		"pass it as `crewlet run %s` or with -config)",
		err, neighbour, defaultBootstrapPath, neighbour)
}

// Tier is which document a validation is about: one of the two config tiers,
// or a repository's checks file.
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
	// TierChecks is a repository's `.crewlet/checks.yaml` (gitlet.ParseChecks):
	// no config at all, but a document its author checks the same way before
	// pushing it, so a check's typo is found on a laptop rather than by the
	// first change it should have run on.
	TierChecks Tier = "checks"
)

// tiers is the closed set, for the flag's error and for validation.
var tiers = []Tier{TierAuto, TierCompany, TierBootstrap, TierChecks}

func (t Tier) Valid() bool {
	return slices.Contains(tiers, t)
}

// detectTier reads a document and says which tier it is.
//
// Operators break filename conventions, so the vocabularies come from the
// CONFIG STRUCTS (config.CompanyKeys / config.BootstrapKeys) reduced to each
// tier's unique keys; a restated list drifts.
//
// A CHECKS FILE is `checks:` beside no key either tier owns
// (TestTheTierVocabulariesShareNoKey), so a typo'd key beside it is refused by
// the checks file's strict reading, at its line.
//
// An UNDECIDABLE document is an error naming -tier, never a guess: a wrong
// guess reports every field invalid, like a genuinely broken document.
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
	_, checks := doc[checksFileKey]
	switch {
	case company > bootstrap:
		return TierCompany, nil
	case bootstrap > company:
		return TierBootstrap, nil
	case company == 0 && checks:
		return TierChecks, nil
	default:
		return "", fmt.Errorf(
			"cannot tell which tier this document is: it carries %d key(s) "+
				"only a company has and %d only a bootstrap has. Name it with "+
				"-tier company, -tier bootstrap or -tier checks", company, bootstrap)
	}
}

// checksFileKey is the one key a repository's checks file holds, which no
// config tier has.
const checksFileKey = "checks"

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
		"which document a positional file is: auto, company, bootstrap or checks (a repository's "+
			gitlet.ChecksFilePath+")")
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
			"usage: crewlet validate [<file.yaml>] [-tier auto|company|bootstrap|checks] [-json]\n"+
				"   or: crewlet validate [-config <tier-a.yaml>] [-company <tier-b.yaml>] [-json]")
		return errors.New("name at most one document")
	}
	if !Tier(*tier).Valid() {
		return fmt.Errorf("-tier %q is not one of %s", *tier, words.Or(tiers))
	}
	if file != "" {
		return validateOne(file, Tier(*tier), *asJSON, stdout)
	}
	return validateBoth(cfg, *asJSON, stdout)
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

	if res.Tier == TierChecks {
		// A REPOSITORY'S CHECKS FILE, read as the home Gitlet node reads it
		// from the target branch it governs — one parser, so a file that
		// passes here is one Gitlet runs, and every fault is
		// located at its field and its line as a config's is.
		checks, checksErr := gitlet.ParseChecks(raw)
		if checksErr != nil {
			res.Problems = config.Problems(checksErr)
			return report(stdout, res, asJSON)
		}
		names := make([]string, len(checks.Checks))
		for i, c := range checks.Checks {
			names[i] = c.Name
		}
		res.Valid = true
		res.Summary = map[string]any{"checks": len(names), "names": names}
		return report(stdout, res, asJSON)
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
		// TIER A HAS WARNINGS OF ITS OWN (a declined fsync, a trim that will never
		// advance, a peer entry no rule counts). Each describes a correct deployment
		// for somebody, so none is a refusal, and here the consequence can still
		// change the decision, before deploy. `crewlet run` logs the same list on every
		// start ([logBootstrapAdvisories]) for deployments nobody ran this for.
		//
		// APPENDED TO THE EMPTY LIST, not assigned: [config.Bootstrap.Warnings] is nil
		// when there are none, and assigning that would put `null` where the payload
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
// held to, and then builds its epoch, which reaches nothing (no broker, store
// or provider is dialled) and catches what a schema cannot, such as a seat
// whose llm names no configured provider.
//
// A document that did NOT hold up reports its references alone, whenever it
// PARSED, since a misspelled lead is worth seeing in the same pass. The
// admission half of [config.Company.Warnings] and the advisories stay off that
// path: an admission rule is refused as a PROBLEM, so printing it twice reads
// as two findings, and an advisory about what a document that RUNS will do is
// noise beside a refusal. A document that held up carries the whole set.
func checkCompany(raw []byte) (*checkedCompany, []config.Warning, error) {
	company, err := config.ParseCompanyDocument(raw)
	if err != nil {
		return nil, []config.Warning{}, err
	}
	warnings := company.ReferenceWarnings()
	if err = errors.Join(company.Validate(), company.ValidateHost(os.Geteuid())); err != nil {
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
	// BOTH TIERS' WARNINGS, for the reason both tiers' problems are reported: an
	// operator told about the other file on the next run pays twice for one edit.
	// The company's are already here (checkCompany puts them there so a refused
	// document still reports them), so Tier A's go in FRONT, the order the files
	// are named in, and the warnings neither tier can raise alone
	// ([config.TierWarnings]) between the two.
	//
	// Inserted rather than appended onto Tier A's own list, which is nil for a
	// document with no warnings: the payload promises arrays, and `append(nil)` of
	// nothing stays nil.
	res.Warnings = slices.Insert(res.Warnings, 0,
		slices.Concat(engine.BootstrapWarnings(boot),
			config.TierWarnings(boot, company.Company))...)
	// AND WHAT ONLY THE TWO TOGETHER SAY OF THIS HOST: a Tier A bind no
	// container box on this host can reach, for a company that runs code
	// work in one. Judged for the host validating, which is the host a
	// single-node install runs.
	res.Warnings = append(res.Warnings,
		engine.ContainerReachWarnings(boot, company.Company, runtime.GOOS)...)
	// AND EVERY LINE AN INTEGRATION'S OWN CREDENTIAL REPLACES in an agent's box.
	res.Warnings = append(res.Warnings, engine.BoxCredentialWarnings(company.Company)...)
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
		if err := printJSON(stdout, res); err != nil {
			return err
		}
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
	if names, ok := res.Summary["names"].([]string); ok && res.Tier == TierChecks {
		switch len(names) {
		case 0:
			return fmt.Sprintf("%s: no checks", res.File)
		case 1:
			return fmt.Sprintf("%s: 1 check (%s)", res.File, names[0])
		}
		return fmt.Sprintf("%s: %d checks (%s)", res.File, len(names), strings.Join(names, ", "))
	}
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
		return fmt.Errorf("-mode %q is not one of %s: the two maintenance modes "+
			"start no publisher, so a typo here would be a node that boots, "+
			"reports healthy and runs nothing", *mode, words.Or(statelog.MaintenanceModes))
	}
	// WHAT IS VALID ABOUT TIER A AND WORTH KNOWING, said on every start (see
	// [logBootstrapAdvisories]). Only here: after every refusal Tier A's rules
	// can draw from the file and flags ([validateFolded]), since a warning beside
	// a refusal is noise; after the log file is attached, so the lines reach the
	// durable record; and after every flag is folded in, so they describe the node
	// this run starts. What can still fail after them is not Tier A's to refuse:
	// the environment's node id, a port somebody else holds, a company file the
	// seed holds to the admission rules then, a store or broker that will not open.
	logBootstrapAdvisories(log, boot)
	// WHAT THE LAST PROCESS LEFT FOR ITS ISSUE REPORTS, before anything else
	// here can crash: its crash output moves aside for the first revision to
	// judge, and this process's is armed if the node last applied the setting on.
	nextStart, nextErr := issuereport.OpenNextStart(filepath.Dir(boot.Store.Path), issuereport.OriginOf(version.String()))
	if nextErr != nil {
		log.Warn("issue_reports_next_start_unread", "error", nextErr.Error(),
			"hint", "the node could not read or arm its crash output; a crash of this process may go unreported")
	}

	// The engine owns the process signals exclusively — one handler per
	// signal — because a graceful drain is the difference between a
	// restart that resumes cleanly and one that redelivers half-finished
	// turns. Nothing else in the process may install a handler.
	ctx, signals := interruptContext()
	defer signals.end()

	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return err
	}
	// THE LISTENERS, BOUND NOW: before the first boot step that can take time,
	// everything from [decideSeed] on. Until the node hands them its own surface
	// they answer internal/api's boot surface ([api.Boot]): /health 503 while the
	// node recovers what it holds and 200 once it has (so a startup probe bounds
	// the recovery, and nothing the company configures can hold it), /ready 503
	// throughout, and a 503 `starting` with Retry-After on every other route
	// rather than a refused connection.
	//
	// A PORT ALREADY TAKEN FAILS THE BOOT HERE, at once, not after a recovery that
	// can take minutes.
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
			opts.Metrics, opts.Mode, opts.NextStart = recorder, nodeMode, nextStart
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
		// A MIGRATION THAT STOPPED THE START is kept for the next one that
		// succeeds to report, where the setting and the build allow it.
		if keepErr := nextStart.MigrationFailed(err); keepErr != nil {
			log.Warn("issue_reports_migration_unkept", "error", keepErr.Error())
		}
		return err
	}

	// THE COMPANY'S OWN CHAT, LIVE, on EVERY node and from here on: a seat
	// raises its working indicator on whichever node its turn runs on, as
	// its member of the room's presence on this node's presence hub, so the
	// hub is the PROCESS's rather than the API's — built after the engine and attached
	// through its setter, started before it is attached. serveAPI mounts the
	// socket over it where this node serves one. See [api.EngineChatLive].
	chatLive := api.NewEngineChatLive(ctx, e)
	// abandon takes down what a boot that failed before the engine started
	// had brought up: the hub before the engine whose appliers call it.
	abandon := func() {
		chatLive.Stop(context.WithoutCancel(ctx))
		e.Stop(context.WithoutCancel(ctx))
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
		abandon()
		return err
	}

	// THE POSTURE REACHES PEERS from here, because the reconciler owns it
	// and is only built once the engine exists. Set before Start, so the
	// first heartbeat already carries it.
	e.SetPosture(func(ctx context.Context) (string, error) {
		// ReadPosture, not Posture: the fail-open serve is the probe's
		// answer, never one to broadcast to the fleet.
		posture, readErr := reconciler.ReadPosture(ctx)
		return string(posture), readErr
	})

	// AND THE GATE THAT ACTS ON IT. Reporting the posture is not applying
	// it: without this the inbound edge, the scheduler and every seat's
	// own inbox admitted work whatever this node had concluded, so a
	// `shed` reached the dashboard and the readiness probe and changed
	// nothing else. Set beside the reporter and before Start, so the
	// refusal is live from the first delivery this node could take.
	e.SetAdmits(reconciler.Admits)

	// THE CONFIG SURFACE IS BUILT HERE, NOT INSIDE serveAPI, because of a node with
	// `api.port: 0`: a worker-only node must still get a config writer. The
	// reconcile loop is armed regardless, as a FLEET SINGLETON that can land on
	// that node, and every path needing the writer would fail for the life of the
	// process.
	//
	// Writing the company document needs no listener (the surface takes a store, a
	// cipher, the plane and the queue), so it is built on every node that HOLDS THE
	// ESTATE and handed to serveAPI, which shares the ONE instance with the REST
	// routes and socket queries. A node without `data` builds none: reconcile is a
	// `workers` duty, the API an `ingress` surface, and both need `data`.
	configSurface, err := companySurface(e, boot, cipher)
	if err != nil {
		abandon()
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

	// ONE PROCESS IS BOTH ENGINE AND API, sharing one broker and one store. The
	// API makes the node reachable (every inbound webhook arrives through it).
	//
	// SERVED BEFORE THE ENGINE STARTS, where the bound listeners are handed the
	// node's own surface. The order is a fix: the engine claims seats first, and a
	// seat is not claimed until its per-role MCP children are up (21 on the Nimbus
	// example), so the inbound edge went unserved for as long as they took.
	// Nothing here needs a started engine: /health reports no seats yet, and a
	// webhook in the window is retained (mailboxes exist before claiming; see
	// Node.Start).
	surface, err := serveAPI(ctx, boot, e, reconciler, cipher, configSurface,
		chatLive.Hub(), dev, listeners, log)
	if err != nil {
		abandon()
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
		shutdown(context.WithoutCancel(ctx), e, surface, chatLive, log)
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
		// AND THE AGENTS' LAPSED GRANTS leave the document once no
		// credential holds them, with the requests nobody answered.
		go keepSweepingGrants(ctx, e, configSurface, log)
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
		// The signals were handed back before the context was cancelled
		// (see [interrupts]), so the second interrupt the drain line below
		// promises does what it says.
	case <-e.Fatal():
		fatal = e.FatalCause()
		// NO SIGNAL ARRIVED, so the hold is still on its first tier: handed
		// back here, an operator's FIRST interrupt during this drain ends it.
		// Left on the first tier, that press would only cancel a context the
		// drain is already detached from, and be swallowed.
		signals.handBack()
		log.ErrorContext(ctx, "engine_fatal", "error", fatal,
			"detail", "this node cannot go on running: it stops the way a signal "+
				"stops it and exits non-zero, for whatever supervises it to start "+
				"it again")
	}

	// Said BEFORE the drain, which can take minutes: an operator at a terminal
	// reads this line to learn what the node waits for and how to stop waiting.
	//
	// WHAT THE PROBES SAY IS CONDITIONAL on there being a listener. A node with
	// api.port 0 is a real posture (see [serveAPI]'s api_disabled line), and
	// promising it a /health that stays 200 sends the operator to a refused
	// connection, which is what a node that died mid-drain looks like.
	//
	// WHAT THE WORK ROUTES SAY is conditional on there being any: a node without
	// the ingress role serves only its probes (and a bridge whose calls belong to
	// runs already under way), so no route would start new work to refuse.
	probes := "this node binds no HTTP listener, so its drain is visible " +
		"only in this log"
	finish := "the turns already running finish"
	switch {
	case surface != nil && surface.app != nil:
		probes = "/health stays 200, /ready answers 503 and every route " +
			"that would start new work answers 503"
		finish += ", and then the git requests it is serving,"
	case surface != nil:
		probes = "/health stays 200 and /ready answers 503"
	}
	//
	// AND WHAT IT WAITS FOR IS CONDITIONAL on which connection a fatal lost.
	// With the queue's own gone the drain waits for no turn — they are
	// cancelled, see the shutdown below — and a line promising they finish
	// first would describe a drain this node is not running. The git
	// requests are waited for either way ([shutdown]), so a node serving any
	// says so in both.
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
		"detail", finish + " before this node stops; until they have, " + probes,
		"hint", press + " exits at once, leaving those turns to be " +
			"redelivered once their ack window elapses"}
	select {
	case <-e.AcksLost():
		detail := "this node's own broker connection is closed for good, so " +
			"nothing the turns still running conclude can be acknowledged and " +
			"whichever node takes each seat runs it again: they are cancelled " +
			"rather than waited for, and the drain ends at once"
		if surface != nil && surface.app != nil {
			detail += "; the git requests it is serving still finish before " +
				"this node stops"
		}
		draining = []any{"in_flight", e.Backends().Queue.InFlightCount(),
			"detail", detail}
	default:
	}
	log.InfoContext(ctx, "engine_draining", draining...)

	// Detached from the signal context, which a signal has already cancelled: the
	// drain is bounded only by the context it is given, so that one would abandon
	// every running turn. The bound belongs to whatever supervises the process (a
	// container runtime's kill grace, or the next interrupt: see [interrupts]).
	//
	// THE SAME DRAIN ON A FATAL has no kill grace and still ends. With the QUEUE'S
	// OWN connection gone it cancels running turns at once ([node.Node.Drain]):
	// their results cannot be acknowledged, so a peer reruns them and finishing
	// here would double every side effect. With only the COORDINATION STORE'S gone
	// every ack still lands, so it waits and the turns end as their lease and
	// ledger writes fail. The order after is a signal's; a second path is one more.
	shutdown(context.WithoutCancel(ctx), e, surface, chatLive, log)
	if fatal != nil {
		return fmt.Errorf("the node stopped itself: %w", fatal)
	}
	return nil
}

// shutdown is the one order a node comes down in, whatever stopped it:
//  1. DRAIN WITH THE LISTENER UP: /health and /ready keep answering while turns
//     finish, and the drain gate refuses routes that would start new work.
//  2. FINISH THE GIT REQUESTS ([api.App.DrainGit]), listener up: a clone or push
//     a close cut restarts from nothing on a peer. Peers' upload RPCs are
//     still served; the supervisor's grace is the wait's only bound.
//  3. CLOSE THE LISTENER, not before: that fails liveness during a healthy drain.
//  4. STOP THE CHAT HUB before the engine, whose appliers would announce into
//     a hub that no longer drains; detached first, with the indicator driver.
//  5. TEAR THE ENGINE DOWN: probes and git requests read the stores it closes.
//
// A nil surface or hub is skipped; [TestShutdownTakesANodeDownInOneOrder] pins it.
func shutdown(ctx context.Context, e drainingEngine, surface listeners,
	chatLive stoppingHub, log *slog.Logger) {

	e.Drain(ctx)
	surface.finishGit(ctx)
	surface.stop(ctx, log)
	chatLive.Stop(ctx)
	e.Stop(ctx)
}

// drainingEngine is the engine as [shutdown] takes it down.
type drainingEngine interface {
	Drain(ctx context.Context)
	Stop(ctx context.Context)
}

// listeners is the HTTP surface as [shutdown] closes it: the git requests it
// is serving finished first, then its listeners closed.
type listeners interface {
	finishGit(ctx context.Context)
	stop(ctx context.Context, log *slog.Logger)
}

// stoppingHub is the chat hub as [shutdown] stops it.
type stoppingHub interface{ Stop(ctx context.Context) }

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
	// seeding is the boot seeds still being read, which write into the
	// projection and so stop before the app does.
	seeding *observe.Seeding
	// drainGit waits for the git requests the API is serving — the app's
	// [api.App.DrainGit] — and is nil on a node without the ingress role,
	// which serves no git.
	drainGit func(context.Context) error

	// assistant is the Dashboard Assistant's service, nil on a node without
	// the ingress role. Closed before any listener, so its open streams end
	// rather than holding a listener's grace.
	assistant *assistantNode

	// bridge is the engine's tool bridge, nil on a node that builds none.
	// Shut down before any listener, for the assistant's reason: an executor's
	// poll for its notes is held open for seconds
	// (internal/api/mcpbridge), and a held poll is a request a listener's
	// grace would otherwise wait out.
	bridge *mcpbridge.Bridge
}

// finishGit waits for the git requests this surface is serving, once the
// engine has drained. See [shutdown] for why there, and [api.App.DrainGit] for
// what it waits for; what it could not wait out is logged there. Nil-safe, as
// [httpSurface.stop] is: a node that bound no listener serves no git.
func (s *httpSurface) finishGit(ctx context.Context) {
	if s == nil || s.drainGit == nil {
		return
	}
	_ = s.drainGit(ctx)
}

// apiListener is one socket a node binds, the server serving it, and the slot
// holding what that server answers with.
// THE SOCKET IS KEPT BESIDE THE SERVER because the server does not hold it
// until the background Serve ([listenAPI]) registers it, and Shutdown closes
// only registered listeners: a stop that came first (every failed public bind)
// left api.port accepting. [shutdownListener] closes the socket itself, then
// waits for that goroutine (served).
//
// ONE SERVER FOR THE PROCESS, with the handler changing ([handlerSlot]): bound
// before the boot, served the boot surface, then handed the node's own
// ([apiListener.serve]). A started Server's Handler must not change, a second
// server races Accept, and a Shutdown would close the needed socket.
type apiListener struct {
	server  *http.Server
	socket  net.Listener
	handler *handlerSlot
	// served is closed once the goroutine [listenAPI] started to Serve the
	// socket has returned.
	served <-chan struct{}
	// unused is every connection that has not sent a request, which the
	// server closes as its shutdown begins.
	unused *unusedConns
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
	if s == nil {
		// NO LISTENER TO CLOSE: a node that bound none.
		return
	}
	// THE ASSISTANT BEFORE EVERY LISTENER: each turn it runs ends
	// interrupted and is recorded so, which ends every stream following it,
	// and a stream is a request a listener's grace would otherwise wait out
	// whole. The engine its ends are written to has not been torn down yet.
	closeAssistant(ctx, s.assistant, log)
	// AND EVERY HELD POLL FOR A RUN'S NOTES, for the same reason — one this
	// node serves from its own opening and one it relays to the session's
	// holder alike: released at once and turned away as a 503 the harness pauses
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
	// Nil-safe, and nil on a node without ingress: it has no panel to keep,
	// and no projection to seed.
	s.seeding.Stop()
	s.runs.Stop()
	if s.app != nil {
		s.app.Stop(grace)
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
// its requests, and returns only once the socket is closed and the goroutine
// serving it has ended. listener is the listener as the log names it.
// THE SERVER'S OWN SHUTDOWN IS NOT ENOUGH: it closes only listeners Serve has
// registered, and [listenAPI] starts Serve on a goroutine. A stop that won
// that race (a failed public bind) left the socket accepting for a node that
// had reported failure; on 4 cores 42-61% of immediate stops did (Go 1.27.2).
//
// So the stop closes the socket itself after the shutdown, then waits for the
// goroutine, which returns [http.ErrServerClosed] once the server is stopping.
// The wait has no bound and cannot come first: a registered Serve runs until
// the server is told to stop.
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
	// AND THE GOROUTINE SERVING IT, which belongs to this stop: once it has
	// returned nothing of this listener runs on into the teardown, and
	// nothing it logs can land after the stop that ended it.
	<-l.served
}

// apiShutdownGrace bounds how long a listener waits for in-flight REQUESTS,
// and CUTS whatever is still running when it expires.
// Requests, not turns (the drain waits for those). Five seconds covers a REST
// read or a webhook's signature check plus publish, so only a client that
// stopped reading spends it.
//
// NOT A BOUND ON EVERY REQUEST: [mcpbridge.PathPrefix] sessions have NO idle
// timeout (see [mcpbridge.Bridge.Handler]) because a harness goes quiet for a
// build, so stopping with one open spends the grace and logs
// api_shutdown_failed, honestly: this process never resumes that run. The
// /operator/mcp stream ends as shutdown begins ([api.App.EndListens]), and
// idle connections close ([unusedConns]).
const apiShutdownGrace = 5 * time.Second

// seedPauses puts every paused seat on the live projection, by the agent id the
// projection keys seats by, and pushes the seats it moved to every open socket.
// A pause whose seat the company no longer has is skipped: the activation that
// removed the seat clears it.
func seedPauses(ctx context.Context, e *engine.Engine, live *stream.Service) error {
	pauses, err := e.Backends().Fleet.ListSeatPauses(ctx)
	if err != nil {
		return err
	}
	company := e.Company()
	if company == nil || company.Org == nil {
		// READ ALL THE SAME: a node holding no company yet has no seat a
		// pause could be drawn on, and the projection learns the record
		// was read (an item's flow waits on it; livestate's posture.go).
		live.SeedPauses(nil)
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
		// AND EACH MCP SERVER'S OAUTH GRANT, which a removal of the server
		// revokes: the engine's own.
		Grants: e,
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
		// AND WHAT A COMPANY DOES BESIDE THIS NODE'S TIER A, carried in a
		// write's answer ([configapi.Options.TierWarnings]): a person
		// turning Gitlet on reads the cross-tier consequence where they
		// made the change. The explicit budgets only ([config.TierWarnings]),
		// as `crewlet validate` compares them — what this node RESOLVED is
		// the apply's log line ([config.NodeTierWarnings]).
		TierWarnings: func(c *config.Company) []config.Warning {
			return append(config.TierWarnings(boot, c), engine.BoxCredentialWarnings(c)...)
		},
		// AND THE OPERATOR'S SIGN-IN LIMITS, which a write past one is
		// refused at this node's door by (ADR-0152).
		SignInLimits: boot.API.Auth.Limits,
		// THE ENGINE'S OWN NAME FOR THIS NODE, never boot.Node.ID, which is
		// empty on a node named through CREWLET_NODE_ID: a write whose
		// activation goes unanswered names it as the one node holding the
		// revision it stored, the way /health and the fleet answer name it.
		Node: e.Node().ID(),
		// AND WHICH AGENT SEATS A WRITE RAISES to an administration role at
		// a vendor (D10), from the vendors' own offers, so every authored
		// write asks a person's fresh step-up for one.
		Raises: e.SeatAccessRaises,
		// AND THE COMPANY'S CREDENTIAL STORE, the one /secrets writes: a
		// removal of an MCP server offers the values only it read and
		// deletes the ones the caller names.
		Secrets: fleetsecrets.New(e.Backends().Fleet, cipher),
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
//
// chatLive is the process's chat hub, which the socket is mounted over where
// this node serves the API — and only there, which is what tells the hub its
// node shows anybody what it hears.
func serveAPI(ctx context.Context, boot *config.Bootstrap, e *engine.Engine,
	reconciler *engine.Reconciler, cipher secrets.Cipher,
	configSurface *configapi.Service, chatLive *stream.ChatHub,
	dev *auth.DevPrincipal, listeners *nodeListeners, log *slog.Logger,
) (*httpSurface, error) {
	if boot.API.Port == 0 {
		// A real posture: a node with no listener at all. Saying so lets an operator
		// who expected an integration or an orchestrator probe to work learn it here
		// rather than from a silent webhook or a restart loop.
		//
		// WHAT IT GAVE UP DEPENDS ON ITS ROLES. With the ingress role, every
		// integration is deaf without a port. Without it the node serves its probes
		// and, where it runs seats, the routes its own direct boxes dial on loopback
		// (which, given no listener, reach their tools only at the fleet's address,
		// through an ingress node); a satellite with nothing inbound is a chosen
		// shape. The roles are read off Tier A, whose answer is the presence lease's:
		// the profile's id is the one field it does not need.
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
	// THE INGRESS ROLE DECIDES, not only the port: a node told to run only seats
	// must not bind api.port and answer webhooks, the dashboard and REST. The
	// port stays the hard off switch above; this is the role saying the same.
	//
	// Except for its PROBES, served on api.port so an orchestrator can restart a
	// wedged node and wait on one that has not joined, and the two routes its own
	// direct boxes dial (tool bridge, telemetry receiver) on this host's loopback
	// ([nodeListeners.boxHostURL]), so a seat pool with no address of its own
	// still runs its seats. Every other box dials the fleet's bridge address, and
	// the ingress node relays the call here over the broker (internal/api/mcpbridge).
	// See [api.Probes].
	if !boot.Profile(nodeID).RunsIngress() {
		return serveProbes(ctx, boot, e, reconciler, listeners, log)
	}
	// The config surface is the caller's, built before this function so a node
	// with no HTTP listener still has a config WRITER (see runEngine). One
	// instance serves the REST routes, socket queries and the engine's disconnect
	// path, sealing and opening with the SAME keyring the reconciler applies
	// through: a second cipher would write revisions no node can read, and make a
	// secret rotation readable only on the node that served it.
	//
	// THE IDENTITY ESTATE (rotation's second half, and who holds a seat for
	// /config and /health) is the engine's CORE, opened on every node holding the
	// estate, which a node serving this API always is. An engine holding none did
	// not come from engine.New: refused here by name, not handed on as a nil.
	directory, keyring := e.IAM(), e.IdentityKeyring()
	if directory == nil || keyring == nil {
		return nil, errors.New("api: the engine opened no identity estate, and " +
			"engine.New opens it on every node holding the estate, company or not")
	}
	// The assistant estate opens beside the identity estate (engine core),
	// so a node holding one holds the other.
	assistantValues := e.AssistantReader()
	if assistantValues == nil {
		return nil, errors.New("api: the engine opened no assistant estate: " +
			"engine.New opens it beside the identity estate on every node holding the estate")
	}
	executorValues := e.ExecutorReader()
	if executorValues == nil {
		return nil, errors.New("api: the engine opened no executor estate: " +
			"engine.New opens it beside the identity estate on every node holding the estate")
	}
	accessValues := e.AccessKeyring()
	if accessValues == nil {
		return nil, errors.New("api: the engine opened no access estate: " +
			"engine.New opens it beside the identity estate on every node holding the estate")
	}
	secretSurface, err := secretsapi.New(secretsapi.Options{
		Fleet: e.Backends().Fleet, Cipher: cipher,
		ActiveKeyID: boot.Secrets.ActiveKeyID,
		// AND THE IDENTITY ESTATE'S HALF of a rotation and the access log's
		// commands, which only a record can move, and the assistant's and the
		// executor sessions', which a rotation waits out.
		Identity: keyring, Assistant: assistantValues, Executor: executorValues,
		Access: accessValues,
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

	// Connecting an integration from the dashboard, and each agent's access
	// at each vendor: built by the one constructor the fleet suite calls too.
	setupSurface, err := api.NewSetupSurface(boot, e, configSurface, cipher, slackInstalls)
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
	// THE ENGINE'S OWN WIRING, every registered log included, so a backup
	// names each one and the trim and the backup alarm can count it.
	backups, err := backup.New(e.BackupOptions())
	if err != nil {
		return nil, err
	}
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
		// THE NODE'S OBJECTIVE METER, which every committed change the
		// socket pushes is timed into.
		Objectives: e.Objectives(),
		// WHOSE CREDENTIALS MOVED, from this engine's own identity
		// applier — what closes the open tab of a session that ended.
		Credentials: e,
		// THE COMPANY'S OWN CHAT on this node's sockets: the process's
		// hub, attached to the engine on every node and mounted here.
		ChatLive: chatLive,
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
			// THE FLEET'S turn-level history: every live data node's own
			// store, asked at query time, with the nodes that did not
			// answer named on every answer (ADR-0021). The ENGINE's,
			// because the roster is its lease view and the answerer it
			// registered is the other half of the same protocol.
			Events: e.History(),
			// EVERY DATA NODE'S company days, for every named spend window
			// and its series — the replicated usage domain (ADR-0020),
			// through the estate's router, so a node whose usage copy is
			// out of service answers from a peer's (ADR-0079).
			Usage: e.UsageReads(),
			// Read through the ENGINE's epoch rather than a captured
			// company: an apply replaces it, and a screen bound to the
			// one this process booted on would describe a company that
			// is no longer running.
			Company: func() (*config.Company, *org.Organization) { return companyConfig(e) },
			// The web pages Ask Crewlet reads, through the address check
			// every outbound call takes (ADR-0151).
			Research: research.New(httpx.Guarded(research.Timeout), time.Now),
			Searcher: e,
			// WHICH OF THE COMPANY'S OWN SERVICES THIS NODE SERVES ONLY
			// ONCE RESTARTED, so the org projection says it beside the
			// backends and a reader is not shown a workspace whose every
			// question this node refuses.
			AwaitingRestart: e.AwaitingRestart,
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
			// EACH NODE'S MCP, MODEL AND RUNNER ROWS, on its status
			// record beside its presence, counted against Coord's.
			NodeStatuses: e.Backends().Fleet,
			Plane:        e.Backends().Fleet,
			// The object collector's last report, read from the store
			// every node reads it from, for the fleet view's card — as a
			// report on the object store this node agreed with the fleet,
			// through the reader the collector's duty resumes from, so a
			// report left behind by a move of the files is none here too.
			Objects: e.Backends(),
			// A SEAT'S MEMORY, answered by the node HOLDING the seat.
			// The ENGINE's reader, because the answerer it registered on
			// every node is the other half of the same protocol, and a
			// node serving no API may be the one holding the seat.
			Memory: memoryReads(e),
			// The fleet's agent-to-agent authorization record. The
			// FLEET's, not this node's: a channel is opened by whichever
			// node owns the requester's seat, so a per-node read would
			// draw a different set depending on which node answered.
			Channels: e.Backends().Fleet,
			// The fleet's record of the operating insights it shares (ADR-0119).
			Insights: e.Backends().Fleet,
			// The advisor's analyst: the fleet's records of it, and its models (ADR-0157).
			Analysis: e.Backends().Fleet,
			Analyst:  engine.AnalystModels(e),
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
			Holds:     e.Backends().Fleet,
			// WHERE A THIRD-PARTY APP REACHES THIS DEPLOYMENT: the public listener's
			// `api.public.external_url` where configured, `api.external_url` otherwise,
			// which a surface's recorded endpoint is compared with. The SAME accessor the
			// reconcile loop and the setup surface above stamp that endpoint from, never
			// `api.external_url` alone: behind a public listener that is another socket,
			// and every registration would read as moved.
			//
			// A function because this value is built once and read for the life of the
			// process, and it reads Tier A, which cannot change under a running node.
			WebhookBase:   func() string { return boot.API.WebhookBase() },
			DashboardBase: func() string { return boot.API.ExternalBase() },
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
			// The DURABLE record of every detached executor, whole: the
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
			// EACH ASKING AGENT'S PENDING ACCESS REVIEW (ADR-0111).
			Access: e,
			// WHO GAVE A RUN'S RECORDED ANSWER, which only the answer's
			// seal says: opened under this node's fleet keyring.
			RunAnswers: e.AnswerSeal(),
			// WHAT A HELD CALL WOULD SEND, sealed on its run's row and
			// opened under this node's fleet keyring for who may approve
			// it.
			HeldCalls: sandbox.NewCallSeal(e.Cipher()),
			// A RUNNING RUN'S LIVE OUTPUT, answered by the node that
			// owns the run. The ENGINE's reader, because the answerer
			// it registered on every node is the other half of the same
			// protocol, and a node serving no API may be the owner.
			ExecutorTail: executorTails(e),
			// A SEAT'S EXECUTOR SESSIONS: the rows through the estate
			// router, so a node without `data` answers as one with it,
			// and each session's sealed bytes through the fleet's one
			// object store, opened under this node's keyring.
			ExecutorSessions: executorSessionReads(e),
			SessionObjects:   sessionObjects(e, log),
			// AND WHETHER A LAUNCH CONTINUES EACH ONE — a relaunch of its
			// turn, or its conversation's next turn — decided as that
			// launch decides it: off the FLEET's run records — the store a
			// relaunch reads its own turn's from — and the member the
			// launch would be launched on, as the node holding the seat
			// picks it, asked of that node.
			SessionRuns:     sandbox.NewCoordStore(e.Backends().Fleet),
			RelaunchTargets: e,
			// WHAT THIS NODE CAN SAY ABOUT THE LOG'S OWN HISTORY —
			// how far each domain may be trimmed, what is stopping
			// it, and what this node costs to replace. Assembled per
			// call, because half of it is coordination that changes
			// under the answer and the other half is this node's own
			// loops.
			Retention: api.RetentionSource(ctx, e),
			// WHAT THE FLEET HAS BACKED UP: each node's newest
			// announced point and the operator's newest
			// acknowledgement of each log, marked by the policy the
			// trim takes — Tier A, fixed for the life of this process.
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
		// GITLET AND THE CODE READS, ASKED PER REQUEST:
		// Gitlet is built with the native runtime, which a company
		// applied after boot brings up after this line ran. The git routes
		// are mounted where this node's roles name gitlet; the reads on every
		// node, through the estate router.
		Code: api.EngineCode(e),
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
		// THE WAKE FEEDS' DEAD LETTERS, listed and discarded through the
		// engine's one ledger over its queue, so a discard takes the claim
		// every node's expiry count reads: kept in the broker's dead-letter
		// stream, which binds no socket on the default topology.
		DeadLetters: feedDeadLetters(e),
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
		// WHETHER THIS NODE CAN ANSWER FROM THE ESTATE AT ALL, for /ready: the
		// ENGINE's own verdict, the one deciding whether it may keep its seats, so
		// the two never disagree.
		//
		// [engine.Engine.SeatsServiceable] rather than [engine.Engine.StateLogHydrated]:
		// hydration is a copy BEHIND, which catches up, and refusing traffic for a
		// legitimate boot pulls a node out of rotation when the fleet wants it back. A
		// copy that is WRONG goes OUT OF SERVICE and requests route to a sound data
		// node; what it refuses is a node that can no longer route.
		//
		// A METHOD VALUE, not a snapshot: every term moves. It takes the REQUEST's
		// context, so a probe against a slow broker ends at its own deadline.
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
	assistantSvc, err := mountAssistant(ctx, boot, e, &opts)
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
	// WORKLET, DOCLET AND CHATLET through the
	// estate router, exactly as a seat's tools reach them, and RESOLVED
	// PER QUESTION: the native halves come up with the node's first
	// company — at an apply, on a node that booted with none — so a value
	// read once here was nothing for the life of such a process. A question
	// on a node that has not met its company answers `503
	// no_active_revision`, and one about a half the company keeps
	// elsewhere is the question this node does not have. See
	// [api.NativeSources], which sets every one of them or none.
	api.NativeSources(e).Into(&opts.Sources)
	// SETTINGS › MODELS: the model routes, writing through the config
	// surface and the fleet's store under the same keyring, audited on this
	// node's queue, and the questions that read every model's state. A
	// failure here returns without a surface, so it closes the assistant,
	// as every such path after the assistant's mount does.
	modelSurface, err := api.NewModelSurface(e, configSurface, cipher, e.Backends().Queue)
	if err != nil {
		closeAssistant(ctx, assistantSvc, log)
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
	// engine's live epoch through the same Sources.Company seam as every other
	// question, so an apply bringing this node its first revision flips it with
	// nothing to remember to call.
	//
	// It is still not the POSTURE: an apply that FAILS leaves the node serving the
	// previous epoch, which is a configured node. /ready reads the posture as well,
	// and collapsing the two would take a correctly-serving node out of rotation
	// for being behind.
	app.Start(ctx)
	// AND EVERY TRACKER READ ON THIS NODE READS ITS ITEMS' FLOW from the
	// projection this app folds (ADR-0162): a node serving no API folds none,
	// and its answers say `flow_live: false`.
	e.UsePostures(app.Postures())

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
		app.Stop(context.WithoutCancel(ctx))
		closeAssistant(ctx, assistantSvc, log)
		return nil, err
	}

	// AND THE HISTORY BEHIND IT, or every screen the projection feeds (activity,
	// spend, per-agent rows) starts at this process's boot.
	// After the subscription above, so an event between read and subscribe cannot
	// fall in the gap (the projection counts a double once); before the handover,
	// so the first socket sees the seeded snapshot ([stream.Service.Seed]).
	// THE FLEET'S HISTORY comes from every live data node's store: one node's read
	// shows a restarted node only its share. WHICH SEATS A PERSON PAUSED and THE
	// EXECUTORS OUT IN THEIR BOXES come from the durable record, since no event
	// this process hears mentions them; then every interval.
	//
	// SIDE BY SIDE, EACH READ UNTIL IT LANDS ([observe.Seeding]): in sequence a
	// slow history seed spent the budget and failed the others on every boot.
	runs := observe.NewSandboxReconciler(sandbox.NewCoordStore(e.Backends().Fleet), app.Stream())
	seeding := observe.StartSeeding(ctx,
		observe.HistorySeed(e.History(), func() *org.Organization {
			_, composed := companyConfig(e)
			return composed
		}, app.Stream()),
		observe.BootSeed{
			Name:      "the paused seats",
			NotSeeded: "seat_pauses_not_seeded", Seeded: "seat_pauses_seeded",
			Hint: "a seat paused before this process started shows as paused once " +
				"the seed lands, or from its next pause or resume; the pause itself is in force",
			Attempt: func(ctx context.Context) error { return seedPauses(ctx, e, app.Stream()) },
		},
		observe.BootSeed{
			Name:      "the executors in flight",
			NotSeeded: "sandbox_panel_not_seeded", Seeded: "sandbox_panel_seeded",
			Hint: "the running-runs panel starts from what this process hears until " +
				"the durable run record is read",
			Attempt: runs.Reconcile,
		})
	seeding.Wait(ctx, projectionSeedBudget)
	runs.Start(ctx)

	surface := &httpSurface{
		app: app, projector: projector, runs: runs, seeding: seeding, auth: humans.SignIn,
		drainGit: app.DrainGit, assistant: assistantSvc, bridge: e.Bridge(),
	}
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
	// AN ASSISTANT'S LISTENING STREAM ENDS AS THIS LISTENER SHUTS DOWN. It is
	// one request held open on /operator/mcp, so the graceful shutdown waited
	// for every assistant listening until apiShutdownGrace ran out and then
	// cut them, logging api_shutdown_failed — on every stop of a node an
	// assistant was connected to, burying the warning that means a request
	// was stuck. Only api.port: the operator routes are never public.
	listeners.api.server.RegisterOnShutdown(app.EndListens)
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
		"optional_second_factor_allowed", boot.API.Auth.Limits.OptionalSecondFactor(),
		// WHERE THE ROUTES OUTSIDE PARTIES CALL ARE SERVED: empty is
		// api.port itself, which is every deployment without api.public.
		"public_addr", publicAddr,
		"tokens", app.Guard().Tokens(),
		// THE BROWSER POSTURE BESIDE THE CREDENTIAL ONE. Zero is
		// same-origin only, which is what the dashboard this process
		// serves needs and what every other site gets.
		"cross_origin_sites", app.CORS().Origins())
	// THE COMPANY-DERIVED SURFACES, re-sent whenever a published company changes
	// them. The roster, org tree and tool catalogue come from the company, so no
	// event corrects them and an overlay merge cannot express a deletion.
	//
	// WHOLE PAYLOADS, each replacing its predecessor: the hub drops the oldest
	// queued envelope silently, so a dropped delta would be unrecoverable.
	// Registered after the app exists, hence a setter (Engine.SetOnCompanyPublished).
	//
	// AND THE OPERATOR'S CATALOGUE: assistants listening on /operator/mcp are told
	// their tools changed, and every other node tells its own, at the publish
	// rather than at the next request that finds the move
	// ([api.App.OperatorToolsMoved]).
	e.SetOnCompanyPublished(func(ctx context.Context) {
		app.Stream().CompanyPublished()
		app.OperatorToolsMoved(ctx)
	})
	// AND THE TOOL CATALOGUE ALONE when it moves between publishes: a shared
	// MCP server that came back with other tools, or came up on a retry, is
	// refiled into the company already published — see
	// Engine.SetOnCatalogueChanged. The operator's catalogue carries the
	// same tools, so its listeners are told too.
	e.SetOnCatalogueChanged(func(ctx context.Context) {
		app.Stream().ToolsChanged()
		app.OperatorToolsMoved(ctx)
	})

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
// background, until [apiListener.serve] hands it another, and returns the
// listener, which [shutdownListener] ends.
//
// ONE PATH for every listener a node opens — api.port and the public listener,
// whichever surface each serves over its life — so they cannot drift apart on
// the timeouts that bound an unauthenticated client, on how a bind failure is
// reported, or on how a stop ends the goroutine serving the socket and the
// socket with it.
func listenAPI(ctx context.Context, bind bindFunc, addr string, handler http.Handler,
	log *slog.Logger,
) (*apiListener, string, error) {
	listener, err := bind(ctx, addr)
	if err != nil {
		return nil, "", fmt.Errorf("api: bind %s: %w", addr, err)
	}
	slot := newHandlerSlot(handler)
	unused := newUnusedConns()
	server := &http.Server{
		Handler: slot,
		// A read that never completes holds a connection open forever,
		// and the listener is the one surface an unauthenticated client
		// can reach.
		ReadHeaderTimeout: apiReadHeaderTimeout,
		IdleTimeout:       apiIdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
		ConnState:         unused.track,
	}
	server.RegisterOnShutdown(unused.close)
	// The goroutine belongs to the listener's stop, which waits for it
	// ([shutdownListener]).
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.ErrorContext(ctx, "api_serve_failed", "error", err)
		}
	}()
	return &apiListener{server: server, socket: listener, handler: slot, served: served, unused: unused},
		listener.Addr().String(), nil
}

// apiReadHeaderTimeout bounds how long a client may take to send its request
// line and headers.
//
// Ten seconds is generous for a header on any real network and short enough
// that a connection opened and left silent — the cheapest denial there is
// against a listener — costs one slot for ten seconds rather than for ever.
const apiReadHeaderTimeout = 10 * time.Second

// projectionSeedBudget is how long the handover waits for the boot seeds: the
// fleet reads that seed the live projection, the pause record and the first
// read of the durable run record ([observe.Seeding]).
//
// They run side by side, so the wait is the slowest. Healthy, each is
// milliseconds (indexed range scans bounded by the projection). What sizes the
// wait is a peer that does NOT answer: each scatter waits
// [eventfan.FleetReadBudget], a seat's turn read is two, so five seconds is
// those two plus a second. It is a ceiling on the handover, never on a seed:
// until then every webhook gets `503 starting`, so an unanswering fleet must
// not hold it back, and a late seed lands on every open screen.
const projectionSeedBudget = 5 * time.Second

// apiIdleTimeout bounds how long a kept-alive connection may sit between
// requests.
//
// Without it net/http falls back to the also-unset ReadTimeout, so a client
// that went quiet after a request held its slot forever. ReadHeaderTimeout
// bounds only the FIRST header, not the silence after a response.
//
// Sixty seconds is well above any real client's think time (the dashboard
// polls faster and its websocket leaves the pool) and far below the cost of a
// slot held for a vanished client. A blanket ReadTimeout is NOT the
// instrument: it caps the body too, limiting `crewlet backup` and large config
// imports; body reads are bounded per route.
const apiIdleTimeout = 60 * time.Second

// issueReportCatalogue names the engine's issue report catalogue to `crewlet
// schema`, and is the name of its checked-in file.
const issueReportCatalogue = "issue-report-catalogue"

// insightsCatalogue names the operating insights catalogue the same way.
const insightsCatalogue = "insights-catalogue"

// catalogues are the exports `crewlet schema` emits beside the two tiers.
var catalogues = map[string]func() ([]byte, error){
	issueReportCatalogue: issuereport.CatalogueJSON,
	insightsCatalogue:    insights.CatalogueJSON,
}

// emitSchema writes a tier's JSON Schema.
//
// # Why the CLI carries this at all
//
// The engine does not read the schema: the Go types in `internal/config` are
// the validator, and the schema is a SUBSET. It serves people and tools that
// author a config without running one (the `# yaml-language-server:
// $schema=` modeline in every example, a CI linter, an assistant).
//
// It is generated because a hand-written one would diverge, and a schema that
// red-underlines a config the engine runs teaches authors to ignore it. CI
// compares the copies under `schema/`, so a field added without one fails.
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
		fmt.Fprintf(stderr, "usage: crewlet schema [%s|%s|%s|%s] [-o path]\n",
			config.TierCompany, config.TierBootstrap, issueReportCatalogue, insightsCatalogue)
		return errors.New("name at most one tier")
	}
	if name == "" {
		// THE COMPANY TIER IS THE DEFAULT because it is the one an author
		// edits: Tier A is a handful of URLs an operator writes once,
		// while Tier B is the org chart, the providers and every
		// integration — which is what an editor's completion is for.
		name = string(config.TierCompany)
	}

	var body []byte
	if export, ok := catalogues[name]; ok {
		// NOT A TIER: a catalogue a relay vendors, emitted and gated beside
		// the two schemas for the same reason they are checked in. It ends in
		// its own newline.
		catalogue, err := export()
		if err != nil {
			return err
		}
		body = catalogue
	} else {
		schema, err := config.Schema(config.Tier(name))
		if err != nil {
			return err
		}
		// A TRAILING NEWLINE, because the file is checked in and compared: a
		// generator that emits none leaves every editor and every `git diff`
		// reporting a change nobody made.
		body = schema
		body = append(body, '\n')
	}

	if *out == "" {
		_, err := stdout.Write(body)
		return err
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(stdout, "wrote %s (%s, %d bytes)\n", *out, name, len(body))
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
// Both fail SOFT on purpose: a misspelled log level must never stop a company
// booting or cost a CI step its migration. But an unannounced soft failure is
// how `debug: true` did nothing for its whole life; the warning costs one line.
// It logs, not errors, because the FILE is where a bad value is refused
// (config.Logging.validate); a flag or variable belongs to one invocation and
// may not fail it.
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
// into the document, so verbosity, file and advisories read the block this
// run applies. A reader judging the document alone ([config.Bootstrap.Warnings])
// gave a second answer (`-debug` with `stream.debug: true` was told the broker's
// debug output had nowhere to go), so the flags are written in once, as
// [overrideNode] writes `-roles`; the result must pass [validateFolded].
// The command line wins only where it spoke ([isFlagSet]: a flag carries its
// default, else `-log-level` kills `logging.level: warn`). An EXPLICIT
// `-log-file ""` drops the file and its shape, level and caps, which the
// validator refuses without a path. `-debug` only RAISES; `-log-file` moves
// the PATH, TRIMMED as Tier A trims strings. `logging.stderr: false` with no
// file is refused.
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
// It fails the command rather than falling back to stderr alone: every other
// bad logging value resolves to a default, but an operator who configured a
// durable record and silently did not get one has nothing pointing at why.
//
// Detached before it is closed, or a line after the close would meet a closed
// descriptor and report the process's own shutdown as a write failure. The
// console is silenced only once the file is open, or a failed open would leave
// no destination for the failure. report carries the notices that cannot go
// through the logger (os.Stderr in production; a parameter as with
// [logging.OpenFile]); it is never the process-wide sink ([logging.Configure]).
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
// A flag reaches every rule that reads what it moved: `-api-port 8080` on a
// document with no `api:` block turns the API on, which then needs
// `api.external_url`. engine.New refuses that too, but after this command
// logged the advisories as though the document were the one this node runs, and
// without naming the flag. Asked here, the refusal names the flags and comes
// first, before [bindListeners] and [decideSeed] open anything. It NORMALIZES
// in place as [config.Bootstrap.Validate] does. Only when a flag that writes
// into the document was given; otherwise it is the validated file.
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
// Most deployments (an image, Helm chart or operator rendering the document)
// never run validate, and the node's log is the one place every deployment has.
// Tier A changes only on a restart, so once per start is the complete record.
//
// # Its fields are the ones a person acts on
//
// `config` says which tier (the key `config_reference_unresolved` uses), `path`
// is where the remedy is written, `detail` is the whole message. No `kind`.
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
// Warn unless $CREWLET_LOG_LEVEL names another. A TYPO RESOLVES TO WARN, this
// command's own default, rather than failing or drifting to info: a bad level
// must never stop a migration nor quietly change the default. `run` applies
// the same never-fail rule to its flag with info as the fallback.
//
// Recognised against the closed set explicitly, because logging.ParseLevel
// cannot report "not recognised": it answers info for everything unknown,
// right for `run` and wrong here.
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
// $CREWLET_LOG_FORMAT names another. It is the sibling of $CREWLET_LOG_LEVEL:
// these commands take no logging flags, and the CI step shipping a `crewlet
// migrate` run's output to a collector needs `json` as much as `crewlet run`
// does ($NO_COLOR says nothing about shape).
//
// A name this build does not know resolves to console: logging.ParseFormat
// applies the same never-fail rule operatorLogLevel applies to a level.
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

// Disconnect is the one write of a disconnect: the block gone and the agents
// on through it turned off, as one revision ([configapi.Service.Disconnect]).
func (w engineConfigWriter) Disconnect(ctx context.Context, kind integration.Kind, summary string,
	by iam.Actor) ([]string, error) {

	done, err := w.surface.Disconnect(ctx, configapi.DisconnectRequest{
		Kind: string(kind), Summary: summary, By: by, Engine: true,
	})
	return done.SeatsOff, err
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

// feedDeadLetters is the wake feeds' dead letters as the engine lists and
// discards them — through its one ledger over the node's queue — or an UNTYPED
// nil where the engine keeps none, which [api.New] then refuses by name. A
// typed nil would pass its check and panic on the first read.
func feedDeadLetters(e *engine.Engine) api.FeedDeadLetters {
	if d := e.FeedDeadLetters(); d != nil {
		return d
	}
	return nil
}

// memoryReads is the engine's memory reader as the read surface's seam, or nil
// when the engine has none — a typed nil in the interface would register
// `agent_memory` over a reader that cannot answer.
func memoryReads(e *engine.Engine) queries.SeatMemory {
	if r := e.MemoryReads(); r != nil {
		return r
	}
	return nil
}

// executorTails is this node's run-output reader, as the read surface wants it
// — converted for [memoryReads]'s reason: a nil *TailReader in the interface
// would register a question whose every answer panics.
func executorTails(e *engine.Engine) queries.ExecutorTails {
	if r := e.ExecutorTails(); r != nil {
		return r
	}
	return nil
}

// executorSessionReads is the executor log's reads through this node's estate
// router, as the read surface wants them, or nil on an engine with no router —
// which leaves the sessions questions unregistered rather than registered over
// a reader that cannot answer.
func executorSessionReads(e *engine.Engine) queries.ExecutorSessions {
	if reads, ok := engine.ExecutorSessionReads(e); ok {
		return reads
	}
	return nil
}

// sessionObjects is the store a session's sealed object is opened through, or
// nil — converted for [memoryReads]'s reason — on a node with no object store,
// and on one whose keyring cannot open a sealed stream, which is logged: the
// sessions are still listed there, and no transcript is read.
func sessionObjects(e *engine.Engine, log *slog.Logger) queries.SessionObjects {
	store, err := e.ExecutorSessions()
	if err != nil {
		log.Warn("executor_session_transcripts_off", "error", err.Error(),
			"detail", "this node's keyring cannot open a session's sealed stream, so it "+
				"answers no session's transcript; the keyring's own error names what to fix")
		return nil
	}
	if store == nil {
		return nil
	}
	return store
}
