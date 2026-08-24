package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/plane"
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
	rotate := fs.Bool("rotate", false,
		"mint a fresh token for every seat, including seats whose current "+
			"one still works (the engine has to be restarted after)")
	decommission := fs.Bool("decommission", false,
		"delete managed service accounts whose seats have left the config")
	expiryDays := fs.Int("token-expiry-days", 0,
		"lifetime for minted tokens; 0 sends none and lets the instance "+
			"policy decide")
	dryRun := fs.Bool("dry-run", false,
		"print what the run would do and touch nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// PASSED ONLY WHEN THE OPERATOR TYPED IT, so the flag's default
	// cannot be told from a deliberate zero.
	var expiry *int
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "token-expiry-days" {
			expiry = expiryDays
		}
	})
	tail := fs.Args()
	if companyPath == "" && len(tail) == 1 {
		companyPath, tail = tail[0], nil
	}
	if companyPath == "" || len(tail) > 0 {
		fmt.Fprintln(stderr,
			"usage: crewlet gitlab provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-public-url URL] "+
				"[-rotate] [-decommission] [-token-expiry-days N] [-dry-run]")
		return errors.New("name exactly one company document")
	}
	if expiry != nil && *expiry < 0 {
		return errors.New("-token-expiry-days must not be negative")
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
	printPlan(stdout, plan, "a GitLab credential (mcp_env."+gitlab.SeatEnv+"."+gitlab.CredentialKeys[0]+")")
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
		Rotate:        *rotate, Decommission: *decommission, ExpiryDays: expiry,
	})
	if err != nil {
		return err
	}
	printResult(stdout, res, sink.Describe())
	return nil
}

// printPlan renders what a run intends to do.
// The `what` names the credential a seat would have to reference, because
// an empty plan is almost always a config that names none — and "nothing to
// do" without saying what was looked for sends an operator to the vendor.
func printPlan(w io.Writer, plan *provision.Plan, what string) {
	if plan.Empty() {
		fmt.Fprintf(w, "No seat references %s, so there is nothing to provision.\n", what)
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
		fmt.Fprintf(w, "Minted a token for %d seat(s): %s\n",
			len(res.Rotated), strings.Join(res.Rotated, ", "))
	}
	printKept(w, res.Kept)
	if len(res.Decommissioned) > 0 {
		fmt.Fprintf(w, "Deleted %d account(s) whose seats have left: %s\n",
			len(res.Decommissioned), strings.Join(res.Decommissioned, ", "))
	}
	if res.Hooked != "" {
		// WHERE, not just whether. A group hook covers every project in
		// the group including ones added later; a set of project hooks
		// covers exactly what was listed, and the difference only shows
		// up the day somebody adds a repository.
		where := "on the group"
		if len(res.HookedOn) != 1 || res.HookedOn[0] != "group" {
			where = fmt.Sprintf("on %d project(s): %s",
				len(res.HookedOn), strings.Join(res.HookedOn, ", "))
		}
		fmt.Fprintf(w, "Webhook registered at %s %s\n", res.Hooked, where)
	}
	printNotes(w, res.Notes)
}

// printKept reports the seats a run deliberately did not touch.
//
// SAID OUT LOUD, because it is the successful outcome of a re-run: a report
// that mentioned only what changed would read as a run that did nothing,
// and the operator's next move would be to reach for -rotate — which is
// exactly the outage this behaviour exists to prevent.
func printKept(w io.Writer, kept []string) {
	if len(kept) == 0 {
		return
	}
	fmt.Fprintf(w, "Left %d seat(s) alone — their credential still works, "+
		"and rotating it would revoke what the running engine is using: %s\n"+
		"  (pass -rotate to mint fresh ones, and restart the engine after)\n",
		len(kept), strings.Join(kept, ", "))
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
	case vendor == "mattermost" && sub == "provision":
		return runMattermostProvision(rest, stdout, stderr)
	case vendor == "mattermost" && sub == "doctor":
		return runMattermostDoctor(rest, stdout, stderr)
	case vendor == "plane" && sub == "provision":
		return runPlaneProvision(rest, stdout, stderr)
	case vendor == "plane" && sub == "import":
		return runPlaneImport(rest, stdout, stderr)
	case vendor == "plane" && sub == "resync":
		return runPlaneResync(rest, stdout, stderr)
	case sub == "" || sub == "help":
		fmt.Fprintf(stderr, "usage: crewlet %s provision <company.yaml>\n", vendor)
		if vendor == "mattermost" {
			fmt.Fprintln(stderr,
				"       crewlet mattermost doctor <company.yaml>")
		}
		if vendor == "plane" {
			fmt.Fprintln(stderr,
				"       crewlet plane import <company.yaml> <directory>\n"+
					"       crewlet plane resync <company.yaml>")
		}
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown %s command %q", vendor, sub)
	}
}

