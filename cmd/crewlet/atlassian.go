package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provision"
)

// `crewlet atlassian provision` — the organization's service accounts.
//
// # It is the third true minting reconcile, and it took a correction to get
// here
//
// `crewlet jira provision` says Atlassian issues no credential on a
// provisioner's behalf. That is true of a USER account and false of a SERVICE
// account: with an organization API key created without scopes, the admin API
// creates the account, mints its credential, and licenses it into a product.
// So this command creates one Atlassian identity per agent seat — which is
// what lets an agent be an assignee, a watcher and an @mention target in its
// own right rather than acting through one shared company account.
//
// # Cloud only, and it says so rather than degrading
//
// The account-management API does not exist on Data Center, where a personal
// access token can only be minted for the calling user. A Data Center company
// is refused up front, by name, and pointed at `crewlet jira provision` —
// which keeps the Data Center duties: the seat-identity report, the
// project/lead agreement check, and the inbound webhook. Two commands that
// look interchangeable and are not is worse than one that says no.
//
// # There are no webhook flags here, and that is not an omission
//
// Cloud events arrive through the Forge app on /webhooks/forge, verified by
// the app's invocation token against integrations.forge_app_id. There is no
// HMAC secret in that path and no dynamic webhook an API token may register,
// so a -public-url here would be a flag that can never do anything.

func runAtlassianProvision(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("atlassian provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	adminToken := fs.String("admin-token", "",
		"an Atlassian organization API key, created WITHOUT scopes; empty "+
			"reads ATLASSIAN_ORG_API_KEY, then ATLASSIAN_ADMIN_TOKEN")
	rotate := fs.Bool("rotate", false,
		"mint a fresh credential for every seat, including seats whose "+
			"current one still works (the engine has to be restarted after)")
	only := fs.String("handles", "",
		"provision only these seat handles, comma-separated; empty does all")
	decommission := fs.Bool("decommission", false,
		"delete the service accounts of seats that have left the config — "+
			"Atlassian has no disable verb, so the account's history goes with it")
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
			"usage: crewlet atlassian provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-handles a,b] "+
				"[-rotate] [-decommission] [-dry-run]")
		return errors.New("name exactly one company document")
	}

	company, err := config.LoadCompany(companyPath)
	if err != nil {
		return err
	}
	cfg := company.Integrations.Atlassian
	if cfg == nil {
		return errors.New(
			"atlassian: the company config has no integrations.atlassian " +
				"block, so there is no organization to provision service " +
				"accounts in — add one naming org_id, which is on the Settings " +
				"page at admin.atlassian.com")
	}
	organization, err := company.Organization()
	if err != nil {
		return err
	}

	ctx := context.Background()
	env, closeEnv, err := companyResolver(ctx, *sinks.bootstrap, stdout)
	if err != nil {
		return err
	}
	defer closeEnv()

	sites, err := atlassianSites(&company.Integrations, env)
	if err != nil {
		return err
	}
	products := make([]atlassian.Product, 0, len(sites))
	for _, product := range atlassian.Products {
		if sites[product] != "" {
			products = append(products, product)
		}
	}

	orgID := strings.TrimSpace(env.Value(cfg.OrgID))
	if orgID == "" {
		return fmt.Errorf(
			"atlassian: integrations.atlassian.org_id (%q) resolved empty — it "+
				"is the organization every service account is created in",
			cfg.OrgID)
	}

	plan, err := atlassian.PlanFor(organization, cfg, products)
	if err != nil {
		return err
	}

	// NARROWED BEFORE IT IS PRINTED. -handles is passed to the run below,
	// so a plan printed un-narrowed describes a run that will not happen —
	// and a -dry-run that lists twelve seats before provisioning one is the
	// opposite of what the flag promises. Narrowing here also names a handle
	// that matched no seat, which otherwise printed a full plan, an empty
	// result and exit 0 while the seat kept its old credential.
	handles := splitHandles(*only)
	if notes := narrowPlan(plan, handles); len(notes) > 0 {
		plan.Notes = append(plan.Notes, notes...)
	}

	// THE PLAN IS PRINTED EITHER WAY, and it is the SAME plan the run uses.
	// A --dry-run that re-derived it separately would be a second
	// implementation that can disagree with the real one about what it was
	// going to do.
	printAtlassianPlan(stdout, plan, orgID, sites, cfg.Prefix(), *decommission)
	if *dryRun {
		fmt.Fprintln(stdout,
			"\n-dry-run: nothing was created, minted, licensed or deleted.")
		return nil
	}
	if plan.Empty() && !*decommission {
		return nil
	}

	// THE SINK IS OPENED FOR A REAL RUN ONLY. A dry run mints nothing, so
	// it has nothing to record — and opening a sink is not free: the
	// -env-file one CREATES the file at 0600, and the -secret-store one
	// probes the store's lock and may reach a running node's API. A
	// command that promised to touch nothing must not do any of that.
	sink, closeSink, err := sinks.open(ctx, stdout)
	if err != nil {
		return err
	}
	defer closeSink()

	key := strings.TrimSpace(*adminToken)
	if key == "" {
		key = operatorCredential("ATLASSIAN_ORG_API_KEY", "ATLASSIAN_ADMIN_TOKEN")
	}
	if key == "" {
		return errors.New(
			"no organization API key: pass -admin-token or export " +
				"ATLASSIAN_ORG_API_KEY. Create it at admin.atlassian.com under " +
				"Settings → API keys, WITHOUT scopes — the service-account admin " +
				"API refuses a scoped key with 403 whatever scopes it holds. The " +
				"seats' own credentials are what this run MINTS, so it cannot " +
				"bootstrap itself from them")
	}
	admin, err := atlassian.NewAdminClient(atlassian.AdminOptions{Key: key})
	if err != nil {
		return err
	}

	// THE SITE URL IS DISCOVERED ONLY WHEN NOTHING NAMED ONE. It is the
	// base every settings link in the permission report is built from, and
	// a report that names a container an operator has to go and find by
	// hand is most of the way to useless. One request, and only for the
	// company that set the field nowhere — asking every run would spend it
	// to learn something three config fields already say.
	//
	// A FAILURE HERE IS A NOTE, NOT A REFUSAL: the run's real work does not
	// need this, and an organization with two sites legitimately has no
	// single answer.
	siteURL := atlassianSiteURL(&company.Integrations, env)
	if siteURL == "" {
		site, siteErr := admin.DiscoverSite(ctx, orgID)
		switch {
		case siteErr == nil:
			siteURL = site.URL
		default:
			fmt.Fprintf(stdout,
				"No site_url is set and it could not be discovered (%v), so the "+
					"access report below names each project and space without a "+
					"link to its settings. Set integrations.atlassian.site_url.\n",
				siteErr)
		}
	}

	containers := map[atlassian.Product][]string{}
	for _, product := range products {
		containers[product] = atlassian.ContainersOf(organization, product)
	}

	res, err := atlassian.Reconcile(ctx, atlassian.Options{
		Admin: admin, OrgID: orgID, Plan: plan, Sink: sink,
		SiteOf:            sites,
		Containers:        containers,
		SiteURL:           siteURL,
		DisplayNamePrefix: cfg.Prefix(),
		TokenLifetime:     time.Duration(cfg.ExpiryDays()) * 24 * time.Hour,
		Rotate:            *rotate, Decommission: *decommission,
		Only: handles,
	})
	if res != nil {
		printAtlassianResult(stdout, res, sink)
	}
	return err
}

