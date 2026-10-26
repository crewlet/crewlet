package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
	"github.com/crewlet/crewlet/internal/secrets"
)

// `crewlet llm` — the operator's window onto the coding CLIs every model runs
// through.
//
// # Why these commands exist at all
//
// A providers.llm entry is a LOGIN — browser OAuth with PKCE, often behind
// SSO and MFA — held by a vendor's own command-line tool in a directory on
// disk, unless it runs on an API key. There is no password grant to script, and driving a
// headless browser through a vendor's login page breaks on their next
// redesign. So Crewlet does not re-implement any of it. What it controls is
// WHERE the credential lands: inside the provider's own isolated directory,
// separate from the operator's personal login on the same machine.
//
// # Why doctor is the command that matters
//
// An entry can be configured perfectly and still not work: the
// binary can be missing from the engine host, the profile's flags can have
// drifted from the installed version, the login can be present but expired,
// and — the one nothing else catches — the CLI can fail to reach the seat's
// tools at all (the `crewlet` MCP server it is handed) and answer in prose with
// nothing called, which ends every phase without its submission. Only a real
// run calling a real tool proves the last one, so `doctor` runs one.
//
// Two of the profile's claims are measured the same way, because a flag that
// stopped working is silent in both directions. The shell probe asks the CLI
// to run a command on the engine host and reports whether it did — a profile
// that denies local tools and a CLI that ran one is a hole where the isolation
// was assumed. The web probe asks it to fetch a URL, because web is the one
// local tool every profile keeps ON and a vendor's sandbox flag can cut it
// without saying so.
const llmUsage = `crewlet llm — the coding CLIs every model runs through: logins,
health and tokens

Usage:
  crewlet llm list                        Providers, agent, model and how each is signed in
  crewlet llm doctor [KEY]                Verify providers.llm entries end to end
                                          (-no-smoke skips the real calls)
  crewlet llm login KEY                   Broker the vendor's own interactive login
  crewlet llm login KEY -from-host        Adopt a login already on this machine
  crewlet llm login KEY -capture-token    Mint a headless token into the secret store
  crewlet llm login KEY -capture-token -print-token
                                          ... or to stdout, storing nothing
  crewlet llm login KEY -token-stdin      Store a token you already have
  crewlet llm login KEY -username U -password-stdin
                                          Where the CLI genuinely has a credential login
  crewlet llm status KEY                  Ask the CLI who it is logged in as
  crewlet llm logout KEY                  Revoke locally and delete the credentials
  crewlet llm export KEY [-secret-store]  Pack the login into a blob, or into the secret store
  crewlet llm import KEY                  Restore a bundle from stdin onto this host

Flags:
  -company PATH  Tier B, the company document naming the providers (default %q)
  -config PATH   Tier A, carrying the store and the secret keyring (default %q)
  -api URL       A running node to read stored values through and store a
                 token or bundle in; default is this node's engine when it
                 runs, and the fleet's store itself when it is stopped. A
                 seats node, which holds no data, names one, as does a
                 cluster member whose engine is stopped
  -home PATH     Read a host login from somewhere other than this user's home
  -no-smoke      Skip doctor's real completions (the tool call and both probes)
  -print-token   Write a captured token to stdout instead of the store (login only)
`