// runMattermostProvision is `crewlet mattermost provision`.
func runMattermostProvision(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("mattermost provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	adminToken := fs.String("admin-token", "",
		"a Mattermost token for a system administrator; empty reads MATTERMOST_ADMIN_TOKEN")
	rotate := fs.Bool("rotate", false,
		"mint a fresh token for every bot, including bots whose current "+
			"one still works (the engine has to be restarted after)")
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
			"usage: crewlet mattermost provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-rotate] [-dry-run]")
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
	cfg := company.Integrations.Mattermost
	plan, err := mattermost.PlanFor(organization, cfg)
	if err != nil {
		return err
	}

	printPlan(stdout, plan, "a Mattermost bot token (its role's mattermost.bot_token)")
	if *dryRun {
		fmt.Fprintln(stdout, "\n-dry-run: nothing was created, joined or minted.")
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
		token = strings.TrimSpace(env.Lookup("MATTERMOST_ADMIN_TOKEN"))
	}
	if token == "" {
		return errors.New(
			"no administrator token: pass -admin-token or export " +
				"MATTERMOST_ADMIN_TOKEN. The bots' own tokens are what this " +
				"run MINTS, so it cannot bootstrap itself from them")
	}
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: env.Value(cfg.URL), Token: token,
	})
	if err != nil {
		return err
	}

	res, err := mattermost.Reconcile(ctx, mattermost.Options{
		Client: client, Config: cfg, Org: organization, Plan: plan, Sink: sink,
		Rotate: *rotate,
	})
	if err != nil {
		return err
	}
	printChatResult(stdout, res, sink.Describe())
	return nil
}

