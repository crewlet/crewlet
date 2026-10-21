package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provision"
)

// The company's secret store, as one CLI run reads and writes it.
//
// # One route for both sides of a run
//
// A provisioning command READS the company's own values — the instance URL,
// the webhook signing secret, every seat's token — and RECORDS what it mints.
// Both are the fleet's secret store, the one the engine resolves every
// `${VAR}` through, so both go the way [routeSecretStore] decides once per
// run: the running node's API while the engine holds this node's store, or
// `-api` names a node; this node's own table while it is stopped.
//
// It used to be two. The sink routed, and the resolver opened this node's
// table by itself: against a running node every one of these commands died on
// the store lock before it planned anything, `-dry-run` included; with `-api`
// the resolver ignored the flag and created an empty database on a machine
// that was not the node; and on a stopped node that had booted it read a table
// the engine empties onto the fleet at every start, so every value the fleet
// held resolved as nothing — which for a webhook signing secret is the signal
// to MINT, re-pointing a working hook at a key no node holds.
//
// # Through a node: exactly what the document reads, in one question
//
// The run asks the node for the values of the variables its document reads
// ([config.Company.VariablesRead]) on the node's break-glass read,
// `POST /secrets/reveal` — decided on `secrets.reveal`, so admin, a person
// present and a recent proof, and logged by name — rather than for the store,
// and rather than once per name. A read the node refuses, or a node that does
// not answer, FAILS THE RUN: nothing is resolved from the environment in its
// place, because a value missing for that reason is not a value the fleet
// does not hold, and the environment's answer would be the stale export the
// store exists to outrank.
//
// # Stopped: a miss is unknown, never absent
//
// This node's own table is the whole store only on a node that has never
// started — every start moves its rows onto the fleet and deletes them here —
// and nothing on this disk says whether a fleet exists: a node joining one has
// never started either, and an external broker keeps its state on no disk of
// this node's. So on this route a variable the table does not hold is
// UNKNOWN, the third answer of every read in this engine — WHATEVER THIS SHELL
// EXPORTS FOR IT: the environment still fills the resolver in behind the rows,
// as the engine's own environment does behind the store, but the fleet's
// store outranks it there, so what this shell exports for a name the rows
// lack is a guess at the deployment's value rather than the value. Every
// decision that would act on such an answer as the deployment's is refused
// rather than taken: a mint into a variable the document resolved as empty,
// registering what this shell exported for one with a vendor
// ([companyValues.Unvouched], which every pass that mints or registers a
// secret hands [provision.Resolved] and [provision.MintSecret]), and a seat
// credential `-secret-store` reads back to decide whether to keep it
// ([sinkStore]). A decision that never read the store — a seat's token under
// `-print`, which holds nothing, or `-env-file`, which holds what its file
// holds — is the same on this route as on any other, so a seat those sinks
// lack is minted a token here exactly as it would be through a node; and so is
// one that reads no answer at all — a seat whose account the run creates, and
// every decision the command's own flag for a fresh value takes. The run says
// which store it read on its first line, which of its decisions that refuses
// or that its flag for a fresh value takes regardless
// ([companyStore.unknownRefuses]), and how to reach the fleet's.
//
// With no Tier A file at `-config` and no `-api`, there is no store to read at
// all: the run resolves from the environment alone and says so, which is how
// these commands run on a workstation that is not a node.

// storeFlags is the `-config` / `-api` pair every command that reads the
// company's values takes.
type storeFlags struct {
	bootstrap *string
	api       *string

	// decides is what a command recording through a sink ([addSinkFlags])
	// decides on a variable's value, which is what its note on a stopped
	// node has to describe ([companyStore.unknownRefuses]); nothing for a
	// command with no sink.
	decides sinkDecisions
}

