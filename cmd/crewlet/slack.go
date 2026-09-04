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
	"github.com/crewlet/crewlet/internal/slack"
)

// `crewlet slack provision` — one Slack app per agent seat.
//
// # The one command with a human step in the middle
//
// Every other vendor's provisioning runs to completion unattended. Slack
// cannot: installing an app into a workspace is an OAuth grant, and OAuth
// exists precisely so that a person decides. So this run creates and updates
// the apps by itself, then hands the operator one authorize URL per seat and
// takes the code back — and when there is no operator to ask (-dry-run, or a
// pipe), it prints the URLs and stops rather than pretending.

func runSlackProvision(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("slack provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	publicURLFlag := fs.String("public-url", "",
		"this deployment's public HTTPS base URL; every app's request URL "+
			"and redirect URL are built from it. Empty takes api.public_url "+
			"from the Tier A config")
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
				"[-secret-store|-env-file PATH|-print] -public-url URL "+
				"[-config-token TOKEN] [-ledger PATH] [-handles a,b] "+
				"[-reinstall] [-no-install] [-dry-run]")
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
	plans := slack.PlanFor(organization)
	publicURL := publicBase(*publicURLFlag, *sinks.bootstrap)
	if *ledgerPath == "" {
		*ledgerPath = slack.LedgerPathFor(companyPath)
	}
	ledger, err := slack.LoadLedger(*ledgerPath)
	if err != nil {
		return err
	}

	printSlackPlan(stdout, plans, ledger, *ledgerPath, publicURL)

	refresh := strings.TrimSpace(*refreshToken)
	if refresh == "" {
		refresh = operatorCredential("SLACK_CONFIG_REFRESH_TOKEN")
	}

	if *dryRun {
		// THE MANIFESTS ARE CHECKED, which is the whole reason a dry run
		// touches the network: apps.manifest.create is rate limited to
		// roughly one request a minute, so discovering a malformed
		// manifest from the create costs a minute per seat and leaves
		// the seats before the bad one already created.
		res, checked := slack.Validate(context.Background(), slack.Options{
			Admin: slack.NewAdmin(nil), Seats: plans,
			Ledger: ledger, LedgerPath: *ledgerPath,
			BaseURL: publicURL, ConfigRefreshToken: refresh,
			Only: splitHandles(*only),
		})
		if res != nil {
			printSlackValidation(stdout, res)
		}
		fmt.Fprintln(stdout, "\n-dry-run: no app was created, updated or installed.")
		return checked
	}
	if len(plans) == 0 {
		return nil
	}
	if publicURL == "" {
		return errors.New(
			"no -public-url and no api.public_url in the Tier A config: every " +
				"app's Events API request URL and OAuth redirect URL are built " +
				"from it, so an app created without one delivers nowhere and " +
				"cannot be installed")
	}

	ctx := context.Background()
	sink, closeSink, err := sinks.open(ctx, stdout)
	if err != nil {
		return err
	}
	defer closeSink()

	opts := slack.Options{
		Admin: slack.NewAdmin(nil), Seats: plans,
		Ledger: ledger, LedgerPath: *ledgerPath, Sink: sink,
		BaseURL: publicURL, ConfigRefreshToken: refresh, Reinstall: *reinstall,
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

// askForInstallCode is the human step: show the URL, take the code back.
//
// AN EMPTY LINE SKIPS THE SEAT rather than failing the run, and that is
// deliberate: an operator part-way through seven apps may not have
// permission to install the eighth, and the six already done must not be
// undone by it. The app exists either way and the next run picks it up.
func askForInstallCode(stdout io.Writer, stdin io.Reader) slack.Installer {
	reader := bufio.NewReader(stdin)
	return func(_ context.Context, handle, authorize string) (string, error) {
		fmt.Fprintf(stdout, "\n%s — open this and click Allow:\n  %s\n", handle, authorize)
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

// printSlackResult renders what a run did.
func printSlackResult(w io.Writer, res *slack.Result, ledgerPath string, sink interface{ Describe() string }) {
	fmt.Fprintf(w, "\nRecorded in %s; app ledger in %s.\n", sink.Describe(), ledgerPath)
	// SAID EVERY RUN, because the ledger holds client secrets Slack
	// serves once and an operator who commits it has published them.
	fmt.Fprintf(w, "%s holds app client secrets — keep it out of version control.\n",
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
	printKept(w, res.Kept)
	if len(res.Failed) > 0 {
		handles := slices.Sorted(maps.Keys(res.Failed))
		fmt.Fprintf(w, "\n%d seat(s) FAILED — everything else completed and is "+
			"recorded, so re-running resumes:\n", len(handles))
		for _, handle := range handles {
			fmt.Fprintf(w, "  %-16s %s\n", handle, res.Failed[handle])
		}
	}
	if len(res.Pending) > 0 {
		handles := slices.Sorted(maps.Keys(res.Pending))
		fmt.Fprintf(w, "\n%d app(s) still need a workspace install — open each "+
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