// printChatResult renders what a Mattermost run did.
func printChatResult(w io.Writer, res *mattermost.Result, where string) {
	fmt.Fprintf(w, "\nRecorded in %s.\n", where)
	if len(res.Created) > 0 {
		fmt.Fprintf(w, "Created %d bot(s): %s\n",
			len(res.Created), strings.Join(res.Created, ", "))
	}
	if len(res.Rotated) > 0 {
		fmt.Fprintf(w, "Minted a token for %d bot(s): %s\n",
			len(res.Rotated), strings.Join(res.Rotated, ", "))
	}
	printKept(w, res.Kept)
	// THE CHANNELS ARE THE PART TO CHECK. A bot receives only what its
	// channels deliver, so this line is the difference between an agent
	// that wakes and one that never does.
	for _, seat := range sortedKeys(res.Joined) {
		fmt.Fprintf(w, "  %-16s channels: %s\n", seat,
			strings.Join(res.Joined[seat], ", "))
	}
	printNotes(w, res.Notes)
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runPlaneProvision is `crewlet plane provision`.
func runPlaneProvision(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("plane provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	adminToken := fs.String("admin-token", "",
		"a Plane API key for a workspace administrator; empty reads PLANE_ADMIN_TOKEN")
	publicURL := fs.String("public-url", "",
		"this deployment's public base URL, for registering the workspace webhook")
	rotate := fs.Bool("rotate", false,
		"mint a fresh credential for every seat, including seats whose "+
			"current one still works (the engine has to be restarted after)")
	decommission := fs.Bool("decommission", false,
		"delete managed service accounts whose seats have left the config")
	createProjects := fs.Bool("create-projects", false,
		"create configured projects the workspace does not have")
	recreateWebhook := fs.Bool("recreate-webhook", false,
		"delete and remake the workspace webhook to mint a fresh secret; "+
			"invalidates the secret every other deployment holds")
	expiryDays := fs.Int("token-expiry-days", 0,
		"override provisioning.token_expiry_days for this run; 0 means the "+
			"token never expires")
	dryRun := fs.Bool("dry-run", false,
		"print what the run would do and touch nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// PASSED ONLY WHEN THE OPERATOR TYPED IT. Zero is a meaningful value
	// here — it means never-expires — so the flag's default cannot be
	// told from a choice without asking which flags were actually set.
	var expiry *int
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "token-expiry-days" {
			expiry = expiryDays
		}
	})
	tail := fs.Args()
	if companyPath == "" && len(tail) == 1 {
		companyPath, tail = tail[0], nil
	}
	if companyPath == "" || len(tail) > 0 {
		fmt.Fprintln(stderr,
			"usage: crewlet plane provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-public-url URL] "+
				"[-rotate] [-decommission] [-create-projects] "+
				"[-recreate-webhook] [-token-expiry-days N] [-dry-run]")
		return errors.New("name exactly one company document")
	}
	if expiry != nil && *expiry < 0 {
		return errors.New(
			"-token-expiry-days must not be negative: it would silently mean " +
				"0, which is the inverse of a shorter expiry. Use 0 for a " +
				"token that never expires")
	}

	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return err
	}
	organization, err := company.Organization()
	if err != nil {
		return err
	}
	cfg := company.Integrations.Plane
	plan, err := plane.PlanFor(organization, cfg)
	if err != nil {
		return err
	}

	printPlan(stdout, plan, "a Plane API key (mcp_env."+plane.SeatEnv+"."+plane.SeatKey+")")
	if *dryRun {
		fmt.Fprintln(stdout, "\n-dry-run: nothing was created, joined, minted or registered.")
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
		token = strings.TrimSpace(env.Lookup("PLANE_ADMIN_TOKEN"))
	}
	if token == "" {
		return errors.New(
			"no administrator key: pass -admin-token or export PLANE_ADMIN_TOKEN. " +
				"The seats' own keys are what this run MINTS, so it cannot " +
				"bootstrap itself from them")
	}
	client, err := plane.NewClient(plane.ClientOptions{
		URL: env.Value(cfg.URL), Workspace: env.Value(cfg.Workspace), APIKey: token,
	})
	if err != nil {
		return err
	}

	res, err := plane.Reconcile(ctx, plane.Options{
		Client: client, Config: cfg, Plan: plan, Sink: sink,
		Org: organization, WebhookBase: *publicURL,
		Rotate: *rotate, Decommission: *decommission,
		CreateProjects: *createProjects, RecreateWebhook: *recreateWebhook,
		ExpiryDays: expiry,
	})
	if err != nil {
		return err
	}
	printTrackerResult(stdout, res, sink.Describe())
	return nil
}

// printTrackerResult renders what a Plane run did.
func printTrackerResult(w io.Writer, res *plane.Result, where string) {
	fmt.Fprintf(w, "\nRecorded in %s.\n", where)
	if len(res.Created) > 0 {
		fmt.Fprintf(w, "Created %d service account(s): %s\n",
			len(res.Created), strings.Join(res.Created, ", "))
	}
	if len(res.Rotated) > 0 {
		fmt.Fprintf(w, "Minted a token for %d seat(s): %s\n",
			len(res.Rotated), strings.Join(res.Rotated, ", "))
	}
	printKept(w, res.Kept)
	if len(res.Decommissioned) > 0 {
		fmt.Fprintf(w, "Deleted %d account(s) whose seats have left: %s\n",
			len(res.Decommissioned), strings.Join(res.Decommissioned, ", "))
	}
	if len(res.Joined) > 0 {
		fmt.Fprintf(w, "Joined to project(s): %s\n", strings.Join(res.Joined, ", "))
	}
	if res.Hooked != "" {
		fmt.Fprintf(w, "Webhook registered at %s\n", res.Hooked)
	}
	printNotes(w, res.Notes)
	printMembers(w, res.Members)
}