// sinkDecisions is what a provisioning command decides on the value its
// document resolves for a variable: the decisions an unknown answer on a
// stopped node's own rows refuses, or leaves to the sink — and so what the
// run's note there says ([companyStore.unknownRefuses]).
//
// DECLARED BY EACH COMMAND, never implied by its having a sink, because the
// commands decide different things: GitHub, Jira and Confluence register
// webhooks and mint no seat a token, Mattermost mints its bots' tokens and has
// no webhook secret, and GitLab does both. Implied by the sink, every note
// recited both, so a GitHub run on a stopped node said -print would mint a
// token for a seat whose token it lacked — a decision that run never takes.
type sinkDecisions struct {
	// secrets: the run mints a webhook or signing secret into a variable
	// the document resolved as empty, and registers the value it resolved
	// with the vendor ([provision.Resolved], [provision.MintSecret]).
	secrets bool

	// seatTokens: the run mints a seat its own token where the sink holds
	// none for it, retiring the one the seat had — and a seat whose
	// account it creates its first, asking no sink, since nothing can hold
	// a token for an account that did not exist.
	seatTokens bool

	// fresh is the command's own flag for a fresh value ([addFreshFlag]).
	//
	// PASSED, IT TAKES EVERY DECISION ABOVE WITHOUT READING AN ANSWER:
	// -rotate mints every seat a token and re-mints a signing secret,
	// -recreate-webhooks and -recreate-webhook re-mint a webhook secret,
	// and -reinstall installs every app again, none of them asking the rows
	// or the shell what they hold ([provision.MintSecret] with recreate, and
	// a pass that skips its keep-or-mint check). A secret is re-minted into
	// the whole ${VAR} that names it: a literal has no variable to mint into,
	// and no flag replaces one. An unknown answer refuses nothing there, so the
	// note says the flag goes ahead — a note promising that nothing is
	// minted over a run that mints was the claim this field exists to
	// stop.
	fresh freshFlag
}

// freshFlag is a command's flag for a fresh value — what a run passes to
// replace a value whatever any store holds — by name, and the value the
// command parsed into it, which the note reads once the flags are parsed.
type freshFlag struct {
	name string
	on   *bool
}

// addFreshFlag declares a command's flag for a fresh value, named once: the
// flag the command parses and the name its note recites are the same
// declaration.
func addFreshFlag(fs *flag.FlagSet, name, usage string) freshFlag {
	return freshFlag{name: name, on: fs.Bool(name, false, usage)}
}

// asked reports whether the run passed the flag: false for a command that
// declares none.
func (f freshFlag) asked() bool { return f.on != nil && *f.on }

// addStoreFlags adds `-config` and `-api`.
//
// EVERY command that reads Tier B takes them, not only the ones that WRITE a
// credential: the store is where a rotated secret lives, so a command
// resolving a company's `${VAR}` without it reads an empty string for
// everything the operator has already rotated.
func addStoreFlags(fs *flag.FlagSet) storeFlags {
	return storeFlags{
		bootstrap: fs.String("config", defaultBootstrapPath,
			"Tier A config: this node's store, its API and its secret keyring"),
		// THE SAME FLAG `crewlet secrets` takes, and for the same reason:
		// a running engine holds its database, so this run goes through
		// its API — which is also where the values every node reads are.
		api: fs.String("api", "",
			"the running node to read the company's secrets through and record "+
				"minted credentials through; default is the api.host:port in "+
				"-config while its engine runs"),
	}
}

// companyStore is one run's route to the company's secret store, decided at
// most once and shared by every read and write the run makes.
type companyStore struct {
	environ       config.Source
	bootstrapPath string
	apiURL        string
	command       string

	// decides is what the command decides on a variable's value
	// ([storeFlags.decides]).
	decides sinkDecisions

	// boot is the Tier A document at bootstrapPath, nil where there is no
	// file there. Where a vendor reaches this deployment is Tier A's
	// ([webhookBase]), so a command building a webhook URL reads it from
	// here rather than loading the file a second time.
	boot *config.Bootstrap

	routed      bool
	target      *secretTarget
	closeTarget func()
	routeErr    error
}

