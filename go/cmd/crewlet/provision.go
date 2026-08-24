package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/provision"
)

// The provisioning commands, and the flags they share.
//
// # Why the sink is a required choice
//
// A run with nowhere to put what it mints creates live credentials at the
// vendor and prints none of them — the worst outcome available, because
// every one has to be found and revoked by hand. So there is no default: the
// operator says where, up front, and a run with no answer is refused before
// it touches anything.

// sinkFlags is the shared --secret-store / --env-file / --print choice.
type sinkFlags struct {
	secretStore *bool
	envFile     *string
	print       *bool
	bootstrap   *string
}

func addSinkFlags(fs *flag.FlagSet) sinkFlags {
	return sinkFlags{
		secretStore: fs.Bool("secret-store", false,
			"record minted credentials in the encrypted secret store"),
		envFile: fs.String("env-file", "",
			"record minted credentials in this .env file"),
		print: fs.Bool("print", false,
			"print minted credentials to stdout and persist nothing"),
		bootstrap: fs.String("config", defaultBootstrapPath,
			"Tier A config: this node's store and its secret keyring"),
	}
}

// open builds the chosen sink, refusing an ambiguous or absent choice.
func (s sinkFlags) open(ctx context.Context) (provision.TokenSink, func(), error) {
	chosen := 0
	for _, on := range []bool{*s.secretStore, *s.envFile != "", *s.print} {
		if on {
			chosen++
		}
	}
	switch {
	case chosen == 0:
		return nil, nil, fmt.Errorf("%w: pass -secret-store, -env-file PATH or -print",
			provision.ErrNoSink)
	case chosen > 1:
		// REFUSED RATHER THAN ORDERED. Writing to two places doubles the
		// number of copies of a live credential, and picking one by
		// precedence would put it somewhere the operator did not ask for.
		return nil, nil, errors.New(
			"name exactly one of -secret-store, -env-file and -print")
	}

	switch {
	case *s.print:
		return provision.NewPrintSink(stdoutForPrintSink), func() {}, nil
	case *s.envFile != "":
		sink, err := provision.NewEnvFileSink(*s.envFile)
		return sink, func() {}, err
	}

	sv, closeStore, err := openSecretStore(ctx, *s.bootstrap)
	if err != nil {
		return nil, nil, err
	}
	return provision.NewSecretStoreSink(sv, currentOperator()), closeStore, nil
}

// stdoutForPrintSink is set by the command so the sink writes where the
// command was told to write.
var stdoutForPrintSink io.Writer

// runGitLabProvision is `crewlet gitlab provision`.
func runGitLabProvision(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("gitlab provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	adminToken := fs.String("admin-token", "",
		"a GitLab token permitted to create service accounts; empty reads GITLAB_ADMIN_TOKEN")
	publicURL := fs.String("public-url", "",
		"this deployment's public base URL, for registering the webhook")
	dryRun := fs.Bool("dry-run", false,
		"print what the run would do and touch nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tail := fs.Args()
	if companyPath == "" && len(tail) == 1 {
		companyPath, tail = tail[0], nil
	}
	if companyPath == "" || len(tail) > 0 {
		fmt.Fprintln(stderr,
			"usage: crewlet gitlab provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-dry-run]")
		return errors.New("name exactly one company document")
	}

	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return err
	}
	organization, err := company.Organization()
	if err != nil {
		return err
	}
	cfg := company.Integrations.GitLab
	plan, err := gitlab.PlanFor(organization, cfg)
	if err != nil {
		return err
	}

	// THE PLAN IS PRINTED EITHER WAY, and it is the SAME plan the run
	// uses. A --dry-run that re-derived it separately would be a second
	// implementation that can disagree with the real one about what it
	// was going to do.
	printPlan(stdout, plan)
	if *dryRun {
		fmt.Fprintln(stdout, "\n-dry-run: nothing was created, minted or registered.")
		return nil
	}
	if plan.Empty() {
		return nil
	}

	ctx := context.Background()
	stdoutForPrintSink = stdout
	sink, closeSink, err := sinks.open(ctx)
	if err != nil {
		return err
	}
	defer closeSink()

	env := config.EnvOnly()
	token := strings.TrimSpace(*adminToken)
	if token == "" {
		token = strings.TrimSpace(env.Lookup("GITLAB_ADMIN_TOKEN"))
	}
	if token == "" {
		return errors.New(
			"no administrator token: pass -admin-token or export " +
				"GITLAB_ADMIN_TOKEN. The seats' own tokens are what this run " +
				"MINTS, so it cannot bootstrap itself from them")
	}
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: env.Value(cfg.URL), Token: token,
	})
	if err != nil {
		return err
	}

	res, err := gitlab.Reconcile(ctx, gitlab.Options{
		Client: client, Config: cfg, Plan: plan, Sink: sink,
		WebhookBase:   *publicURL,
		SigningSecret: env.Value(cfg.SigningSecret),
	})
	if err != nil {
		return err
	}
	printResult(stdout, res, sink.Describe())
	return nil
}

// printPlan renders what a run intends to do.
func printPlan(w io.Writer, plan *provision.Plan) {
	if plan.Empty() {
		fmt.Fprintln(w, "No seat references a code-host credential, so there "+
			"is nothing to provision.")
	} else {
		fmt.Fprintf(w, "%d seat(s) to provision:\n", len(plan.Seats))
		for _, seat := range plan.Seats {
			fmt.Fprintf(w, "  %-16s %s → %s\n", seat.Handle, seat.Role, seat.TokenVar)
		}
	}
	printNotes(w, plan.Notes)
}

// printResult renders what a run did.
//
// THE REPORT ENDS WITH THE NOTES, because they are the part an operator has
// to act on: the run itself either worked or returned an error, while a note
// is a seat that was skipped and will keep being skipped.
func printResult(w io.Writer, res *gitlab.Result, where string) {
	fmt.Fprintf(w, "\nRecorded in %s.\n", where)
	if len(res.Created) > 0 {
		fmt.Fprintf(w, "Created %d account(s): %s\n",
			len(res.Created), strings.Join(res.Created, ", "))
	}
	if len(res.Rotated) > 0 {
		// SAID PLAINLY, because it is the surprising part: a personal
		// access token's value is shown once, so there is no
		// already-correct state and every run rotates.
		fmt.Fprintf(w, "Minted a fresh token for %d seat(s): %s\n"+
			"  (every run rotates — a token's value is returned once and "+
			"cannot be read back)\n",
			len(res.Rotated), strings.Join(res.Rotated, ", "))
	}
	if res.Hooked != "" {
		fmt.Fprintf(w, "Webhook registered at %s\n", res.Hooked)
	}
	printNotes(w, res.Notes)
}

func printNotes(w io.Writer, notes []string) {
	if len(notes) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%d note(s):\n", len(notes))
	for _, note := range notes {
		fmt.Fprintf(w, "  - %s\n", note)
	}
}

// runIntegration dispatches `crewlet <vendor> <command>`.
func runIntegration(vendor string, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch {
	case vendor == "gitlab" && sub == "provision":
		return runGitLabProvision(rest, stdout, stderr)
	case sub == "" || sub == "help":
		fmt.Fprintf(stderr, "usage: crewlet %s provision <company.yaml>\n", vendor)
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown %s command %q", vendor, sub)
	}
}