// atlassianSites is the cloud id each product's site is reached by.
//
// # Per product, because an organization may run them on two sites
//
// One site is the ordinary arrangement, which is why a company that has it
// declares the same cloud id twice and nothing here complains. What this
// shape buys is the company that does not: a licence is granted on a SITE, so
// reading the id from the product's own block is the difference between
// licensing an agent where it works and licensing it somewhere it will never
// look.
//
// A DATA CENTER address is refused here rather than at the first 404. It is
// the one input that decides whether this command can do anything at all, and
// finding out from a refusal half way through a company leaves an operator
// working out which seats landed.
func atlassianSites(in *config.Integrations, env *config.Resolver) (map[atlassian.Product]string, error) {
	sites := map[atlassian.Product]string{}
	type block struct {
		product atlassian.Product
		field   string
		url     string
		cloudID string
	}
	var blocks []block
	if j := in.Jira; j != nil {
		blocks = append(blocks, block{atlassian.ProductJira, "jira",
			strings.TrimSpace(env.Value(j.URL)), strings.TrimSpace(env.Value(j.CloudID))})
	}
	if c := in.Confluence; c != nil {
		blocks = append(blocks, block{atlassian.ProductConfluence, "confluence",
			strings.TrimSpace(env.Value(c.URL)), strings.TrimSpace(env.Value(c.CloudID))})
	}
	for _, b := range blocks {
		switch {
		case b.cloudID != "":
			sites[b.product] = b.cloudID
		case b.url != "" && !atlassian.IsCloud(b.url):
			return nil, fmt.Errorf(
				"atlassian: integrations.%s.url is %q, which is a Data Center "+
					"instance. Service accounts are a Cloud-only capability: "+
					"there is no organization admin API on Data Center, and a "+
					"personal access token can only be minted for the calling "+
					"user. Use `crewlet jira provision`, which reports which "+
					"account each seat's own credential authenticates as and "+
					"registers the inbound webhook", b.field, b.url)
		default:
			// NEITHER, which is what an unresolved `${VAR}` looks like: the
			// validator already refuses a block naming its instance
			// nowhere, so a value that arrives empty here is a variable
			// this run could not resolve.
			return nil, fmt.Errorf(
				"atlassian: integrations.%s names no cloud_id that resolved to "+
					"anything, so there is no site to license a seat into — a "+
					"Cloud site's id is at %s/_edge/tenant_info, and on the "+
					"organization's page at admin.atlassian.com",
				b.field, strings.TrimRight(atlassianSiteURL(in, env), "/"))
		}
	}
	if len(sites) == 0 {
		return nil, errors.New(
			"atlassian: the company configures neither integrations.jira nor " +
				"integrations.confluence, so there is no product to license a " +
				"seat for")
	}
	return sites, nil
}