// openCompanyStore loads the Tier A document the flags name — opening nothing
// else — for the command named, which a refusal names.
//
// A BOOTSTRAP THAT EXISTS AND CANNOT BE READ FAILS THE RUN, rather than
// sending it to the environment: someone who configured a store and did not
// get it must not have their secrets quietly resolved from a stale export.
// And Tier A refuses a file with no keyring, so one that loads always names
// the keys its store is sealed under.
func openCompanyStore(environ config.Source, flags storeFlags, command string) (*companyStore, error) {
	s := &companyStore{
		environ: environ, bootstrapPath: *flags.bootstrap,
		apiURL: strings.TrimSpace(*flags.api), command: command,
		decides: flags.decides,
	}
	switch _, err := os.Stat(s.bootstrapPath); {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	boot, err := loadBootstrapForStore(s.bootstrapPath)
	if err != nil {
		return nil, err
	}
	s.boot = boot
	return s, nil
}

// route opens the run's store, once: nil where there is none — no Tier A file
// and no `-api` — which is the environment-only run.
func (s *companyStore) route(ctx context.Context) (*secretTarget, error) {
	if s.routed {
		return s.target, s.routeErr
	}
	s.routed = true
	if s.boot == nil && s.apiURL == "" {
		return nil, nil
	}
	// EVERY RUN HERE REVEALS: the values its document reads, and a seat
	// credential its sink reads back, are the node's break-glass read.
	target, closeTarget, err := routeSecretStore(ctx, s.environ, s.boot,
		s.bootstrapPath, s.apiURL, storeRun{command: s.command, reveals: true})
	if err != nil {
		s.routeErr = err
		return nil, err
	}
	s.target, s.closeTarget = target, closeTarget
	return target, nil
}

// Close releases whatever the route opened.
func (s *companyStore) Close() {
	if s.closeTarget != nil {
		s.closeTarget()
	}
}

// unvouched answers, for a variable the run's store does not hold, why that
// absence is not one the run can act on — nil wherever the store is the
// fleet's, whose miss is a miss. On this node's own table, read while its
// engine is stopped, every miss is unknown ([unvouchedRow]).
func (s *companyStore) unvouched() provision.Unvouched {
	if s.target == nil || s.target.fleet {
		return nil
	}
	path := s.boot.Store.Path
	return func(name string) error { return unvouchedRow(name, path) }
}

// companyValues is what one run resolves the company's `${VAR}` references
// through: the store's values for the names the document reads, the
// environment behind them — the engine's own order.
type companyValues struct {
	*config.Resolver

	// Unvouched answers, for a variable this resolver answered — empty, or
	// filled in from the environment — why that answer is not one the run
	// can act on as the deployment's value: nil on every route but this
	// node's own table while its engine is stopped, where a variable the
	// rows lack is unknown whatever this shell exports for it. Handed to
	// every pass that mints into a variable or registers its value with a
	// vendor ([provision.Resolved], [provision.MintSecret]).
	Unvouched provision.Unvouched
}

// values resolves the company's references through the run's store, saying on
// notes which store that is wherever it is not the fleet's.
func (s *companyStore) values(ctx context.Context, company *config.Company,
	notes io.Writer) (*companyValues, error) {

	target, err := s.route(ctx)
	if err != nil {
		return nil, err
	}
	if target == nil {
		// A NOTE RATHER THAN SILENCE: a mistyped -config resolving
		// nothing reads every stored value as empty, and an operator has
		// to be able to see which chain ran.
		fmt.Fprintf(notes, "no %s: resolving ${VAR} from the environment only.\n",
			s.bootstrapPath)
		return &companyValues{Resolver: config.EnvOnly()}, nil
	}
	held, err := target.Values(ctx, company.VariablesRead())
	if err != nil {
		return nil, fmt.Errorf("read the company's secrets from %s: %w\n\nNothing "+
			"was resolved from the environment in their place: a value the "+
			"store holds would have been replaced by whatever this shell "+
			"exports", target.where, err)
	}
	values := &companyValues{Resolver: config.WithStore(config.MapSource(held))}
	if !target.fleet {
		path := s.boot.Store.Path
		values.Unvouched = func(name string) error {
			if _, ok := held[name]; ok {
				return nil
			}
			return unvouchedRow(name, path)
		}
		fmt.Fprintf(notes, "read this node's own secret rows (%s) only: its engine "+
			"is stopped, so the fleet's secret store, which every node resolves "+
			"${VAR} through, is out of reach, and a variable these rows do not "+
			"hold is unknown here rather than unset, whatever this shell exports "+
			"for it.%s Start `crewlet run` on this node, or pass -api URL naming "+
			"a running node, to read the fleet's.\n", path, s.unknownRefuses())
	}
	return values, nil
}

// unknownRefuses is the part of a stopped node's note that says what becomes
// of the run's decisions on an unknown answer, led by a space: nothing for a
// command that decides none of them.
//
// SCOPED TO WHAT THE CODE REFUSES, and to the decisions THIS command takes
// ([sinkDecisions]), which is narrower than "nothing is minted into one" — the
// sentence this replaced. A webhook or signing secret is refused, and so is
// registering what this shell exports, and with -secret-store the token of a
// seat whose account exists already, because the sink reads the same rows
// back ([sinkStore]). But -print and -env-file decide a seat's token from
// what they hold, on this route as on every other — so a Mattermost run into
// a fresh -env-file mints every bot a new token and revokes the one the fleet
// holds, under a line that used to say nothing would be minted. A seat whose
// account the run creates asks no sink at all: nothing can hold a token for
// an account that did not exist, so it is minted its first one whatever the
// sink. And a run that passed the command's own flag for a fresh value takes
// every decision without reading an answer ([sinkDecisions.fresh]), so its
// note says the flag goes ahead instead — for a webhook or signing secret,
// only one a whole ${VAR} names. A literal is the document's own, which these
// rows have no say in, and no flag mints over one: gitlab -rotate registers it
// as it is, with a note, and -recreate-webhooks and -recreate-webhook refuse.
// A clause promising a fresh secret for whatever the run registers sat above
// a GitLab plan reusing a literal. A sentence about a decision the
// command never takes, or one this run does not refuse, would be a claim
// about a run that does not happen, so each is said only where it holds.
func (s *companyStore) unknownRefuses() string {
	d := s.decides
	if d.fresh.asked() {
		var ahead []string
		if d.secrets {
			ahead = append(ahead, noteSecretsFresh)
		}
		if d.seatTokens {
			ahead = append(ahead, noteSeatTokensFresh)
		}
		if len(ahead) == 0 {
			return ""
		}
		return " " + freshGoesAhead(d.fresh.name) + strings.Join(ahead, "; ") + "."
	}
	var note strings.Builder
	if d.secrets {
		note.WriteString(" " + noteSecretsRefused)
	}
	if d.seatTokens {
		note.WriteString(" " + noteSeatTokensSinks)
	}
	return note.String()
}

// freshGoesAhead leads the note of a run that passed its flag for a fresh
// value, the flag named as it is typed.
func freshGoesAhead(flag string) string {
	return fmt.Sprintf(noteFreshLead, "-"+flag)
}

// The sentences [companyStore.unknownRefuses] composes a stopped node's note
// from: one per decision ([sinkDecisions]) a run takes on an unknown answer,
// and, for a run that passed its flag for a fresh value, the lead and one
// clause per decision that flag takes regardless.
const (
	noteSecretsRefused = "No webhook or signing secret is minted into one, and " +
		"nothing this shell exports for one is registered with a vendor."
	noteSeatTokensSinks = "A seat's token is its sink's to decide: with " +
		"-secret-store, a seat whose account exists already and whose token " +
		"these rows lack is handed no new one, while -print and -env-file decide " +
		"from what they hold, as they do on every route, and mint a token for a " +
		"seat whose token they lack. A seat whose account this run creates is " +
		"minted its first token whichever the sink."

	noteFreshLead = "%s is a decision rather than a reason, so it goes ahead " +
		"whatever these rows hold: "
	noteSecretsFresh = "a webhook or signing secret named by a whole ${VAR} is " +
		"minted afresh into it wherever the run registers one, and nothing this " +
		"shell exports is registered in its place"
	noteSeatTokensFresh = "every seat the run provisions is minted a fresh " +
		"token, whichever the sink"
)

// unvouchedRow is the refusal for a variable this node's own rows, at path, do
// not hold while its engine is stopped.
func unvouchedRow(name, path string) error {
	return fmt.Errorf("%s: %w: it is not in this node's own secret rows (%s), "+
		"and the fleet's store, which may hold it, is out of reach while the "+
		"engine is stopped, so this run neither mints a value into it nor "+
		"registers one this shell exports for it in its place. Start `crewlet "+
		"run` on this node and re-run, or pass -api URL naming a running node; "+
		"to replace it regardless, run with the command's own flag for a fresh "+
		"value (-rotate, -recreate-webhooks, -recreate-webhook, -reinstall)",
		name, provision.ErrUnvouched, path)
}