// printMembers ends the report with the workspace member table.
//
// LAST, and after the notes that point at it: the ids are what a founder
// copies into `contact.plane_user_id` for their human seats, and Plane's own
// UI does not show them anywhere. Sorted by username so a re-run's output
// diffs cleanly against the last one.
// printMembers reports the PEOPLE in the workspace, with their ids.
//
// # People, and a count that is not the workspace's
//
// The table exists for one thing: a human seat is reached by the UUID in
// `contact.plane_user_id`, nobody can guess it, and Plane's own UI does not
// show it. Service accounts are not in that answer — the run manages those,
// and the lines above already name every one it created or kept.
//
// It is also the table as the run FOUND it, before it created anything,
// which is exactly right for people (a run never creates one) and exactly
// wrong as a workspace census. Printing "Workspace members (2)" under a line
// saying eight accounts were created reads as a run that half-failed; it was
// the pre-run snapshot all along.
func printMembers(w io.Writer, members []plane.Account) {
	people := make([]plane.Account, 0, len(members))
	for _, m := range members {
		if !m.IsBot {
			people = append(people, m)
		}
	}
	if len(people) == 0 {
		return
	}
	sort.Slice(people, func(i, j int) bool {
		return people[i].Username < people[j].Username
	})
	fmt.Fprintf(w, "\nPeople in this workspace (%d) — the id column is what "+
		"contact.plane_user_id takes:\n", len(people))
	for _, m := range people {
		fmt.Fprintf(w, "  %-24s %-38s %s\n", m.Username, m.ID, m.Email)
	}
}

// runPlaneImport is `crewlet plane import`.
func runPlaneImport(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)
	root, args := splitSubject(args)

	fs := flag.NewFlagSet("plane import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apiKey := fs.String("token", "",
		"a Plane API key that may write the target projects; empty reads PLANE_TOKEN")
	prune := fs.Bool("prune", false,
		"delete published tool-skill pages whose key no local file publishes")
	dryRun := fs.Bool("dry-run", false,
		"print what would be published and write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tail := fs.Args()
	for _, extra := range tail {
		switch {
		case companyPath == "":
			companyPath = extra
		case root == "":
			root = extra
		default:
			root = "" // too many positionals; fall through to the usage
		}
	}
	if companyPath == "" || root == "" {
		fmt.Fprintln(stderr,
			"usage: crewlet plane import <company.yaml> <directory> "+
				"[-token KEY] [-prune] [-dry-run]")
		return errors.New("name a company document and a directory")
	}

	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return err
	}
	cfg := company.Integrations.Plane
	plan, err := plane.Walk(root, cfg)
	if err != nil {
		return err
	}
	printImportPlan(stdout, plan)
	if *dryRun {
		fmt.Fprintln(stdout, "\n-dry-run: nothing was published or deleted.")
		return nil
	}
	if len(plan.Items) == 0 {
		return nil
	}

	env := config.EnvOnly()
	key := strings.TrimSpace(*apiKey)
	if key == "" {
		key = strings.TrimSpace(env.Lookup("PLANE_TOKEN"))
	}
	if key == "" {
		key = strings.TrimSpace(env.Value(cfg.Token))
	}
	if key == "" {
		return errors.New(
			"no API key: pass -token, export PLANE_TOKEN, or set " +
				"integrations.plane.token to a variable this shell has")
	}
	client, err := plane.NewClient(plane.ClientOptions{
		URL: env.Value(cfg.URL), Workspace: env.Value(cfg.Workspace), APIKey: key,
	})
	if err != nil {
		return err
	}

	res, err := plane.Publish(context.Background(), plane.PublishOptions{
		Client: client, Config: cfg, Plan: plan, Prune: *prune,
	})
	if err != nil {
		return err
	}
	printImportResult(stdout, res)
	if len(res.Failed) > 0 {
		// THE EXIT CODE CARRIES THE TRUTH. Page failures are isolated so
		// one locked page does not cost the other forty, but a run that
		// exited 0 with pages missing would be reported as a successful
		// import by whatever ran it.
		return fmt.Errorf("%d page(s) could not be published", len(res.Failed))
	}
	return nil
}