func runLLM(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	if sub == "" || sub == "help" {
		fmt.Fprintf(stdout, llmUsage, defaultCompanyPath, defaultBootstrapPath)
		return flag.ErrHelp
	}
	// The provider key comes off before the flags, for the same reason the
	// subcommand did: `llm doctor default -no-smoke` is the natural
	// spelling, and Go's flag package stops at the first non-flag argument.
	key, rest := splitSubject(rest)

	fs := flag.NewFlagSet("llm "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	companyPath := fs.String("company", defaultCompanyPath,
		"Tier B config: the company document naming the providers")
	// -api IS THE SAME FLAG the provisioning commands and `crewlet secrets`
	// take, and needed here for the node they rarely run on: a seats node
	// holds no data, its store is scratch, and the logins of the seats it
	// runs are made on it — so the company's stored values, and the store a
	// captured token goes to, are reached through a running node that holds
	// data, which nothing in a seats node's Tier A addresses.
	storeAt := addStoreFlags(fs)
	fromHost := fs.Bool("from-host", false,
		"adopt the login this machine already has (login only)")
	captureToken := fs.Bool("capture-token", false,
		"run the vendor's token-minting command (login only)")
	tokenStdin := fs.Bool("token-stdin", false,
		"read a headless token from stdin (login only)")
	username := fs.String("username", "",
		"username for a CLI with a real credential login (login only)")
	passwordStdin := fs.Bool("password-stdin", false,
		"read the password from stdin (login only)")
	home := fs.String("home", "",
		"read a host login from this home directory instead of the engine user's")
	secretStore := fs.Bool("secret-store", false,
		"write the bundle into the encrypted secret store (export only)")
	noSmoke := fs.Bool("no-smoke", false,
		"skip the real completions — the tool call and both isolation probes (doctor only)")
	printToken := fs.Bool("print-token", false,
		"write a captured token to stdout instead of storing it (login only)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	key, given := onePositional(fs, key)
	if given > 1 {
		return fmt.Errorf("llm %s takes one provider key, got %d", sub, given)
	}

	if !slices.Contains(llmCommands, sub) {
		fmt.Fprintf(stderr, llmUsage, defaultCompanyPath, defaultBootstrapPath)
		return fmt.Errorf("unknown llm command %q", sub)
	}

	companySecrets, err := openCompanyStore(environ, storeAt, "crewlet llm "+sub)
	if err != nil {
		return err
	}
	defer companySecrets.Close()

	ctx := context.Background()
	company, keys, resolver, err := loadLLMConfig(ctx, *companyPath, companySecrets, sub, key, stderr)
	if err != nil {
		return err
	}

	if sub == "doctor" {
		return doctorLLM(ctx, doctorEntries(company, keys, resolver), !*noSmoke, stdout)
	}
	providers, err := cliAgents(company, keys, resolver)
	if err != nil {
		return err
	}

	switch sub {
	case "list":
		return listLLMProviders(ctx, providers, stdout, stderr)
	case "login":
		return loginLLM(ctx, loginRequest{
			providers: providers, key: key, home: *home,
			fromHost: *fromHost, captureToken: *captureToken,
			tokenStdin: *tokenStdin, username: *username, passwordStdin: *passwordStdin,
			companySecrets: companySecrets, companyPath: *companyPath,
			printToken: *printToken, stdin: os.Stdin,
		}, stdout, stderr)
	case "status":
		p, err := oneProvider(providers, key)
		if err != nil {
			return err
		}
		restoreConfiguredBundle(ctx, p, stderr)
		return p.Status(ctx, stdout, stderr)
	case "logout":
		p, err := oneProvider(providers, key)
		if err != nil {
			return err
		}
		logoutErr := p.Logout(ctx, stdout, stderr)
		if logoutErr == nil {
			fmt.Fprintf(stdout, "Logged %s out and removed its credentials.\n", p.Key())
		}
		// WHETHER OR NOT THE LOGOUT AS A WHOLE SUCCEEDED: a vendor logout that
		// failed after the login was removed fails this command with the
		// logout recorded all the same, and that record is what keeps the
		// configured bundle out — which the operator revoking a compromised
		// login most needs to hear. See [cliagent.Provider.LogoutNotice].
		switch notice, err := p.LogoutNotice(); {
		case err != nil:
			fmt.Fprintf(stderr, "crewlet: %s: %v\n", p.Key(), err)
		case notice != "":
			fmt.Fprintln(stdout, notice)
		}
		return logoutErr
	case "export":
		return exportLLM(ctx, providers, key, *secretStore, companySecrets, stdout)
	case "import":
		return importLLM(ctx, providers, key, *companyPath, companySecrets, os.Stdin, stdout)
	}
	return nil
}

// llmCommands is every `crewlet llm` subcommand, checked before anything is
// read: an unknown one reads nothing of the company, and reveals nothing.
var llmCommands = []string{"list", "doctor", "login", "status", "logout", "export", "import"}

// cliAgentProvider pairs a built provider with the key it was configured
// under, in the order the company document wrote them.
type cliAgentProvider struct {
	key      string
	provider *cliagent.Provider
}

// loadLLMConfig reads the company document, the entries a subcommand
// addresses ([llmEntries]), and the resolver their references go through.
//
// The SAME resolver the provisioning CLIs use — store first, then the
// environment, read through the run's own route ([companyStore]) — because a
// token rotated into the secret store must win over a stale `.env` exported
// into this shell months ago. A command that resolved from the environment
// alone would report a provider as having no token while the running engine
// used one. Beside a running engine it reads the company's stored values
// through that engine, which is where they are while it runs: these commands
// are made to run beside the seats, under the lock beside the credentials
// directory that both take. And the store a login or an export writes into
// is the one this read went through.
//
// THE ADDRESSED ENTRIES ALONE are resolved, because beside a running engine
// each stored value comes back through a reveal logged by name: `crewlet llm
// login work` reveals what the `work` entry reads — its token, its key or
// its bundle, and the ${VAR}s its model and env name — and none of another
// entry's credentials.
func loadLLMConfig(ctx context.Context, companyPath string, companySecrets *companyStore,
	sub, key string, notes io.Writer) (*config.Company, []string, *config.Resolver, error) {

	company, err := config.LoadCompanyToRun(companyPath, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	keys, err := llmEntries(company, sub, key, companyPath)
	if err != nil {
		return nil, nil, nil, err
	}
	resolver, err := companySecrets.resolver(ctx, llmEntryReads(company, keys), notes)
	if err != nil {
		return nil, nil, nil, err
	}
	return company, keys, resolver, nil
}

// llmEntries is the providers.llm entries a subcommand works on, in the
// order the document wrote them: every one for `list`, and for a `doctor`
// that names none; otherwise the entry it names, or the only one there is.
func llmEntries(company *config.Company, sub, key, companyPath string) ([]string, error) {
	order := company.Providers.ProviderOrder()
	sorted := slices.Sorted(slices.Values(order))
	switch {
	case len(order) == 0:
		return nil, fmt.Errorf(
			"%s declares no providers.llm entries — see "+
				"docs/concepts/llm-backends.md for the config block",
			companyPath)
	case sub == "list" || (sub == "doctor" && key == ""):
		return order, nil
	case key == "" && len(order) == 1:
		return order, nil
	case key == "":
		return nil, fmt.Errorf("name a provider: %s", strings.Join(sorted, ", "))
	case !slices.Contains(order, key) && sub == "doctor":
		return nil, fmt.Errorf("no provider %q (doctor examines %s)", key, strings.Join(sorted, ", "))
	case !slices.Contains(order, key):
		return nil, fmt.Errorf("no provider %q (have %s)", key, strings.Join(sorted, ", "))
	}
	return []string{key}, nil
}

// llmEntryReads is every stored value these entries read, each once, sorted:
// building one resolves every ${VAR} it names, and the variables it signs in
// from without naming them ([config.Company.LLMEntryReads]) — the names a
// running node is asked to reveal, where it lists them
// ([companyStore.resolver]).
//
// LESS ANY NAME NO STORE CAN HOLD. An overrides.token_env or api_key_env
// outside the reference grammar — MY-TOKEN — is a variable the entry signs in
// from, and one this command, which loads its file only to run, still meets:
// every write of the document refuses it, but that is an admission rule. No
// store can hold the name, so it is no stored value: the run's batch never
// asks for it — a node refuses a whole request over one such name, and the
// fleet's store reached directly a read naming one. The engine reads it, if
// at all, from its own environment, which the resolver still consults.
func llmEntryReads(company *config.Company, keys []string) []string {
	var names []string
	for _, key := range keys {
		for _, name := range company.LLMEntryReads(key) {
			if envref.ValidName(name) {
				names = append(names, name)
			}
		}
	}
	return unionReads(names)
}

// cliAgents builds these entries' providers, in the order given.
func cliAgents(company *config.Company, keys []string, resolver *config.Resolver) ([]cliAgentProvider, error) {
	out := make([]cliAgentProvider, 0, len(keys))
	for _, key := range keys {
		built, err := engine.BuildCLIAgent(key, company.Providers.LLM[key], resolver)
		if err != nil {
			return nil, err
		}
		out = append(out, cliAgentProvider{key: key, provider: built})
	}
	return out, nil
}

// oneProvider picks the provider a key names, or explains the choice.
func oneProvider(providers []cliAgentProvider, key string) (*cliagent.Provider, error) {
	if key == "" {
		if len(providers) == 1 {
			return providers[0].provider, nil
		}
		return nil, fmt.Errorf("name a provider: %s", strings.Join(providerKeys(providers), ", "))
	}
	for _, p := range providers {
		if p.key == key {
			return p.provider, nil
		}
	}
	return nil, fmt.Errorf("no provider %q (have %s)",
		key, strings.Join(providerKeys(providers), ", "))
}

// restoreConfiguredBundle makes the restore the engine makes before it runs an
// entry ([cliagent.Provider.RestoreConfiguredBundle]), for a command that
// answers about the login the engine would run with, and says on stderr why
// it restored nothing where it did not: the engine warns and runs the entry
// signed out, and the command then reports a login that is not there, which
// this line explains — a bundle that failed, or a logout that keeps it out
// until a login is written again.
func restoreConfiguredBundle(ctx context.Context, p *cliagent.Provider, stderr io.Writer) {
	err := p.RestoreConfiguredBundle(ctx)
	switch {
	case err == nil:
	case errors.Is(err, cliagent.ErrLoggedOut):
		fmt.Fprintf(stderr, "crewlet: %s: %v\n", p.Key(), err)
	default:
		fmt.Fprintf(stderr, "crewlet: %s: the entry's configured credential bundle did not restore: %v\n",
			p.Key(), err)
	}
}

func providerKeys(providers []cliAgentProvider) []string {
	keys := make([]string, 0, len(providers))
	for _, p := range providers {
		keys = append(keys, p.key)
	}
	slices.Sort(keys)
	return keys
}

// listLLMProviders is `crewlet llm list`: each entry's CLI, model, state
// directory and how it is signed in.
//
// Each entry's configured bundle is RESTORED FIRST, as `doctor` restores it
// before it diagnoses: the SIGN-IN column is the first route doctor names, and
// read before the restore it said `none` for a fresh container that doctor —
// and the engine — would sign in from its bundle. A restore that fails, or a
// logout that keeps the bundle out, is said on stderr, as `status` says it.
func listLLMProviders(ctx context.Context, providers []cliAgentProvider, stdout, stderr io.Writer) error {
	for _, p := range providers {
		restoreConfiguredBundle(ctx, p.provider, stderr)
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tAGENT\tMODEL\tSIGN-IN\tSTATE DIR")
	for _, p := range providers {
		model := p.provider.Model()
		if model == "" {
			model = "(the CLI's default)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			p.key, p.provider.Agent(), model,
			p.provider.SignInState(ctx), p.provider.Workspace().StateDir())
	}
	return w.Flush()
}

// diagnosis is what doctor prints about one entry: the CLI's own report, or
// why there is none. Declared here, by the one caller that needs those
// reports to look alike.
type diagnosis interface {
	Render(w io.Writer)
	Healthy() bool
}

// doctorEntry is one entry doctor examines.
type doctorEntry struct {
	key string
	// diagnose measures the entry; smoke sends the billed calls.
	diagnose func(ctx context.Context, smoke bool) diagnosis
}

// doctorEntries is these entries, in the order given.
//
// AN ENTRY THAT DOES NOT BUILD IS A REPORT, not an abort. The engine would
// refuse to apply this document over it, which is precisely what an operator
// running doctor needs to read — beside every other entry's report, rather
// than instead of it.
func doctorEntries(company *config.Company, keys []string, resolver *config.Resolver) []doctorEntry {
	out := make([]doctorEntry, 0, len(keys))
	for _, key := range keys {
		entry := doctorEntry{key: key}
		built, err := engine.BuildCLIAgent(key, company.Providers.LLM[key], resolver)
		if err != nil {
			entry.diagnose = unbuilt(key, err)
			out = append(out, entry)
			continue
		}
		entry.diagnose = func(ctx context.Context, smoke bool) diagnosis {
			// A logout that keeps the bundle out is no failure of the
			// bundle: the diagnosis names it beside the missing sign-in,
			// as it would for the entry the engine runs.
			if err := built.RestoreConfiguredBundle(ctx); err != nil &&
				!errors.Is(err, cliagent.ErrLoggedOut) {
				return bundleFailure{key: key, err: err}
			}
			return built.Diagnose(ctx, cliagent.DiagnoseOptions{
				Smoke: smoke,
				// THE COMPANY'S OWN ANSWER to which seats launch their
				// executors on this entry, resolved as validation and the
				// launch resolve it: an entry no seat's executor chain
				// holds serves the phases on this host only, and its report says
				// nothing about runners or a bridge it never needs.
				ExecutorSeats: company.ExecutorSeatsOn(key),
				// THE ENGINE'S OWN ANSWERS, not the provider's guess at
				// them: which runners this build registers and what a
				// box can dial are facts about the process, and both
				// decide whether an executor launches on the entry at all.
				AgentRunners: codingagent.Names(),
				Cell:         string(company.EntryCell(key)),
				BridgeURL:    os.Getenv(mcpbridge.BaseURLVar),
			})
		}
		out = append(out, entry)
	}
	return out
}

// buildFailure is the report for an entry that did not build.
type buildFailure struct {
	key string
	err error
}

// bundleFailure is an entry whose configured credential bundle did not
// restore: the engine warns and runs it signed out, so there is no login to
// diagnose, and the bundle is what to fix.
type bundleFailure struct {
	key string
	err error
}

func (b bundleFailure) Healthy() bool { return false }

func (b bundleFailure) Render(w io.Writer) {
	fmt.Fprintf(w, "%-14s: %s\n", "provider", b.key)
	fmt.Fprintln(w, "problems:")
	// THE ENTRY'S KEY, which is what `crewlet llm export` takes: a bare one
	// is refused wherever the company has more than one entry, so the remedy
	// printed under one of them was a command that asks which.
	fmt.Fprintf(w, "  - its configured credential bundle did not restore, so the engine would run it "+
		"signed out — export the bundle again (`crewlet llm export %s`): %v\n", b.key, b.err)
}

func unbuilt(key string, err error) func(context.Context, bool) diagnosis {
	return func(context.Context, bool) diagnosis { return buildFailure{key: key, err: err} }
}

func (b buildFailure) Healthy() bool { return false }

func (b buildFailure) Render(w io.Writer) {
	fmt.Fprintf(w, "%-14s: %s\n", "provider", b.key)
	fmt.Fprintln(w, "problems:")
	fmt.Fprintf(w, "  - it does not build, so the engine would refuse this document: %v\n", b.err)
}

func doctorLLM(ctx context.Context, entries []doctorEntry, smoke bool, stdout io.Writer) error {
	unhealthy := 0
	for i, e := range entries {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		d := e.diagnose(ctx, smoke)
		d.Render(stdout)
		if !d.Healthy() {
			unhealthy++
		}
	}
	if unhealthy > 0 {
		// A non-zero exit, because `doctor` is what a deploy script runs
		// before it lets a node take seats — a green-looking command that
		// exited 0 with problems printed is one nobody gates on.
		return fmt.Errorf("%d of %d providers have problems", unhealthy, len(entries))
	}
	return nil
}

// loginRequest is what `crewlet llm login` was asked to do.
type loginRequest struct {
	providers     []cliAgentProvider
	key           string
	home          string
	fromHost      bool
	captureToken  bool
	tokenStdin    bool
	username      string
	passwordStdin bool
	printToken    bool

	// companySecrets is the run's store, which a captured token is written
	// into: the one the entry's values were read through.
	companySecrets *companyStore
	// companyPath is the company document the entry was read from, which
	// the doctor a broker login ends by naming reads too ([verifyWithDoctor]).
	companyPath string
	// stdin is what a token, a password or the vendor's own login prompts
	// are read from — os.Stdin, and the suite's own reader in a test.
	stdin io.Reader
}

func loginLLM(ctx context.Context, req loginRequest, stdout, stderr io.Writer) error {
	p, err := oneProvider(req.providers, req.key)
	if err != nil {
		return err
	}
	key := req.key
	if key == "" {
		key = req.providers[0].key
	}

	// The modes are mutually exclusive: each writes the credential to a
	// different place, and running two would leave an operator unsure
	// which one the engine reads.
	chosen := 0
	for _, on := range []bool{req.fromHost, req.captureToken, req.tokenStdin, req.username != ""} {
		if on {
			chosen++
		}
	}
	if chosen > 1 {
		return errors.New("choose one of -from-host, -capture-token, -token-stdin or -username")
	}

	switch {
	case req.fromHost:
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		taken, err := p.AdoptHostLogin(ctx, req.home)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Adopted %s into %s.\n",
			strings.Join(taken, ", "), p.Workspace().CredentialsDir())
		// Said every time, because it is the one cost of this route and
		// it only bites later: both copies now descend from one refresh
		// token, and a vendor that rotates them logs out whichever side
		// refreshes second.
		fmt.Fprintln(stdout,
			"\nThis is a COPY: your own login is untouched, but both copies now share\n"+
				"one refresh token, and a vendor that rotates them can log out whichever\n"+
				"side refreshes second.")
		// ONLY WHERE THE ENTRY READS A STORED TOKEN AND THE CLI CAN MINT ONE
		// THERE: under api-key or inherit-env the token this would
		// recommend reaches nothing ([refuseUnreadToken]); a CLI with no
		// minting command has no -capture-token to run; and under a
		// token_env outside the reference grammar the command refuses
		// before it mints ([cliagent.Provider.MintsHeadlessToken]).
		if _, fromEnv := p.ModeSignIn(); fromEnv {
			return nil
		}
		if p.MintsHeadlessToken() {
			fmt.Fprintf(stdout,
				"Prefer `crewlet llm login %s -capture-token`, which mints a headless\n"+
					"%s and avoids the shared refresh token entirely.\n", key, p.Profile().TokenEnv)
		}
		return nil

	case req.captureToken, req.tokenStdin:
		if err = refuseUnreadToken(p, key, req); err != nil {
			return err
		}
		var token string
		if req.tokenStdin {
			token, err = tokenFromReader(req.stdin)
			if err != nil {
				return err
			}
		} else {
			token, err = p.CaptureToken(ctx, req.stdin, stderr)
			if err != nil {
				return err
			}
		}
		// Named, and in the grammar: refuseUnreadToken refused any other.
		tokenVar := p.Profile().TokenEnv
		if req.printToken {
			// TO STDOUT, STORING NOTHING. An operator whose secrets live
			// in somebody else's manager should not have to write the
			// token into Crewlet's store on the way past — which is
			// exactly what the two-step alternative (-capture-token then
			// `secrets get -reveal`) does, leaving a copy behind in a
			// revision history that keeps what a later write deletes.
			//
			// REFUSED ON A TERMINAL: this is a credential, and printing
			// one into a scrollback that a screen-share or a shell
			// history will outlive is the accident the flag exists to
			// enable, not to cause. Redirect it or pipe it.
			if isTerminal(stdout) {
				return errors.New(
					"-print-token writes a credential to stdout and refuses to " +
						"do it on a terminal: pipe it into your secret manager, " +
						"or redirect it to a file you then remove")
			}
			fmt.Fprintln(stdout, token)
			if sign, fromEnv := p.ModeSignIn(); fromEnv && sign.Mode == cliagent.AuthInheritEnv {
				// Reached only under inherit-env: api-key was refused
				// above. That mode reads no auth.token, which validation
				// refuses beside it, so the reference would be one more
				// thing to undo.
				fmt.Fprintf(stderr,
					"Wrote the headless token to stdout and stored NOTHING. Export it as %s "+
						"in the engine's environment, which auth.mode inherit-env forwards.\n", tokenVar)
				return nil
			}
			fmt.Fprintf(stderr,
				"Wrote the headless token to stdout and stored NOTHING. "+
					"Reference it as ${%s}.\n", tokenVar)
			return nil
		}
		target, err := storeLLMSecret(ctx, req.companySecrets, tokenVar, token)
		if err != nil {
			return err
		}
		// WHEN A RUNNING FLEET READS IT, said with where it went: the
		// engine resolves the token from the snapshot it builds at each
		// apply, so a token written through a node signs nothing in until
		// one ([secretTarget.writtenNote]). And that the name is the one
		// the entry reads with no reference, since one is not needed.
		fmt.Fprintf(stdout,
			"Stored the headless token as %s in %s.\n%s\n"+
				"It is read under that name by convention; to name it in the "+
				"company document instead:\n\n%s",
			tokenVar, target.where, target.writtenNote(), authReference(key, "token", tokenVar))
		return nil

	case req.username != "":
		if !req.passwordStdin {
			return errors.New("-username requires -password-stdin: a password on argv is " +
				"visible in ps and lands in shell history")
		}
		password, err := readPipedPassword(req.stdin)
		if err != nil {
			return fmt.Errorf("reading the password from stdin: %w", err)
		}
		if password == "" {
			return errors.New("stdin held no password")
		}
		if err := p.CredentialLogin(ctx, req.username, password, stdout, stderr); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Logged %s in as %s.\n", key, req.username)
		return nil

	default:
		if err := p.BrokerLogin(ctx, req.stdin, stdout, stderr); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nLogin written to %s.\n", p.Workspace().CredentialsDir())
		fmt.Fprintln(stdout, verifyWithDoctor(req.companySecrets, req.companyPath, key))
		return nil
	}
}

// refuseUnreadToken refuses `-capture-token` and `-token-stdin` for an entry
// that would never read the token, BEFORE one is minted, read or stored.
//
// THE MODE DECIDES WHAT THE ENTRY READS, and the command used to ask only the
// CLI. Under api-key the token variable is removed from the CLI's environment
// and given no fallback, so a token reaches nothing wherever it is put; under
// inherit-env the variable is forwarded from the engine's own process
// environment and the secret store is never read, so a STORED token reaches
// nothing, while one written to stdout with -print-token does, once exported
// there. Each was minted and stored all the same, and the `auth.token` snippet
// printed after it is one validation refuses under either mode — so an
// operator who followed it was refused, and one who did not was signed in to
// nothing, which a remote run's refusal and the doctor then reported again.
//
// The error says what the mode reads instead, in the words the doctor and the
// run's refusal use ([cliagent.Provider.ModeSignIn]).
//
// NO TOKEN VARIABLE, OR ONE NO STORE CAN HOLD, is refused before anything
// else but api-key, whatever the mode and -print-token, since a token reaches
// the CLI only through that variable. A CLI whose profile names none — every
// built-in one but claude-code — takes no headless token at all, and it was
// refused only once the token was in hand: after -token-stdin had read it, or
// after a minting command a custom profile declared had run. A name outside
// the reference grammar, which only overrides.token_env can give, is refused
// by the store, so a token minted for it was lost at the write, after the
// vendor had issued it; and -print-token's next step, a ${VAR} or an export,
// cannot spell it either. Every write of the company document refuses such an
// override, but that is an admission rule and this command loads its file
// only to run, so the name reaches here.
//
// API-KEY IS JUDGED FIRST, because the mode decides what the entry reads before
// the CLI does ([cliagent.Provider.ModeSignIn]): an api-key entry reads its key
// and no token under any variable, so it is told its key, as the doctor tells
// it, whatever the profile names. Judged after them, an api-key codex entry
// would be told to sign in with a login, which api-key does not sign in with,
// or to name a token variable the mode would remove; and an api-key MY-TOKEN
// entry to name the variable its CLI reads, as if a token would then reach
// it. Its refusal offers subscription as the mode that reads a stored token
// only under a variable a token can be stored in, and names the field where the
// name is outside the grammar.
func refuseUnreadToken(p *cliagent.Provider, key string, req loginRequest) error {
	tokenVar := p.Profile().TokenEnv
	sign, fromEnv := p.ModeSignIn()
	if fromEnv && sign.Mode == cliagent.AuthAPIKey {
		return refuseAPIKeyToken(p, key, sign)
	}
	switch {
	case tokenVar == "":
		return fmt.Errorf(
			"the %q profile names no token_env, so this CLI takes no headless token, and none "+
				"was minted, read or stored: sign %s in with a login of its own (`crewlet llm "+
				"doctor %s` names the routes it has), or, if your build of the CLI reads a "+
				"token from a variable, name that variable in providers.llm.%s.overrides.token_env",
			p.Agent(), key, key, key)
	case !p.TokenCanBeStored():
		return fmt.Errorf(
			"providers.llm.%s.overrides.token_env is %q, which is not an environment variable "+
				"name (letters, digits and underscores, not starting with a digit): no secret "+
				"store or ${VAR} can hold a token under it and no shell can export one, so none "+
				"was minted, read or stored. Name the variable your build of the CLI reads",
			key, tokenVar)
	}
	if !fromEnv {
		return nil
	}
	if req.printToken {
		return nil
	}
	instead := fmt.Sprintf("export the token you have as %s in the engine's environment", tokenVar)
	if req.captureToken {
		instead = fmt.Sprintf("mint it to stdout, storing nothing, with `crewlet llm login %s "+
			"-capture-token -print-token`, and export it as %s in the engine's environment",
			key, tokenVar)
	}
	return fmt.Errorf(
		"%s runs under auth.mode inherit-env, which %s, so a stored token would reach nothing, "+
			"and none was minted or stored. Instead, %s — or set auth.mode: subscription, "+
			"which reads a stored token",
		key, sign.Reads, instead)
}

// refuseAPIKeyToken is [refuseUnreadToken]'s refusal for an api-key entry: the
// mode removes the token variable from the CLI's environment, so a token
// reaches nothing under any variable, and the remedy is the key the mode reads
// — or, on a CLI whose profile names no key variable, a pairing every write of
// the configuration refuses, the mode's own remedy, a login under subscription
// ([cliagent.ModeSignIn]).
//
// Subscription is offered as the mode that reads a token only where it could
// read one: a CLI whose profile names no token variable, or one outside the
// reference grammar, takes none under any mode, so offering it sent an
// operator to the refusal that names the variable instead.
func refuseAPIKeyToken(p *cliagent.Provider, key string, sign cliagent.ModeSignIn) error {
	tokenVar := p.Profile().TokenEnv
	switch {
	case tokenVar == "":
		return fmt.Errorf(
			"%s runs under auth.mode api-key, which hands the CLI no headless token — and the %q "+
				"profile names no token_env to hand one in — so a token would reach nothing, and "+
				"none was minted or stored. The mode %s: %s",
			key, p.Agent(), sign.Reads, sign.Supply)
	case !p.TokenCanBeStored():
		return fmt.Errorf(
			"%s runs under auth.mode api-key, which removes %q from the CLI's environment, so a "+
				"headless token would reach nothing, and none was minted or stored. The mode %s: "+
				"%s. And providers.llm.%s.overrides.token_env is %q, which is not an environment "+
				"variable name (letters, digits and underscores, not starting with a digit), so "+
				"no secret store or ${VAR} could hold a token under it in any mode either: name "+
				"the variable your build of the CLI reads",
			key, tokenVar, sign.Reads, sign.Supply, key, tokenVar)
	}
	return fmt.Errorf(
		"%s runs under auth.mode api-key, which removes %s from the CLI's environment, so a "+
			"headless token would reach nothing, and none was minted or stored. The mode %s: "+
			"%s — or set auth.mode: subscription to sign it in with a token",
		key, tokenVar, sign.Reads, sign.Supply)
}

// tokenFromReader reads the headless token piped to `-token-stdin`: trimmed of
// the whitespace around it, refused when there is none, and held to the size
// every store keeps a value to — before the rest of it is read, like every
// value piped to be stored ([readPipedValue]).
func tokenFromReader(r io.Reader) (string, error) {
	raw, err := readPipedValue(r)
	if err != nil {
		return "", fmt.Errorf("reading the token from stdin: %w", err)
	}
	token := strings.TrimSpace(raw)
	if token == "" {
		return "", errors.New("stdin held no token")
	}
	if err := secrets.CheckValue(token); err != nil {
		return "", fmt.Errorf("the token on stdin: %w", err)
	}
	return token, nil
}

func exportLLM(ctx context.Context, providers []cliAgentProvider, key string, toStore bool,
	companySecrets *companyStore, stdout io.Writer) error {
	p, err := oneProvider(providers, key)
	if err != nil {
		return err
	}
	if key == "" {
		key = providers[0].key
	}
	if err = p.RestoreConfiguredBundle(ctx); errors.Is(err, cliagent.ErrLoggedOut) {
		return fmt.Errorf("%s has no login to export: %w", key, err)
	} else if err != nil {
		return fmt.Errorf("the entry's configured credential bundle did not restore, so there is "+
			"no login the engine would run with to export: %w", err)
	}
	bundle, err := p.ExportBundle(ctx)
	if err != nil {
		return err
	}
	if !toStore {
		// To stdout, so an operator can pipe it into their own secret
		// manager. It is a CREDENTIAL — the caller asked for it in the
		// clear, and the reminder is the honest thing to print beside it.
		fmt.Fprintln(stdout, bundle)
		return nil
	}
	// A BUNDLE NO STORE KEEPS IS REFUSED HERE, IN A BUNDLE'S WORDS, before
	// anything is written. Every store holds a value to
	// [secrets.MaxValueBytes], and refuses a longer one as most likely the
	// wrong input, because a credential is a few kilobytes. A bundle is not
	// a pasted credential: it is this entry's whole credentials directory,
	// packed as it is, and a CLI whose directory packs to more than the
	// store holds is not fixed by other input. What works is a route that
	// does not go through the store: the bundle on stdout, kept wherever the
	// operator keeps such things, or a login on each host. This caps the
	// bundle as the store keeps it, packed; a restore holds what it unpacks
	// to cliagent's own bound, which measures different bytes, so a stored
	// bundle must pass both.
	if len(bundle) > secrets.MaxValueBytes {
		return fmt.Errorf("%w: %s's credential bundle is %d bytes once packed, and the store "+
			"keeps a value of at most %d (%d KiB), so nothing was stored. Export it to stdout "+
			"instead (`crewlet llm export %s`, without -secret-store), keep it in your own "+
			"secret manager and pipe it to `crewlet llm import %s` on each host — or sign %s "+
			"in on each host with `crewlet llm login %s`",
			secrets.ErrValueTooLarge, key, len(bundle), secrets.MaxValueBytes,
			secrets.MaxValueBytes>>10, key, key, key, key)
	}
	name := cliprofile.BundleVarName(key)
	target, err := storeLLMSecret(ctx, companySecrets, name, bundle)
	if err != nil {
		return err
	}
	// WHERE IT WENT AND WHEN IT IS READ, said as `crewlet secrets set` says
	// them. The store is the FLEET's: the store's rows are the company's, on
	// the coordination KV every node reads, and a write reaches them there
	// whichever way it went, so this is run once, not once per node — and a
	// node restores it only when it BUILDS A COMPANY TO RUN, at its boot and
	// at every apply (the engine's restoreBundles), never by watching its
	// directory, so a running fleet reads it at its next apply.
	fmt.Fprintf(stdout,
		"Stored the credential bundle as %s in %s.\n%s\n"+
			"A node restores it when it builds a company to run, at boot and at every\n"+
			"apply, if this entry's credentials directory is empty and the entry was\n"+
			"not logged out there since its last login. It is read under that name by\n"+
			"convention; to name it in the company document instead:\n\n%s",
		name, target.where, target.writtenNote(), authReference(key, "credential_bundle", name))
	return nil
}

// authReference is the company-document snippet that points an entry's auth
// field at a stored secret, for the line printed after one is written.
func authReference(key, field, name string) string {
	return fmt.Sprintf("  providers:\n    llm:\n      %s:\n        auth:\n"+
		"          %s: \"${%s}\"\n", key, field, name)
}

// storeLLMSecret writes one value into the encrypted secret store the run read
// its company's values from — through the running node, or directly with this
// node's engine stopped — as [secrets.SourceLLMLogin] where the write keeps
// the word (made directly; the node's route records its own), and answers the
// store it went to, which says where that is, said after every write for the
// reason [secretTarget.where] gives, and who reads it when
// ([secretTarget.writtenNote]).
func storeLLMSecret(ctx context.Context, companySecrets *companyStore, name, value string) (*secretTarget, error) {
	target, err := companySecrets.route(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the secret store to save %s: %w", name, err)
	}
	if target == nil {
		return nil, fmt.Errorf("cannot save %s: there is no Tier A file at %s naming "+
			"this node's store and keyring. Point -config at it, or pass -api URL "+
			"naming a running node", name, companySecrets.bootstrapPath)
	}
	if _, err := target.Set(ctx, name, value, hostAuthor(), secrets.SourceLLMLogin,
		time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("writing %s: %w", name, err)
	}
	return target, nil
}

// importLLM restores an exported credential bundle onto this host.
//
// # The other half of export
//
// `crewlet llm export KEY` writes a blob to stdout and, without this, nothing
// read it back: the documented way to "move a login onto another host" ended
// at a string in a terminal. The `-secret-store` half reaches a second host
// wherever the two are nodes of one company — the store is the fleet's — and
// not where they are two unrelated laptops.
//
// FROM STDIN, never from a path argument: a credential bundle on argv is
// visible in `ps` and lands in shell history, and the natural spelling —
// `crewlet llm export k | ssh host crewlet llm import k` — is a pipe anyway.
//
// It REFUSES TO OVERWRITE an existing login, which is [Provider.RestoreBundle]'s
// own rule: a host that has been running holds the fresher refresh token, and
// restoring a blob over it is how a fleet logs itself out. That is reported
// here rather than swallowed, because "nothing happened" and "restored" look
// identical from the outside and only one of them means the host is ready —
// asked before stdin is read, so a refusal costs nothing, and answered again by
// [cliagent.Provider.ImportBundle] under the lock its write takes, because a
// login landing between the two was otherwise reported as a bundle restored.
//
// companyPath is the company document the entry was read from and
// companySecrets the run's store, which say how the doctor that checks the
// restored login can be run ([verifyWithDoctor]).
func importLLM(ctx context.Context, providers []cliAgentProvider, key, companyPath string,
	companySecrets *companyStore, stdin io.Reader, stdout io.Writer) error {
	p, err := oneProvider(providers, key)
	if err != nil {
		return err
	}
	if key == "" {
		key = providers[0].key
	}
	present := func() error {
		return fmt.Errorf(
			"%s already has a login in %s, and restoring over it would replace "+
				"a fresher refresh token with an older one — log it out first "+
				"(`crewlet llm logout %s`) if you mean to replace it",
			key, p.Workspace().CredentialsDir(), key)
	}
	has, err := p.Workspace().HasLogin(ctx)
	if err != nil {
		return err
	}
	if has {
		return present()
	}
	// +1 SO THE OVERRUN IS VISIBLE. io.LimitReader stops at the cap and
	// reports a clean EOF, so reading exactly maxBundleBytes cannot be told
	// from a bundle that was clipped there — and a clipped bundle is a
	// truncated tar that RestoreBundle would unpack as far as it goes. The
	// constant's own doc says this "refuses to buffer" an oversized pipe;
	// reading one extra byte is what makes that true.
	raw, err := io.ReadAll(io.LimitReader(stdin, maxBundleBytes+1))
	if err != nil {
		return fmt.Errorf("reading the bundle from stdin: %w", err)
	}
	if len(raw) > maxBundleBytes {
		return fmt.Errorf(
			"the bundle on stdin exceeds %d bytes, which is far larger than any "+
				"real credential directory — check what is on the other side of "+
				"the pipe (`crewlet llm export %s` produces one)",
			maxBundleBytes, key)
	}
	blob := strings.TrimSpace(string(raw))
	if blob == "" {
		return errors.New(
			"stdin held no bundle: pipe one in, e.g. " +
				"`crewlet llm export " + key + " | ssh host crewlet llm import " + key + "`")
	}
	if err := p.ImportBundle(ctx, blob); errors.Is(err, cliagent.ErrLoginPresent) {
		return present()
	} else if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restored %s into %s.\n%s\n",
		key, p.Workspace().CredentialsDir(), verifyWithDoctor(companySecrets, companyPath, key))
	return nil
}

// verifyWithDoctor is the line a broker login (one with none of -from-host,
// -capture-token, -token-stdin and -username) or an import ends on: the doctor
// that checks the login end to end, as it can be run from where the command
// ran.
//
// WITH THE RUN'S OWN FLAGS ([doctorCommand]). The doctor reads the company
// document and the store the run did, and a bare one reads the defaults: after
// a login through -api it would route to this machine's Tier A, which may be
// another node's, or another store altogether. companySecrets is the run's
// store, or nil where there is none to ask, and companyPath the company
// document the run read.
func verifyWithDoctor(companySecrets *companyStore, companyPath, key string) string {
	return fmt.Sprintf("Verify it with `%s`.", doctorCommand(companySecrets, companyPath, key))
}

// doctorCommand is `crewlet llm doctor KEY` with each of -company, -config and
// -api the run was given, where it is not the default: the flags that decide
// which company document the doctor reads and which store it reads it through.
func doctorCommand(companySecrets *companyStore, companyPath, key string) string {
	args := []string{"crewlet", "llm", "doctor", key}
	if companyPath != defaultCompanyPath {
		args = append(args, "-company", companyPath)
	}
	if companySecrets != nil {
		if companySecrets.bootstrapPath != defaultBootstrapPath {
			args = append(args, "-config", companySecrets.bootstrapPath)
		}
		if companySecrets.apiURL != "" {
			args = append(args, "-api", companySecrets.apiURL)
		}
	}
	return strings.Join(args, " ")
}

// maxBundleBytes bounds what import will read.
//
// A credential directory is a handful of small JSON files; 8 MiB is orders of
// magnitude above any real one, and a pipe carrying more than that is REFUSED
// rather than read to the cap — a clipped bundle is a truncated tar, and
// unpacking one as far as it goes writes a partial credential directory that
// looks like a finished login.
const maxBundleBytes = 8 << 20

// isTerminal reports whether a writer is a character device.
//
// Used by exactly one caller, to refuse printing a credential into a
// scrollback. A conservative answer: anything this cannot inspect is treated
// as NOT a terminal, because the alternative refuses a legitimate pipe.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
