package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/slack"
)

// `crewlet slack provision` — one Slack app per agent seat.
//
// # The one command with a human step in the middle
//
// Every other third-party app's provisioning runs to completion unattended. Slack
// cannot: installing an app into a workspace is an OAuth grant, and OAuth
// exists precisely so that a person decides. So this run creates and updates
// the apps by itself, then hands the operator one authorize URL per seat and
// takes the code back — and when there is no operator to ask (-dry-run, or a
// pipe), it prints the URLs and stops rather than pretending.

func runSlackProvision(inv invocation, args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("slack provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	publicURL := fs.String("public-url", "",
		"this deployment's public HTTPS base URL; every app's request URL "+
			"and redirect URL are built from it. Defaults to "+
			publicURLDefault)
	refreshToken := fs.String("config-token", "",
		"a Slack app-configuration REFRESH token; empty reads SLACK_CONFIG_REFRESH_TOKEN")
	ledgerPath := fs.String("ledger", "",
		"where the app ledger lives; empty puts it beside the company document")
	reinstall := fs.Bool("reinstall", false,
		"run the OAuth install again even for a seat whose token is already "+
			"recorded (this revokes that seat's current token)")
	only := fs.String("handles", "",
		"comma-separated seat handles to provision; empty does every one")
	noInstall := fs.Bool("no-install", false,
		"create and update the apps, and print the authorize URLs instead of "+
			"asking for codes")
	dryRun := fs.Bool("dry-run", false,
		"print what the run would do and touch nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	companyPath, given := onePositional(fs, companyPath)
	if given != 1 {
		fmt.Fprintln(stderr,
			"usage: crewlet slack provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-public-url URL] "+
				"[-config-token TOKEN] [-ledger PATH] [-handles a,b] "+
				"[-reinstall] [-no-install] [-dry-run] [-config PATH] [-api URL]")
		return errors.New("name exactly one company document")
	}

	company, err := config.LoadCompanyToRun(companyPath, nil)
	if err != nil {
		return err
	}
	organization, err := company.Organization()
	if err != nil {
		return err
	}
	// TIER A ALONE, and no ${VAR} of the company's: the one thing this run
	// reads from the deployment is the ADDRESS, and it is no longer a Tier B
	// value — what goes into an app manifest is a URL, and while it was a
	// Tier B pointer a manifest could carry "${PUBLIC_URL}/webhooks/slack/<handle>"
	// where an address belongs, which Slack refuses naming nothing. The
	// address is Tier A's now ([webhookBase]), resolved before the file is
	// decoded, so resolving the company's values here would open the store,
	// or reveal credentials through the running node, for nothing read.
	// Loading it opens nothing: only a real run with seats to provision
	// reaches the store, below, for what its sink needs of it.
	companySecrets, err := openCompanyStore(inv.environ, sinks.store,
		"crewlet slack provision")
	if err != nil {
		return err
	}
	defer companySecrets.Close()
	boot := companySecrets.boot

	// READ ONCE. Four places below build a URL from it, and four separate
	// reads of the flag is how one of them ends up using the flag while
	// the others use the deployment's own address.
	base := webhookBase(*publicURL, boot)

	plans := slack.PlanFor(organization)
	// WHETHER THERE IS ANYTHING TO PROVISION: a seat this run carries
	// through ([slackProvisions]). A seat whose bot_token or signing_secret
	// is a literal is one an operator manages by hand, which the plan reports
	// and the run leaves alone, so a run with no other seat reads nothing of
	// the store, whatever its sink, and decides no route.
	provisions := slices.ContainsFunc(plans, func(plan slack.SeatPlan) bool {
		return slackProvisions(plan, splitHandles(*only))
	})
	if *ledgerPath == "" {
		*ledgerPath = slack.LedgerPathFor(companyPath)
	}
	ledger, err := slack.LoadLedger(*ledgerPath)
	if err != nil {
		return err
	}

	refresh := strings.TrimSpace(*refreshToken)
	if refresh == "" {
		refresh = operatorCredential(inv.environ, "SLACK_CONFIG_REFRESH_TOKEN")
	}

	if *dryRun || !provisions {
		printSlackPlan(stdout, plans, ledger, *ledgerPath, base)
	}
	if *dryRun {
		// THE MANIFESTS ARE CHECKED, which is the whole reason a dry run
		// touches the network: apps.manifest.create is rate limited to
		// roughly one request a minute, so discovering a malformed
		// manifest from the create costs a minute per seat and leaves
		// the seats before the bad one already created.
		res, checked := slack.Validate(context.Background(), slack.Options{
			Admin: slack.NewAdmin(inv.slack), Seats: plans,
			Ledger: ledger, LedgerPath: *ledgerPath,
			BaseURL: base, ConfigRefreshToken: refresh,
			Only: splitHandles(*only),
		})
		if res != nil {
			printSlackValidation(stdout, res)
		}
		fmt.Fprintln(stdout, "\n-dry-run: no app was created, updated or installed.")
		return checked
	}
	if !provisions {
		if len(plans) > 0 {
			fmt.Fprintln(stdout, "\nNothing to provision: no seat in this run has a bot_token "+
				"and a signing_secret that are each a whole ${VAR} reference.")
		}
		return nil
	}
	// A REAL RUN DECIDES ITS ROUTE BEFORE IT PRINTS ANYTHING, as every reader
	// of the company does, whatever its sink — and the sink is chosen first,
	// for the reason GitLab's command gives.
	if err = sinks.check(); err != nil {
		return err
	}
	ctx := context.Background()
	if sinks.recordsInStore() {
		// A RUN THAT RECORDS INTO THE STORE READS IT BACK — the token of
		// each seat whose app the ledger records installed, to leave that
		// install alone — and reads those tokens in one batch up front.
		// -reinstall reads none of them back.
		var readBack []string
		if !*reinstall {
			readBack = slackTokenVars(plans, ledger, splitHandles(*only))
		}
		if _, err = companySecrets.stored(ctx, readBack, stdout); err != nil {
			return err
		}
	} else {
		// AND ONE THAT RECORDS ANYWHERE ELSE ASKS THE STORE WHAT IT HOLDS,
		// and nothing more: an install mints each seat's token and the run
		// records each app's signing secret, and where the store holds
		// either name, the engine reads the store's over anything recorded
		// here.
		companySecrets.onlyLists()
		if err = sinks.refuseOutranked(ctx, companySecrets, "Slack",
			slackMintsInto(plans, splitHandles(*only), !*noInstall), stdout); err != nil {
			return err
		}
	}
	printSlackPlan(stdout, plans, ledger, *ledgerPath, base)
	if base == "" {
		// TWO WAYS TO BE EMPTY, and they need different work: no Tier A
		// file was read, so only the flag could have supplied one, or the
		// file names no webhook address — and which key that is depends
		// on whether it sets a public listener. The message named only the
		// flag once, so an operator who had set the field went looking for
		// a flag they did not need; see [noPublicBase] for the rest.
		return errors.New(noPublicBase(boot))
	}

	sink, err := sinks.open(ctx, companySecrets, stdout)
	if err != nil {
		return err
	}

	opts := slack.Options{
		Admin: slack.NewAdmin(inv.slack), Seats: plans,
		Ledger: ledger, LedgerPath: *ledgerPath, Sink: sink,
		BaseURL: base, ConfigRefreshToken: refresh, Reinstall: *reinstall,
		Only: splitHandles(*only),
	}
	if !*noInstall {
		opts.Install = askForInstallCode(stdout, os.Stdin)
	}

	res, err := slack.Reconcile(ctx, opts)
	if res != nil {
		printSlackResult(stdout, res, *ledgerPath, sink)
	}
	return err
}

// slackProvisions reports whether a run provisions this seat: one only names,
// or any where only names none, whose credentials the run can record at all
// ([slack.SeatPlan.Provisionable]) — a seat [slack.Reconcile] carries through,
// and the only kind a run reads the store for.
func slackProvisions(plan slack.SeatPlan, only []string) bool {
	return plan.Provisionable() && (len(only) == 0 || slices.Contains(only, plan.Handle))
}

// slackTokenVars is the variable each seat a run provisions
// ([slackProvisions]) records its bot token under, for each one whose app the
// ledger records installed — the tokens a `-secret-store` run reads back.
//
// ONLY AN INSTALLED APP'S, because only there can the store's answer spare an
// install: an app nobody has installed has no token anywhere to keep, so its
// install asks the store nothing. Read anyway, a token the store held for one
// — left by a ledger that was lost, say, or set by hand — cost a reveal for
// nothing, and a read that failed — a reveal refused to a machine token, which
// is refused every one, or a row the keyring cannot open — refused the whole
// run before it had created an app. A recorded app that turns out deleted in
// Slack is replaced by a record that is not installed, which asks nothing
// either, so the ledger as the run loads it names every token the run can read
// back.
func slackTokenVars(plans []slack.SeatPlan, ledger *slack.Ledger, only []string) []string {
	var names []string
	for _, plan := range plans {
		if slackProvisions(plan, only) && ledger.Apps[plan.Handle].Installed() {
			names = append(names, plan.TokenVar)
		}
	}
	return unionReads(names)
}

// slackMintsInto is every variable a run can record a seat's credentials
// into, for each seat it provisions ([slackProvisions]): its signing secret,
// which the run records from the app it creates or the ledger holds, and,
// where install is set (the run has no -no-install), its bot token, which an
// install mints. Whether an app is created or an install run is settled
// against the ledger and Slack part way through the run, so every such seat
// counts.
//
// -no-install RECORDS NO BOT TOKEN: with nobody to ask for a code, each seat's
// authorize URL is printed and nothing is installed, so counting the token
// there would refuse a run over a name it never writes.
func slackMintsInto(plans []slack.SeatPlan, only []string, install bool) []string {
	var names []string
	for _, plan := range plans {
		if slackProvisions(plan, only) {
			names = append(names, plan.SigningVar)
			if install {
				names = append(names, plan.TokenVar)
			}
		}
	}
	return unionReads(names)
}

// askForInstallCode is the human step: show the URL, take the code back.
//
// AN EMPTY LINE SKIPS THE SEAT rather than failing the run, and that is
// deliberate: an operator part-way through seven apps may not have
// permission to install the eighth, and the six already done must not be
// undone by it. The app exists either way and the next run picks it up.
func askForInstallCode(stdout io.Writer, stdin io.Reader) slack.Installer {
	reader := bufio.NewReader(stdin)
	return func(_ context.Context, handle, authorize string) (string, error) {
		fmt.Fprintf(stdout, "\n%s: open this and click Allow:\n  %s\n", handle, authorize)
		fmt.Fprintf(stdout,
			"Paste the code the landing page shows (or the whole redirect "+
				"URL), or press enter to skip %s: ", handle)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return slack.InstallCode(line), nil
	}
}

// printSlackPlan renders what a run intends to do.
func printSlackPlan(w io.Writer, plans []slack.SeatPlan, ledger *slack.Ledger, ledgerPath, publicURL string) {
	if len(plans) == 0 {
		fmt.Fprintln(w,
			"No seat declares integrations.slack, so there is nothing to provision.")
		return
	}
	fmt.Fprintf(w, "%d seat(s) with a Slack app, ledger %s:\n", len(plans), ledgerPath)
	for _, plan := range plans {
		state := "to create"
		if record, ok := ledger.Apps[plan.Handle]; ok {
			state = "app " + record.AppID
			if record.Installed() {
				state += ", installed"
			} else {
				state += ", NOT installed"
			}
		}
		fmt.Fprintf(w, "  %-16s %s\n", plan.Handle, state)
		if publicURL != "" {
			fmt.Fprintf(w, "  %-16s events → %s\n", "",
				slack.EventsRequestURL(publicURL, plan.Handle))
		}
	}
	var notes []string
	for _, plan := range plans {
		for _, note := range plan.Notes {
			notes = append(notes, plan.Handle+": "+note)
		}
	}
	printNotes(w, notes)
}

// printSlackResult renders what a run did, and — where it recorded a value a
// running engine does not hold yet — what has to happen for the engine to
// read it ([printNextStep]).
func printSlackResult(w io.Writer, res *slack.Result, ledgerPath string, sink provision.TokenSink) {
	fmt.Fprintf(w, "\nRecorded in %s; app ledger in %s.\n", sink.Describe(), ledgerPath)
	printNextStep(w, res.Recorded, sink)
	// SAID EVERY RUN, because the ledger holds client secrets Slack
	// serves once and an operator who commits it has published them.
	fmt.Fprintf(w, "%s holds app client secrets. Keep it out of version control.\n",
		ledgerPath)

	if len(res.Created) > 0 {
		fmt.Fprintf(w, "Created %d app(s): %s\n", len(res.Created), strings.Join(res.Created, ", "))
	}
	if len(res.Updated) > 0 {
		fmt.Fprintf(w, "Pushed a new manifest for %d app(s): %s\n",
			len(res.Updated), strings.Join(res.Updated, ", "))
	}
	if len(res.Installed) > 0 {
		fmt.Fprintf(w, "Installed %d app(s): %s\n",
			len(res.Installed), strings.Join(res.Installed, ", "))
	}
	printKept(w, res.Kept, "-reinstall")
	if len(res.Failed) > 0 {
		handles := slices.Sorted(maps.Keys(res.Failed))
		fmt.Fprintf(w, "\n%d seat(s) FAILED. Everything else completed and is "+
			"recorded, so re-running resumes:\n", len(handles))
		for _, handle := range handles {
			fmt.Fprintf(w, "  %-16s %s\n", handle, res.Failed[handle])
		}
	}
	if len(res.Pending) > 0 {
		handles := slices.Sorted(maps.Keys(res.Pending))
		fmt.Fprintf(w, "\n%d app(s) still need a workspace install. Open each "+
			"and click Allow, then re-run:\n", len(handles))
		for _, handle := range handles {
			fmt.Fprintf(w, "  %-16s %s\n", handle, res.Pending[handle])
		}
	}
	printNotes(w, res.Notes)
}

// splitHandles reads the -handles list.
func splitHandles(raw string) []string {
	var out []string
	for handle := range strings.SplitSeq(raw, ",") {
		if handle = strings.TrimSpace(handle); handle != "" {
			out = append(out, handle)
		}
	}
	return out
}

// printSlackValidation renders what a dry run's manifest check found.
func printSlackValidation(w io.Writer, res *slack.Result) {
	if len(res.Validated) > 0 {
		fmt.Fprintf(w, "\nSlack accepted %d manifest(s): %s\n",
			len(res.Validated), strings.Join(res.Validated, ", "))
	}
	if len(res.Failed) > 0 {
		handles := slices.Sorted(maps.Keys(res.Failed))
		fmt.Fprintf(w, "\n%d manifest(s) FAILED validation:\n", len(handles))
		for _, handle := range handles {
			fmt.Fprintf(w, "  %s: %s\n", handle, res.Failed[handle])
		}
	}
	printNotes(w, res.Notes)
}