// atlassianSiteURL is the human-readable base the permission report's
// settings links are built from.
//
// The organization block first, then whichever product block declares one:
// they are all the same site in the ordinary company, and an operator who
// wrote it once should not have to write it three times. Empty prints the
// container without a link, which is honest — the API gateway is not a place
// a browser can go, and a link built from it looks right and opens nothing.
func atlassianSiteURL(in *config.Integrations, env *config.Resolver) string {
	candidates := []string{}
	if a := in.Atlassian; a != nil {
		candidates = append(candidates, a.SiteURL)
	}
	if j := in.Jira; j != nil {
		candidates = append(candidates, j.SiteURL, j.URL)
	}
	if c := in.Confluence; c != nil {
		candidates = append(candidates, c.SiteURL, c.URL)
	}
	for _, candidate := range candidates {
		if value := strings.TrimSpace(env.Value(candidate)); value != "" &&
			!strings.Contains(value, "api.atlassian.com") {
			return strings.TrimRight(value, "/")
		}
	}
	return ""
}

// printAtlassianPlan renders what a run intends to do.
// narrowPlan applies -handles to the plan the report prints, and names any
// handle that matched no seat.
//
// A mistyped handle used to print the whole plan and then a result with no
// Created, Rotated or Kept line at all, and exit 0 — which reads as a healthy
// no-op run on a seat that is still holding its old credential.
func narrowPlan(plan *atlassian.Plan, only []string) []string {
	if len(only) == 0 {
		return nil
	}
	want := make(map[string]bool, len(only))
	for _, handle := range only {
		want[handle] = true
	}
	kept := plan.Seats[:0]
	for _, seat := range plan.Seats {
		if want[seat.Handle] {
			delete(want, seat.Handle)
			kept = append(kept, seat)
		}
	}
	plan.Seats = kept

	var notes []string
	for _, handle := range only {
		if want[handle] {
			notes = append(notes, fmt.Sprintf(
				"-handles named %q, which is not a seat this run can provision — "+
					"check the spelling against the handles above, and that the "+
					"seat names an Atlassian credential in mcp_env", handle))
		}
	}
	return notes
}

func printAtlassianPlan(w io.Writer, plan *atlassian.Plan, orgID string, sites map[atlassian.Product]string, prefix string, sweeping bool) {
	fmt.Fprintf(w, "Atlassian organization %s.\n", orgID)
	for _, product := range atlassian.Products {
		if id := sites[product]; id != "" {
			fmt.Fprintf(w, "  %-12s site %s\n", product.Label(), id)
		}
	}
	if plan.Empty() {
		fmt.Fprintln(w,
			"\nNo seat references an Atlassian credential as a whole ${VAR} "+
				"under mcp_env."+strings.Join(atlassian.SeatEnvs, "/")+", so "+
				"there is nothing to provision.")
	} else {
		fmt.Fprintf(w, "\n%d seat(s) to provision:\n", len(plan.Seats))
		for _, seat := range plan.Seats {
			fmt.Fprintf(w, "  %-16s %-24s %s\n", seat.Handle,
				strings.Join(productLabels(seat.Products), "+"),
				strings.Join(append(append([]string(nil), seat.EmailVars...),
					seat.TokenVars...), ", "))
		}
	}
	if sweeping {
		// THE KEEP-SET, because -decommission is the one irreversible verb
		// here and the plan above does not describe it. Managed is every
		// agent seat the CHART has, which is deliberately wider than the
		// seats this run provisions: an account whose name is NOT on this
		// list and does start with the company's prefix is one the sweep
		// will delete, and Atlassian has no restore.
		fmt.Fprintf(w, "\n-decommission will KEEP the %d account name(s) this "+
			"chart still has, and DELETE every other service account whose "+
			"display name starts with %q — Atlassian has no restore:\n",
			len(plan.Managed), prefix)
		for _, name := range plan.Managed {
			fmt.Fprintf(w, "  %s\n", name)
		}
	}
	printNotes(w, plan.Notes)
}