// printImportPlan renders what an import intends to publish.
func printImportPlan(w io.Writer, plan *plane.Plan) {
	if len(plan.Items) == 0 {
		fmt.Fprintln(w, "Nothing to publish.")
	} else {
		fmt.Fprintf(w, "%d page(s) to publish:\n", len(plan.Items))
		for _, item := range plan.Items {
			kind := "doc"
			if item.Skill {
				kind = "skill"
			}
			fmt.Fprintf(w, "  %-5s %-10s %s\n", kind, item.Container, item.Title)
		}
	}
	printNotes(w, plan.Notes)
}

// printImportResult renders what an import did.
func printImportResult(w io.Writer, res *plane.PublishResult) {
	fmt.Fprintln(w)
	if len(res.Created) > 0 {
		fmt.Fprintf(w, "Published %d new page(s): %s\n",
			len(res.Created), strings.Join(res.Created, ", "))
	}
	if len(res.Updated) > 0 {
		fmt.Fprintf(w, "Updated %d page(s): %s\n",
			len(res.Updated), strings.Join(res.Updated, ", "))
	}
	if len(res.Pruned) > 0 {
		fmt.Fprintf(w, "Deleted %d orphaned skill page(s): %s\n",
			len(res.Pruned), strings.Join(res.Pruned, ", "))
	}
	if len(res.Failed) > 0 {
		fmt.Fprintf(w, "\n%d page(s) FAILED:\n", len(res.Failed))
		for _, failure := range res.Failed {
			fmt.Fprintf(w, "  - %s\n", failure)
		}
	}
	printNotes(w, res.Notes)
}

// runPlaneResync is `crewlet plane resync`.
//
// # It reports, it does not reach into a running engine
//
// A live engine receives Plane page webhooks directly, so its registry is
// already current. This re-runs the SAME walk and the SAME admission
// against a throwaway registry and prints what loaded — which is the answer
// to "why is this skill not being applied", and it is a read-only question.
// Restart the engine, or wait for the next webhook, to change what it holds.
func runPlaneResync(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("plane resync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apiKey := fs.String("token", "",
		"a Plane API key that may read the skills project; empty reads PLANE_TOKEN")
	project := fs.String("project", "",
		"the skills project identifier; empty takes integrations.plane.skills_project")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tail := fs.Args()
	if companyPath == "" && len(tail) == 1 {
		companyPath, tail = tail[0], nil
	}
	if companyPath == "" || len(tail) > 0 {
		fmt.Fprintln(stderr,
			"usage: crewlet plane resync <company.yaml> [-token KEY] [-project ID]")
		return errors.New("name exactly one company document")
	}

	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return err
	}
	cfg := company.Integrations.Plane
	if cfg == nil || !cfg.Enabled {
		return errors.New("plane: the company config does not enable plane")
	}
	env := config.EnvOnly()
	key := strings.TrimSpace(*apiKey)
	if key == "" {
		key = strings.TrimSpace(env.Lookup("PLANE_TOKEN"))
	}
	if key == "" {
		key = strings.TrimSpace(env.Value(cfg.Token))
	}
	if key == "" {
		return errors.New(
			"no API key: pass -token, export PLANE_TOKEN, or set " +
				"integrations.plane.token to a variable this shell has")
	}
	client, err := plane.NewClient(plane.ClientOptions{
		URL: env.Value(cfg.URL), Workspace: env.Value(cfg.Workspace), APIKey: key,
	})
	if err != nil {
		return err
	}

	identifier := strings.TrimSpace(*project)
	if identifier == "" {
		identifier = cfg.SkillsProjectKey()
	}
	pages, err := plane.SkillPages(context.Background(), client, identifier)
	if err != nil {
		return err
	}
	loaded, report := skills.Admit(pages)
	fmt.Fprintf(stdout, "%s holds %d page(s): %d skill(s), %d ordinary page(s).\n",
		identifier, report.Pages, len(loaded), report.Ordinary)
	for _, skill := range loaded {
		fmt.Fprintf(stdout, "  %-28s %s\n", skill.Key, skill.Title)
	}
	if len(report.Undecodable) > 0 {
		// A PAGE THAT LOOKS LIKE A SKILL AND DOES NOT PARSE is the case
		// worth printing: somebody wrote a trigger and got the rest
		// wrong, and the only other symptom is guidance that never
		// appears.
		fmt.Fprintf(stdout, "\n%d page(s) declare a trigger and did not parse:\n",
			len(report.Undecodable))
		for _, title := range report.Undecodable {
			fmt.Fprintf(stdout, "  - %s\n", title)
		}
		return fmt.Errorf("%d page(s) could not be decoded", len(report.Undecodable))
	}
	return nil
}

// runMattermostDoctor is `crewlet mattermost doctor`.
func runMattermostDoctor(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("mattermost doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	adminToken := fs.String("admin-token", "",
		"a Mattermost token to run the checks as; empty reads "+
			"MATTERMOST_ADMIN_TOKEN, and failing that borrows a seat's own")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tail := fs.Args()
	if companyPath == "" && len(tail) == 1 {
		companyPath, tail = tail[0], nil
	}
	if companyPath == "" || len(tail) > 0 {
		fmt.Fprintln(stderr,
			"usage: crewlet mattermost doctor <company.yaml> [-admin-token TOKEN]")
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
	cfg := company.Integrations.Mattermost
	if cfg == nil || !cfg.Enabled {
		return errors.New("mattermost: the company config does not enable mattermost")
	}

	env := config.EnvOnly()
	token := strings.TrimSpace(*adminToken)
	if token == "" {
		token = strings.TrimSpace(env.Lookup("MATTERMOST_ADMIN_TOKEN"))
	}
	// AN EMPTY TOKEN IS FINE. The checks that need one borrow a seat's,
	// because those are the credentials the engine authenticates with —
	// and asking an operator to mint an admin token to find out whether
	// their company works is a step that exists only to be skipped.
	//
	// THE CONFIGURED URL, resolved but NOT normalised away: the whole
	// point is comparing what this company believes against what the
	// server reports, so the check has to see the operator's own value.
	resolved := *cfg
	resolved.URL = env.Value(cfg.URL)
	resolved.Team = env.Value(cfg.Team)
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: resolved.URL, Token: token,
	})
	if err != nil {
		return err
	}

	report, err := mattermost.Doctor(context.Background(), mattermost.DoctorOptions{
		Client: client, Config: &resolved, Org: organization,
		SeatToken: mattermost.SeatTokens(env),
	})
	if err != nil {
		return err
	}
	printDoctor(stdout, report)
	if !report.Healthy() {
		return errors.New("mattermost: the instance is not healthy for this company")
	}
	return nil
}

// printDoctor renders a health report.
func printDoctor(w io.Writer, report *mattermost.Report) {
	for _, finding := range report.Findings {
		mark := "ok  "
		if !finding.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "%s  %-16s %s\n", mark, finding.Check, finding.Detail)
	}
	switch {
	case report.Stopped():
		// SAID OUT LOUD: one failing line with nothing after it reads
		// as "one thing is wrong", when what it means is "nothing else
		// was even asked".
		fmt.Fprintln(w, "\nThe checks stopped here — everything below this "+
			"would have reported a consequence rather than a cause. Fix it "+
			"and run again.")
	case report.Healthy():
		fmt.Fprintln(w, "\nEverything this command can check is working.")
	}
}