// printAtlassianResult renders what a run did.
//
// THE ACCESS TABLE COMES LAST but before the notes, because it is the part an
// operator ACTS on: a licensed agent that cannot browse its project looks
// perfectly provisioned from every other line in this report.
func printAtlassianResult(w io.Writer, res *atlassian.Result, sink provision.TokenSink) {
	fmt.Fprintf(w, "\nRecorded in %s.\n", sink.Describe())
	printNextStep(w, res.Recorded, sink)
	if len(res.Created) > 0 {
		fmt.Fprintf(w, "Created %d service account(s): %s\n",
			len(res.Created), strings.Join(res.Created, ", "))
	}
	if len(res.Adopted) > 0 {
		fmt.Fprintf(w, "Matched %d seat(s) to an account the organization "+
			"already had: %s\n", len(res.Adopted), strings.Join(res.Adopted, ", "))
	}
	if len(res.Rotated) > 0 {
		fmt.Fprintf(w, "Minted a credential for %d seat(s): %s\n",
			len(res.Rotated), strings.Join(res.Rotated, ", "))
	}
	if res.Retired > 0 {
		fmt.Fprintf(w, "Revoked %d superseded credential(s).\n", res.Retired)
	}
	printKept(w, res.Kept)
	// LICENCES ARE BILLABLE, so a run that bought some says how many and
	// for whom. A report that mentioned only accounts would leave the
	// invoice as the first place anyone found out.
	for _, handle := range sortedKeys(res.Licensed) {
		fmt.Fprintf(w, "  %-16s licensed for %s\n", handle,
			strings.Join(res.Licensed[handle], ", "))
	}
	if len(res.Decommissioned) > 0 {
		fmt.Fprintf(w, "Deleted %d account(s) whose seats have left: %s\n",
			len(res.Decommissioned), strings.Join(res.Decommissioned, ", "))
	}
	printAtlassianAccess(w, res.Access)
	printNotes(w, res.Notes)
}

// printAtlassianAccess renders what each agent can actually do.
//
// # The clean containers are counted, not listed
//
// A twenty-seat company works in a handful of projects, so a line per
// (seat, container) is a page of "ok" an operator scrolls past — and the four
// lines that matter go past with it. What is printed in full is every
// container that is missing something, holds something forbidden, could not
// be read, or is still being applied.
func printAtlassianAccess(w io.Writer, reports []atlassian.AccessReport) {
	if len(reports) == 0 {
		return
	}
	var problems []atlassian.AccessReport
	var ok int
	for _, report := range reports {
		if report.OK() {
			ok++
			continue
		}
		problems = append(problems, report)
	}
	fmt.Fprintf(w, "\nAccess, as each agent's own credential reports it "+
		"(%d of %d container(s) fully granted):\n", ok, len(reports))
	if len(problems) == 0 {
		fmt.Fprintln(w, "  every agent holds exactly the permissions the "+
			"Crewlet agent contract asks for.")
		return
	}
	for _, report := range problems {
		fmt.Fprintf(w, "  %-16s %-11s %-10s %s\n", report.Handle,
			report.Product.Label(), report.Container, atlassianVerdict(report))
		if report.SettingsURL != "" {
			fmt.Fprintf(w, "  %-16s %-11s %-10s change it at %s (%s)\n",
				"", "", "", report.SettingsURL, report.SettingsStyle)
		}
	}
	fmt.Fprintln(w,
		"\n  Crewlet cannot place an agent in a project or a space — Atlassian "+
			"refuses that to an API token, and only a Forge app on a paid plan "+
			"may do it. What is missing above is granted by a person, on the "+
			"screens named.")
}

// atlassianVerdict is one container's finding, in one line.
func atlassianVerdict(report atlassian.AccessReport) string {
	switch {
	case report.Reason != "":
		return "COULD NOT CHECK — " + report.Reason
	case report.Pending:
		// NOT a fault. Atlassian applies a product licence asynchronously
		// and has taken minutes over it, so a licence granted moments ago
		// legitimately has not landed. The next run reports it as an error
		// if it really is one.
		return "STILL STARTING — its licence was granted just now and " +
			"Atlassian applies one asynchronously; re-run in a few minutes"
	case len(report.Missing) > 0 && len(report.Excess) > 0:
		return "missing " + strings.Join(report.Missing, ", ") +
			"; and holds " + strings.Join(report.Excess, ", ") +
			" which Crewlet did not grant and cannot revoke"
	case len(report.Missing) > 0:
		return "missing " + strings.Join(report.Missing, ", ")
	default:
		return "holds " + strings.Join(report.Excess, ", ") +
			" — beyond the agent contract, granted by this site's own " +
			"permission scheme, and only you can revoke it"
	}
}

func productLabels(products []atlassian.Product) []string {
	out := make([]string, 0, len(products))
	for _, product := range products {
		out = append(out, product.Label())
	}
	return out
}
