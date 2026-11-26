package queries

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/appleads"
	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/metaads"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// The questions answered from the epoch: what the company DECLARES, which is a
// different question from what it has done.

// RecentRunsLimit bounds the schedule history a listing carries.
//
// The usage domain holds every data node's fires for its whole horizon, and
// the screen shows a recent-activity strip beside the schedule table. Fifty is
// a page of it; one schedule's own history is `schedule_runs`.
const RecentRunsLimit = 50

// schedules answers the schedules question: what is configured, and what
// recently fired. The configured schedules are a PROJECTION of the org,
// computed and never stored, so a new one shows at once. The history is the
// usage domain's fires (ADR-0020) from every data node: the dispatch ledger
// records `fired` when the scheduler sends it, before the seat's node decides
// whether a later fire runs in its place. The engine's default cap rides
// beside them, so a form says what an empty time limit means.
func (s Sources) schedules(ctx context.Context, _ Params) (any, error) {
	return map[string]any{
		"schedules":               s.ConfiguredSchedules(),
		"recent_runs":             s.recentRuns(ctx),
		"default_timeout_seconds": int(org.DefaultScheduleTimeout / time.Second),
	}, nil
}

// ConfiguredSchedules is the schedule rows a company's org declares, with no
// store read at all.
//
// Exported because the dashboard's handshake snapshot carries them too, and
// the two must be ONE implementation: the screen renders its rows from the
// snapshot slice and fetches only the dispatch ledger through the question
// above, so a second derivation here would be a screen whose contents changed
// depending on which of the two arrived last.
//
// The ledger half stays out. It is a store read, and the snapshot is built
// without one — see stream.Service.Snapshot.
func (s Sources) ConfiguredSchedules() []schedule.Row {
	// Both callers hold a company source (the API refuses to build without
	// one, and the schedules question is registered only beside it), so
	// the func is never nil. What it returns is, before a node's first
	// revision activates.
	company, roster := s.Company()
	if company == nil {
		return []schedule.Row{}
	}
	// THE COMPANY'S OWN ORG, the one this node built when it applied the
	// epoch, rather than one re-derived from the document per read: both
	// come from ONE read of the epoch, so they never describe two revisions.
	if roster == nil {
		// A node running no company has no seats to describe. Empty is
		// the honest answer and the screen says so.
		return []schedule.Row{}
	}
	rows := schedule.Describe(roster, schedule.DescribeOptions{
		Zone: company.Location(),
		Now:  s.clock(),
	})
	if rows == nil {
		return []schedule.Row{}
	}
	return rows
}

// integrations answers how each external surface is wired, and what has come
// through it.
//
// IT NEVER INFERS HEALTH: an idle Slack and a 401-ing Slack are identical in
// the event store (verification runs before a row is written). It answers
// what is configured, whether this node has the credential, and how many
// deliveries arrived. `routes` is a property of the BUILD: a webhook route
// stores deliveries once its block is present, but waking a seat needs a
// parser, and four apps lack it. THREE-VALUED like secret_present: null is
// "this node cannot say", which is not false.
func (s Sources) integrations(ctx context.Context, _ Params) (any, error) {
	company, roster := s.Company()
	if company == nil {
		return map[string]any{"integrations": []any{}}, nil
	}
	seen := s.deliveryTraffic(ctx)
	in := company.Integrations
	// ONE READ for the whole answer. Every row asks the same question of
	// the same coordination bucket, and a per-row read would put seven
	// round trips on a screen refresh.
	//
	// Nil when this node cannot say, which a coordination read that failed
	// honestly is, and which is NOT the same claim as "nothing has been
	// reconciled". A row then carries a null reconcile rather than one
	// asserting that nobody has ever checked.
	reconciled, reconcileKnown := s.reconcileStates(ctx)

	// Nil when this node cannot say; an empty-but-non-nil list is a real
	// answer meaning nothing routes, so the two must not collapse.
	var routed []string
	known := s.Routed != nil
	if known {
		routed = s.Routed(ctx)
		known = routed != nil
	}
	// The same shape for what could VERIFY a delivery, and read the same
	// way: nil is "cannot say", empty is "nothing here can verify".
	var verifiable []string
	verifiableKnown := s.Verifiable != nil
	if verifiableKnown {
		verifiable = s.Verifiable(ctx)
		verifiableKnown = verifiable != nil
	}
	// And what a route refuses although its material resolved. A nil map
	// reads as nothing held: a hold is stated, never inferred.
	var held map[string]string
	if s.Held != nil {
		held = s.Held(ctx)
	}

	out := []map[string]any{}
	// WHAT THE ROLL-UP READS, one per row, built from the same values the
	// row carries so the tool's state and the row it names cannot disagree.
	surfaces := map[string]integration.SurfaceStatus{}
	// CONFIGURED and ENABLED are different facts and the answer sends both.
	// A block present with `enabled: false` is a deliberate pause an operator
	// can see; an absent block is an integration nobody set up. Folding them
	// left a paused integration looking unconfigured, which is the state most
	// likely to be mistaken for a mistake.
	add := func(kind string, enabled bool, secret *bool, detail map[string]any) {
		row := map[string]any{
			"key":        kind,
			"configured": true,
			"enabled":    enabled,
			// AT THIS ROW'S INGRESS — see [traffic.count]. Zero when
			// nothing was measured, which `traffic_known` beside the
			// rows says.
			"inbound": seen.count[kind],
			// THREE-VALUED like the secret fields, and for the same
			// reason: null means this node could not read the outcome
			// events, and reporting that as 0 would say every delivery
			// woke a seat on a node that cannot tell.
			"skipped":   countOrNil(seen.skipped, kind),
			"coalesced": countOrNil(seen.coalesced, kind),
		}
		// WHERE A DELIVERY ARRIVES, and null where none ever does.
		//
		// The default used to be `webhook` at `/webhooks/<kind>`, which is
		// the route for five of the eight and a fabrication for the rest.
		// Atlassian is the one it fabricated: the organization receives
		// nothing at all, so the row advertised `/webhooks/atlassian` — a
		// path webhooks.go does not register and never has — for an
		// operator to check their settings page against. See
		// [integration.Kind.Ingests].
		row["inbound_kind"], row["inbound_path"] = nil, nil
		if integration.Kind(kind).Ingests() {
			row["inbound_kind"] = map[bool]string{
				true: "websocket", false: "webhook"}[readOverASocket(kind)]
			row["inbound_path"] = inboundPath(kind)
		}
		// Rendered as a relative time, so an absent one has to be absent
		// rather than the zero instant — which would print as 1970.
		//
		// TO THE STORE'S OWN RESOLUTION, like the window it lies in
		// (`traffic_since`): cut to the second, the newest delivery read as
		// arriving before the window that holds it began whenever the two
		// shared a second.
		if at, ok := seen.last[kind]; ok {
			row["last_at"] = at.UTC().Format(time.RFC3339Nano)
		} else {
			row["last_at"] = nil
		}
		var routes, secretUsable, endpointCurrent *bool
		if sources := deliversAs(kind); known && len(sources) > 0 {
			routes = boolPtr(slices.ContainsFunc(sources, func(source string) bool {
				return slices.Contains(routed, source)
			}))
		}
		row["routes"] = routes
		// THREE-VALUED, and the third value is the point: null means this
		// surface uses no secret at all, false means a route is refusing
		// every delivery, and only an operator can tell those apart.
		//
		// CONFIGURED, which is a claim about the DOCUMENT. A secret lives
		// there as a ${VAR}, so this being true says an operator wrote one
		// down — not that the route has anything to verify with.
		row["secret_present"] = secret
		// Which is the question secret_usable answers, from what this
		// process actually RESOLVED. The gap between the two is invisible
		// from every other surface: an unset variable renders as a secret
		// present, the third-party app's settings page shows a healthy hook, and
		// every delivery is refused with nothing anywhere naming the
		// variable. Null when this node cannot say (nothing has resolved
		// yet) or when the surface has no secret to resolve, as above.
		if secret != nil && verifiableKnown {
			secretUsable = boolPtr(slices.Contains(verifiable, kind))
		}
		row["secret_usable"] = secretUsable
		// WHY EVERY DELIVERY IS REFUSED where the secret is not the reason,
		// in the route's own words; null where nothing is held.
		row["held"] = nil
		if reason := held[kind]; reason != "" {
			row["held"] = reason
		}
		// THREE-VALUED again: null means nothing checks this surface from
		// here, an absent entry means the loop has not reached it yet, a
		// present one is a real finding. A ROW IS NOT A REPORT: the setup
		// write stamps an address on a surface no pass converges, leaving a
		// row with no phase, which rendered as a card with no state;
		// [integration.State.Observed] is the test.
		if !reconcileKnown {
			row["reconcile"] = nil
		} else if state, checked := reconciled[kind]; checked && state.Observed() {
			row["reconcile"] = reconcileRow(state)
		} else {
			row["reconcile"] = nil
		}
		// WHETHER THIS SURFACE'S REGISTRATION STILL POINTS HERE: a registration
		// made against an old public base keeps pointing at an address that no
		// longer answers. Where a pass registers the hook the next tick moves
		// it; otherwise it stays false until a person changes it at the
		// third-party app. THREE-VALUED: null is "no address recorded", which a
		// screen must not report as a fault.
		//
		// COMPARED AGAINST [Sources.WebhookBase], never the company document,
		// where a `${VAR}` could sit unresolved and read every company as
		// moved, and never `api.external_url`, which behind a public listener
		// is another socket. A nil accessor says null, not false.
		row["endpoint"] = nil
		if state, checked := reconciled[kind]; checked && state.Endpoint != "" {
			row["endpoint"] = state.Endpoint
			if s.WebhookBase != nil {
				endpointCurrent = boolPtr(state.Endpoint == s.WebhookBase())
			}
		}
		row["endpoint_current"] = endpointCurrent
		status := integration.SurfaceStatus{
			Key: kind, Enabled: enabled, Known: reconcileKnown,
			SecretUsable: secretUsable, Routes: routes, EndpointCurrent: endpointCurrent,
			Held: held[kind],
		}
		if state, checked := reconciled[kind]; reconcileKnown && checked {
			status.State = &state
		}
		if s.Converges != nil {
			status.Converges = boolPtr(s.Converges(integration.Kind(kind)))
		}
		surfaces[kind] = status
		// Every row carries seats, so the view never reads undefined.
		// An empty list is a real answer — nobody holds credentials of
		// their own for this surface — and it is not the same as absent.
		row["seats"] = seatsFor(roster, kind)
		for key, value := range detail {
			row[key] = value
		}
		out = append(out, row)
	}

	// Slack's org block is the TRANSPORT MARKER, so it is what says this
	// surface exists at all; the per-seat apps are what the inbound route
	// verifies with, and the row reports whether any seat holds one.
	//
	// A company with seat apps and NO block is not a half-configured Slack,
	// which is how this read until a disconnect produced one: the engine
	// retires the transport when the block goes (`slack_retired`), so every
	// delivery is verified at the route and then routes to no seat. Reported
	// as a row it was a surface with nothing enabled — which the dashboard
	// draws as Paused, a word for a state an operator chose, over the residue
	// of a disconnect that had removed the block and left the seats.
	if in.Slack != nil {
		add("slack", true, boolPtr(seatSecrets(company, roster, "slack") > 0), nil)
	}
	if in.Mattermost != nil {
		add("mattermost", true, nil, map[string]any{"url": in.Mattermost.URL})
	}
	// GitHub, on the same terms as Slack and for the same reason: the org
	// block is what turns the surface on, and a company whose agents each
	// have their own app still needs one — [Engine.reconcileGitHub] retires
	// the parser on `cfg == nil || !cfg.Enabled`, so without it the seats'
	// apps deliver to a route that verifies them and routes them nowhere.
	//
	// The clause this was widened for — a company holding nothing but
	// per-agent apps, which had no GitHub row at all while its agents were
	// receiving deliveries — is answered by [seatSecrets] instead, which
	// counts a seat app only where the org block enables the surface it
	// arrives on. Widened here as well, the row outlived a disconnect that
	// removed every installation, and the card sat on Paused.
	if in.GitHub != nil {
		add("github", in.GitHub.Enabled,
			boolPtr(seatSecrets(company, roster, "github") > 0 || in.GitHub.WebhookSecret != ""), nil)
	}
	if in.GitLab != nil {
		add("gitlab", in.GitLab.Enabled, boolPtr(in.GitLab.SigningSecret != ""),
			map[string]any{"url": in.GitLab.URL})
	}
	if in.Jira != nil {
		add("jira", true, boolPtr(in.Jira.WebhookSecret != ""),
			map[string]any{"url": in.Jira.BaseURL()})
	}
	if in.Confluence != nil {
		// TWO ROUTES, so the row names both. Data Center signs on
		// /webhooks/confluence; Cloud carries a token on
		// /webhooks/confluence/{event}, one hook per event. secret_present
		// answers for EITHER credential, because the question is whether a
		// delivery from this surface could be verified at all.
		add("confluence", true,
			boolPtr(in.Confluence.WebhookSecret != "" || in.Confluence.WebhookToken != ""),
			map[string]any{
				"url":        in.Confluence.BaseURL(),
				"cloud_path": "/webhooks/confluence/{event}",
			})
	}
	if in.Datadog != nil {
		// The one row whose secret is not a signing key, and the detail
		// says so rather than leaving a reader to assume the header is
		// verified like every other surface's. route_to is reported
		// because it is where an alert naming no owner goes, which is
		// the single most consequential thing about this integration
		// that is invisible from the traffic counts beside it.
		add("datadog", in.Datadog.Enabled, boolPtr(in.Datadog.WebhookToken != ""),
			map[string]any{
				"handle_tag": in.Datadog.HandleTagOrDefault(),
				"route_to":   in.Datadog.RouteTo,
			})
	}
	if in.Cloudflare != nil {
		// PROVISIONING ALONE, like the organization below: nothing arrives
		// from Cloudflare in this build, so there is no delivery secret.
		detail := map[string]any{}
		if p := in.Cloudflare.Provisioning; p != nil {
			detail["account_id"], detail["zones"] = p.AccountID, p.Zones
		}
		add("cloudflare", in.Cloudflare.Enabled, nil, detail)
	}
	if in.Scaleway != nil {
		// PROVISIONING ALONE, like Cloudflare's: nothing arrives from
		// Scaleway, so there is no delivery secret. The organization is
		// what the row names.
		var org string
		if in.Scaleway.Provisioning != nil {
			org = in.Scaleway.Provisioning.OrganizationID
		}
		add("scaleway", in.Scaleway.Enabled, nil, map[string]any{"organization_id": org})
	}
	if in.Expo != nil {
		// PROVISIONING ONLY, like atlassian below: the provisioner token
		// creates agents' robots and no delivery arrives, so there is no
		// delivery secret to report. The account is what the row names.
		var account string
		if in.Expo.Provisioning != nil {
			account = in.Expo.Provisioning.AccountName
		}
		add("expo", in.Expo.Enabled, nil, map[string]any{"account_name": account})
	}
	if in.DigitalOcean != nil {
		// PROVISIONING ONLY, like expo above: a person pastes each agent's
		// token, which the engine checks and seals, and no delivery arrives,
		// so there is no delivery secret to report. The team is what the row
		// names, as written in the document.
		var team string
		if in.DigitalOcean.Provisioning != nil {
			team = in.DigitalOcean.Provisioning.Team
		}
		add("digitalocean", in.DigitalOcean.Enabled, nil, map[string]any{"team": team})
	}
	if in.AWS != nil {
		// NOTHING INBOUND: the row names the stack, by its short name and
		// home Region, which every name in the account carries.
		var slug, region string
		if in.AWS.Provisioning != nil {
			slug, region = in.AWS.Provisioning.Slug, in.AWS.Provisioning.HomeRegion
		}
		add("aws", in.AWS.Enabled, nil, map[string]any{"slug": slug, "home_region": region})
	}
	if in.GoogleCloud.Started() {
		// PROVISIONING ALONE, like Expo's: nothing arrives from Google, so
		// there is no delivery secret. The identity project, which holds
		// every agent's account, is what the row names.
		var project string
		if in.GoogleCloud.Provisioning != nil {
			project = in.GoogleCloud.Provisioning.IdentityProject
		}
		add("google_cloud", in.GoogleCloud.Enabled, nil, map[string]any{"identity_project": project})
	}
	if in.SSH != nil {
		// NOTHING INBOUND: the engine dials out to each registered server,
		// and the row says how many there are.
		add("ssh", in.SSH.Enabled, nil, map[string]any{"hosts": len(in.SSH.Hosts)})
	}
	if m := in.MetaAds; m != nil {
		// THE APP AND THE TOKEN SLOTS FILLED, never a token: what each
		// permission a token may hold allows.
		var tokens []string
		for _, t := range []struct{ name, ref string }{{"read", m.ReadToken}, {"operate", m.OperateToken},
			{"admin", m.AdminToken}} {
			if t.ref != "" {
				tokens = append(tokens, t.name)
			}
		}
		add("meta_ads", m.Enabled, nil, map[string]any{"app_id": m.AppID, "business_id": m.BusinessID,
			"tokens": tokens, "ad_accounts": len(m.AdAccounts), "permissions": metaads.Catalog().Permissions})
	}
	if a := in.AppStoreConnect; a != nil {
		// THE TEAMS AND THE ROLES: each connection's issuer, the apps this node
		// last read an agent's key list (never asked of Apple here), and what
		// each Apple role allows.
		var held appstoreconnect.Reach
		if s.AppStoreConnectReach != nil {
			held = s.AppStoreConnectReach()
		}
		add("app_store_connect", a.Enabled, nil, map[string]any{
			"connections": appstoreconnect.Connections(a, held),
			"roles":       appstoreconnect.Catalog().RoleOffers()})
	}
	if a := in.AppleAds; a != nil {
		// THE API USERS AND THE ROLES, never a key: each connection's client ID
		// and organization, and what each Apple Ads role allows.
		var users []map[string]any
		for _, id := range a.ConnectionIDs() {
			c := a.Connections[id]
			users = append(users, map[string]any{"id": id, "identity": c.ClientID, "org_id": c.OrgID})
		}
		add("apple_ads", a.Enabled, nil, map[string]any{"connections": users, "ad_accounts": len(a.AdAccounts),
			"roles": appleads.Catalog().RoleOffers()})
	}
	if in.GoogleAnalytics != nil {
		// NO SECRET: the properties the company narrows agents to, and its
		// catalog's roles with what each allows.
		add("google_analytics", in.GoogleAnalytics.Enabled, nil, map[string]any{
			"properties": in.GoogleAnalytics.Properties, "roles": growthRoles("google_analytics")})
	}
	if in.AdMob != nil {
		// NO SECRET: the OAuth client's public ID, the accounts the company
		// narrows agents to, and its catalog's roles with what each allows.
		var client string
		if in.AdMob.OAuth != nil {
			client = in.AdMob.OAuth.ClientID
		}
		add("admob", in.AdMob.Enabled, nil, map[string]any{"client_id": client, "accounts": in.AdMob.Accounts,
			"roles": growthRoles("admob")})
	}
	if in.AdSense != nil {
		// AdMob's shape: the client's public ID, the accounts, and the roles.
		var client string
		if in.AdSense.OAuth != nil {
			client = in.AdSense.OAuth.ClientID
		}
		add("adsense", in.AdSense.Enabled, nil, map[string]any{"client_id": client,
			"accounts": in.AdSense.Accounts, "roles": growthRoles("adsense")})
	}
	if in.GoogleAds != nil {
		// NO SECRET: the accounts agents may be given, which carry caps, and
		// its catalog's access levels with what each allows.
		capped := []string{}
		if in.GoogleAds.Spend != nil {
			for _, c := range in.GoogleAds.Spend.Accounts {
				capped = append(capped, c.Account)
			}
		}
		customers := []string{}
		for _, c := range in.GoogleAds.Customers {
			customers = append(customers, c.ID)
		}
		add("google_ads", in.GoogleAds.Enabled, nil, map[string]any{
			"customers": customers, "capped": capped, "roles": growthRoles("google_ads")})
	}
	if in.SearchConsole != nil {
		// NO SECRET: the sites the company narrows agents to, and its
		// catalog's roles with what each allows.
		add("search_console", in.SearchConsole.Enabled, nil, map[string]any{
			"sites": in.SearchConsole.Sites, "roles": growthRoles("search_console")})
	}
	if in.GooglePlay != nil {
		// NO SECRET: the apps, developer accounts and reports buckets the
		// company names, and its catalog's permissions with what each allows.
		add("google_play", in.GooglePlay.Enabled, nil, map[string]any{
			"apps": nonNil(in.GooglePlay.Apps), "developer_accounts": nonNil(in.GooglePlay.DeveloperAccounts),
			"reports_buckets": nonNil(in.GooglePlay.ReportsBuckets), "roles": growthRoles("google_play")})
	}
	if in.Atlassian != nil {
		// THE ORGANIZATION, not a third product: where the account itself is
		// created, which no site API can do. It reconciles identities,
		// ingests nothing, and has no inbound address, but needs a status row
		// like any surface. PRESENCE IS THE CONFIGURATION: the block has no
		// `enabled` switch. NO DELIVERY SECRET: the organization key is a
		// PROVISIONING credential, and `secret` means the value a delivery is
		// verified with; passed as the key, the row drew "secret unresolved"
		// and "routes nowhere" over a working organization. Mattermost, the
		// other surface with no inbound address, passes nil.
		add("atlassian", true, nil,
			map[string]any{
				"deployment": in.Atlassian.DeploymentOrDefault(),
				"org_id":     in.Atlassian.OrgID,
			})
	}
	if in.ForgeAppID != "" {
		// The app id is the JWT AUDIENCE rather than a secret — it is in
		// every manifest the operator installs — so its presence is the
		// whole configuration and there is no separate credential.
		add("forge", true, nil, map[string]any{"app_id": in.ForgeAppID})
	}
	slices.SortFunc(out, func(a, b map[string]any) int {
		return cmp.Compare(a["key"].(string), b["key"].(string))
	})
	// ONE ROLL-UP PER TOOL, decided here rather than on the client: which
	// surface's word wins, and when a ready phase still needs a person, are
	// rules — and a second copy of them in the dashboard is how a card came
	// to disagree with the surface it named. Every tool is present, a tool
	// nobody configured included, so a reader never has to invent a state
	// for a missing one.
	tools := make([]integration.ToolRollup, 0, len(integration.Tools))
	for _, tool := range integration.Tools {
		var present []integration.SurfaceStatus
		for _, surface := range tool.Surfaces {
			if status, ok := surfaces[surface.Key]; ok {
				present = append(present, status)
			}
		}
		tools = append(tools, integration.Rollup(tool, present))
	}
	body := map[string]any{
		"integrations": out,
		"tools":        tools,
		// Whether the counts above are a MEASUREMENT. Without this a
		// store that could not be read reports every integration at zero
		// inbound, which reads as "nothing is arriving" — the alarming
		// answer — when the truth is "nobody looked".
		"traffic_known": seen.known,
		// WHERE THE WINDOW EVERY COUNT COVERS STARTS — inbound, skipped and
		// coalesced alike — which ends at the instant the answer was read.
		// The deliveries are a capped page, so a count means something only
		// beside the window it covers; see [deliveryWindow]. Null only when
		// no store could be read (`traffic_known` false): every listing
		// that answered names a window, an empty one included.
		"traffic_since": nil,
		// WHICH NODES THE COUNTS WERE READ FROM, or null when no store
		// could be read — the same case `traffic_known` false reports.
		"coverage": seen.coverage,
	}
	if seen.known {
		// TO THE STORE'S OWN RESOLUTION, not the second: the window
		// starts at the history floor under the read's own instant, or
		// one microsecond past a capped page's oldest delivery, and a
		// second-truncated edge would name a window up to a second wider
		// than the one counted.
		body["traffic_since"] = seen.since.UTC().Format(time.RFC3339Nano)
	}
	return body, nil
}

// deliversAs names the sources a verified delivery at this surface arrives
// under. For six of the nine rows it is the surface itself, and the other
// three are the whole reason it exists rather than a `slices.Contains`.
//
// The question `routes` answers is whether a verified delivery would WAKE A
// SEAT, and only a registered parser can. A surface is therefore asked about
// the source its deliveries are published as, which is not always its own
// name.
func deliversAs(kind string) []string {
	switch {
	case kind == "forge":
		// AN INGRESS PATH, NOT A SOURCE. A Cloud event relayed by the
		// Forge app is republished as the PRODUCT it belongs to — jira or
		// confluence, from forgeEvents — and parsed by that product's
		// parser. So no parser is ever registered under "forge", and the
		// relay answered `routes: false` on every Cloud deployment for
		// ever. The dashboard groups it under the Atlassian row, so a
		// tenant whose relay was feeding both products correctly carried a
		// permanent "Forge relay — routes nowhere" beside them.
		return []string{"jira", "confluence"}
	case !integration.Kind(kind).Ingests():
		// Nothing arrives, so there is nothing to route and `false` —
		// "verified deliveries reach nobody" — is a fault report about a
		// question this surface is not asked. See [integration.Kind.Ingests].
		return nil
	default:
		return []string{kind}
	}
}

// inboundPath is where a third-party app's deliveries arrive, so an operator can check
// what they pasted into the third-party app's settings page against what this engine
// actually serves. Static per integration: these are the routes webhooks.go
// registers, and a disagreement between the two is a route nothing reaches.
//
// Asked only of a surface that INGESTS, which is what lets the default stand:
// a kind reaching it has a `/webhooks/<kind>` route unless it is named above.
func inboundPath(kind string) string {
	switch kind {
	case "mattermost":
		return "" // one outbound websocket per seat; nothing arrives here
	case "slack":
		return "/webhooks/slack/{handle}"
	case "github":
		// Both forms are served. The seat one is what a per-agent app
		// points at, and naming it here is what tells an operator the
		// path is meant to carry a handle.
		return "/webhooks/github/{handle}"
	case "forge":
		return "/webhooks/forge"
	default:
		return "/webhooks/" + kind
	}
}

// traffic is what the event store can say about inbound deliveries, and about
// what became of them, over ONE window.
//
// Grouped BY THE STORE rather than by a live counter: a live counter resets
// with the process, and the question an operator asks — "is anything arriving
// from GitLab" — is about recent history rather than about this pod's uptime.
//
// `known` is the load-bearing field. A store that cannot be read and a store
// with nothing in it are opposite facts, and a count of 0 expresses both — so
// the counts are reported ONLY alongside a flag saying they were measured.
type traffic struct {
	known bool

	// count and last are per INGRESS — the route a delivery arrived at,
	// which is the row's own `inbound_path` — and not per integration. The
	// two differ on the Forge relay alone: it hands Jira and Confluence
	// events on under its own token, and they are filed under the product
	// they belong to. Counted by that product, the relay's row read 0 and
	// "never" on every Cloud tenant whose relay was carrying everything;
	// counted under both, the Atlassian card — which sums its surfaces —
	// would count every relayed event twice. By ingress, each delivery is
	// counted once, at the place it arrived, and the card's sum is exact.
	count map[string]int
	last  map[string]time.Time

	// skipped and coalesced are what BECAME of the deliveries, counted
	// from the engine's own events rather than from the inbound rows.
	//
	// The three numbers answer one question together and are useless
	// apart: "128 arrived, 30 the routing gate dropped, 2 merges" is a
	// working integration; "128 arrived" alone cannot tell that from an
	// integration whose every delivery reaches nobody. Both are counted
	// per INTEGRATION, from the notification_source the events carry, so
	// they line up with the inbound count beside them — and over the SAME
	// WINDOW, `[since, at)`, so they line up in time as well.
	skipped   map[string]int
	coalesced map[string]int

	// since is where the window every count covers starts; it ends at the
	// instant the deliveries were read at. See [deliveryWindow] for where,
	// and why. Zero only when nothing was measured.
	since time.Time

	// coverage is which nodes the counts were read from. A delivery is
	// written to the store of the node the load balancer handed it to, so
	// a count from one node is a third of a three-node fleet's traffic —
	// and a node that did not answer is traffic the counts cannot see.
	coverage *eventfan.Coverage
}

func (s Sources) deliveryTraffic(ctx context.Context) traffic {
	if s.Events == nil {
		return traffic{}
	}
	listing, coverage, err := s.Events.List(ctx, store.ListQuery{
		Category: events.WebhookCategory, Limit: MaxEventPage,
	})
	if err != nil {
		log.WarnContext(ctx, "integration_counts_unreadable", "error", err)
		return traffic{}
	}
	at := listing.At
	out := traffic{
		known: true, count: map[string]int{}, last: map[string]time.Time{},
		skipped: map[string]int{}, coalesced: map[string]int{},
		since: deliveryWindow(listing), coverage: &coverage,
	}
	for _, row := range listing.Rows {
		// ONLY THE WINDOW'S: a delivery at the oldest instant a capped
		// page reached may have siblings past the page, and one written
		// after the read was asked is past the window's top — which the
		// outcome counts beside it stop at.
		if row.Time.Before(out.since) || !row.Time.Before(at) {
			continue
		}
		// The ROUTE the delivery arrived at, never its source: the Forge
		// relay files the Jira and Confluence events it carries under
		// their product, and counted there the relay itself would read 0.
		ingress := row.Tags["route"]
		out.count[ingress]++
		if row.Time.After(out.last[ingress]) {
			out.last[ingress] = row.Time
		}
	}
	s.countOutcomes(ctx, &out, at)
	return out
}

// deliveryWindow is where the window a page of deliveries covers starts. Its
// top is the instant the page was read at, [eventfan.Listing.At].
// The outcome counts are ASKED over exactly this window.

// THE PAGE IS CAPPED, NOT TIME-BOUNDED (the newest [MaxEventPage] across the
// fleet), so naming the window is what makes a count a measurement. Which
// window turns on [eventfan.Listing.More], never the page's length:
//   - nothing past the page: it holds every delivery the 30-day history keeps
//     on every node that answered, so the window is the WHOLE HISTORY from
//     the floor under the read's instant, even for zero rows. Started at the
//     oldest delivery, it would cut away what the outcome counts had;
//   - deliveries past it: the window is what lies AFTER the oldest instant
//     reached ([eventfan.MergeListing]). A delivery sharing the last row's
//     instant may lie past the cut, so it starts one store tick (a
//     microsecond) after, and those rows are not counted rather than short.
func deliveryWindow(listing eventfan.Listing) time.Time {
	at := listing.At
	if !listing.More {
		return at.Add(-store.EventHistory)
	}
	var oldest time.Time
	for _, row := range listing.Rows {
		if oldest.IsZero() || row.Time.Before(oldest) {
			oldest = row.Time
		}
	}
	// A PAGE SAYING MORE LIES PAST ROWS IT DOES NOT HOLD names nothing it
	// covers, and nor does one whose every row was written after the read:
	// the window is then the empty one at the read's own instant, rather
	// than one starting past its own top.
	if oldest.IsZero() || !oldest.Before(at) {
		return at
	}
	return oldest.Add(time.Microsecond)
}

// countOutcomes adds the two per-integration outcome counts, over the window
// the deliveries named: `[out.since, at)`.
//
// A QUESTION OF THE FLEET'S OWN ([eventfan.Fleet.NotificationOutcomes]): every
// node counts its outcome events in that window and the counts are summed,
// since outcomes are engine events and deliveries edge rows, so no one
// listing holds both. Its failure leaves the counts absent, not zero (see
// [traffic]): a zero meaning "unreadable" is the number an operator acts on.
// `at` ([eventfan.Listing.At]) is the instant every node floors the history
// at, one reading of the fleet's clock for the whole row.
func (s Sources) countOutcomes(ctx context.Context, out *traffic, at time.Time) {
	counted, coverage, err := s.Events.NotificationOutcomes(ctx,
		store.OutcomeQuery{Since: out.since, At: at})
	if err != nil {
		log.WarnContext(ctx, "integration_outcomes_unreadable", "error", err)
		out.skipped, out.coalesced = nil, nil
		return
	}
	merged := out.coverage.And(coverage)
	out.coverage = &merged
	out.skipped, out.coalesced = counted.Skipped, counted.Coalesced
}

// seatsFor lists the handles that carry this surface in their own config:
// LISTED, not counted, since "which seats reach GitLab" is followed at once by
// "which ones". A seat carries a surface by its OWN field for it (a Slack
// app, a Mattermost identity, a per-seat project or space); one with none is
// served by the company-wide account.
//
// THE WALK IS THE COMPANY'S OWN ORG, not the document's `roles:`, which holds
// only the seats of NO unit and so answered an empty list for a company whose
// agents all sit in units.
func seatsFor(roster *org.Organization, kind string) []string {
	out := []string{}
	if roster == nil {
		return out
	}
	for r := range roster.AllRoles() {
		var carries bool
		switch kind {
		case "slack":
			carries = !r.Slack.IsZero()
		case "mattermost":
			carries = !r.Mattermost.IsZero()
		case "jira":
			// A seat's own PROJECT, which is vendor-neutral now: the same
			// key names a Worklet project and a Jira one, and this answer
			// is about which seats carry an identity of their own rather
			// than about which product serves it.
			carries = r.Project != ""
		case "confluence":
			// The seat's own SPACE, vendor-neutral for the same reason
			// the project is: one key names a Doclet container and a
			// Confluence one.
			carries = r.Space != ""
		case "github":
			carries = r.GitHub != nil
		}
		// THE HANDLE, as this list has always promised, never the
		// name: a name is prose two seats may share, so a list of names
		// could not say WHICH two seats carried their own Slack app, and
		// a reader could not follow one to its page.
		if carries {
			out = append(out, r.Handle())
		}
	}
	slices.Sort(out)
	return out
}

// seatSecrets counts the per-seat apps of one kind that carry a verification
// credential AND can do anything with it. Counted as well as listed by
// [seatsFor]: the COUNT says whether the route can verify anything at all.
//
// A sealed value is not a surface: each arm below adds the thing that makes
// the secret MEAN something, because a disconnect removed the meaningful half
// and left the secret, so the row outlived the integration. A count is a
// claim that deliveries reach a seat, not that a `${VAR}` is written down.
// What a half-finished teardown leaves is reported by the SETUP screen's seat
// roster and by the disconnect itself.
func seatSecrets(company *config.Company, roster *org.Organization, kind string) int {
	n := 0
	if company == nil || roster == nil {
		return n
	}
	for r := range roster.AllRoles() {
		var secret string
		switch kind {
		case "slack":
			// AND THE TRANSPORT MARKER. Declaring `integrations.slack` at
			// all is what turns Slack on; without it the engine retires
			// the transport (`slack_retired`) and unregisters the parser,
			// so a delivery to a seat's app is verified at the route and
			// then turned into work for nobody.
			//
			// Counted without it, a disconnect that dropped the block and
			// left the seats kept the surface alive on two sealed values.
			if !r.Slack.IsZero() && company.Integrations.Slack != nil {
				secret = r.Slack.SigningSecret
			}
		case "github":
			// AN INSTALLATION, and the organization block that enables
			// the surface the delivery arrives on. An app not installed on the
			// organization sees no repository, mints no token and receives no
			// delivery, so its signing secret alone reported a live surface
			// (a disconnect left the row Connecting with `routes nowhere`).
			// The block is the other half: [Engine.reconcileGitHub] retires the
			// parser on `cfg == nil || !cfg.Enabled`, so an installed app with
			// no enabled block delivers to a route that routes nowhere.
			block := company.Integrations.GitHub
			if app := r.GitHub; app != nil && app.InstallationID != 0 &&
				block != nil && block.Enabled {
				secret = app.WebhookSecret
			}
		}
		if secret != "" {
			n++
		}
	}
	return n
}

func boolPtr(v bool) *bool { return &v }

// agentSeatID is the agent id of the AGENT seat a handle names — any handle it
// answers to — and false for a human seat, a handle nobody answers to, and a
// node with no active company. It never hands the handle back: a caller
// comparing the answer against ids must not be given a name to compare.
func (s Sources) agentSeatID(handle string) (string, bool) {
	organization := s.organization()
	if organization == nil {
		return "", false
	}
	id, ok := organization.AgentIDFor(organization.AgentSeatByHandle(handle))
	if !ok {
		return "", false
	}
	return id.String(), true
}

// memorySeat is the AGENT seat a handle names, as the memory reader is handed
// it: its agent id, which its lease, diary and onboarding are keyed by, and its
// handle, which its episodes, skills, counterparty profiles and conversation
// ledger are filed under.
//
// False for a human seat — a person keeps no memory here — for a handle no
// seat answers to, and on a node with no company to resolve against.
func memorySeat(organization *org.Organization, handle string) (memread.Seat, bool) {
	if organization == nil {
		return memread.Seat{}, false
	}
	role := organization.AgentSeatByHandle(handle)
	id, ok := organization.AgentIDFor(role)
	if !ok {
		return memread.Seat{}, false
	}
	return memread.Seat{ID: id, Handle: role.Handle()}, true
}

// trailSeat resolves the seat a question about a SEAT'S TRAIL — what it
// remembers, what it said — is asked about, and decides the caller may read
// it. An absent name is the caller's own seat; an unbound caller has no trail
// ([errNoSeat], whose remedy is a binding). The name is resolved by
// [iam.OwnerOf] first, through [Sources.recordHandle], so a LOGIN is never
// read as a handle. The authority is [authz.ActionSeatTrailRead], the one verb
// every trail question asks (the seat, its lead or an admin, ADR-0033),
// registered as [DecidedBy]: this is the ONLY place the decision is made.
func (s Sources) trailSeat(ctx context.Context, asked string) (memread.Seat, error) {

	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return memread.Seat{}, unresolved(ctx, "seat trail")
	}
	if asked == "" {
		if principal.Seat == "" {
			return memread.Seat{}, errNoSeat
		}
		asked = principal.Seat
	}
	handle, err := s.recordHandle(ctx, authz.ActionSeatTrailRead, asked)
	if err != nil {
		return memread.Seat{}, err
	}
	seat, ok := memorySeat(s.organization(), handle)
	if !ok {
		return memread.Seat{}, fmt.Errorf("%w: no agent seat answers to %q. "+
			"A seat's trail is an agent's, and a person keeps none here",
			ErrNotFound, asked)
	}
	return seat, nil
}

// agentMemory answers a seat's memory: its diary, its episodes, the skills it
// drafted, what it learned about the people it works with, and whether it
// onboarded — each with its total — ANSWERED BY THE SEAT'S HOLDER, which the
// answer names (`held_by`). See [Sources.Memory] for why.
//
// `id` is the seat's handle, or a person's login; `limit` pages
// every collection (at most [memread.PageLimit]); a profile's summary asks for
// one, since the totals and the latest reflection travel whatever the page.
// `skills_order` is the order the skills are read in before their page is
// cut ([learning.SkillOrders], newest first when absent), so a screen sorting
// them by use is handed the most used rather than sorting the newest.
func (s Sources) agentMemory(ctx context.Context, p Params) (any, error) {
	id := strings.TrimSpace(p.String("id"))
	if id == "" {
		return nil, fmt.Errorf("%w: agent_memory needs an id", ErrBadParams)
	}
	order := learning.SkillOrder(strings.TrimSpace(p.String("skills_order")))
	if !order.Valid() {
		return nil, fmt.Errorf("%w: agent_memory reads a seat's skills in one of %s, "+
			"not %q", ErrBadParams, words.Or(learning.SkillOrders()), order)
	}
	seat, err := s.trailSeat(ctx, id)
	if err != nil {
		return nil, err
	}
	memory, err := s.Memory.Memory(ctx, seat,
		memread.MemoryQuery{Limit: p.Int("limit", 0), Skills: order})
	if err != nil {
		return nil, err
	}
	return memory, nil
}

// agentEpisode answers one of a seat's episodes WHOLE — what it was asked and
// what it did, complete — where `agent_memory` lists each as its opening.
// ANSWERED BY THE SEAT'S HOLDER, as `agent_memory` is and for its reason; an
// episode the seat no longer holds (the lifecycle dropped or folded it since it
// was listed) is `episode: null`, an ordinary absence.
func (s Sources) agentEpisode(ctx context.Context, p Params) (any, error) {
	id := strings.TrimSpace(p.String("id"))
	episode := strings.TrimSpace(p.String("episode"))
	if id == "" || episode == "" {
		return nil, fmt.Errorf("%w: agent_episode needs the seat's id and an episode", ErrBadParams)
	}
	// THE SAME SEAT, BY THE SAME RULE, as the listing it opens one row of:
	// resolved through [Sources.trailSeat] and decided by
	// [authz.ActionSeatTrailRead], so an episode is never readable by
	// somebody its seat's memory is refused to.
	seat, err := s.trailSeat(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.Memory.Episode(ctx, seat, episode)
}

// memoryOverview answers every agent seat's memory totals (diary, episodes,
// drafted skills) and newest reflection, counted by the node holding the seat,
// with the `coverage` naming every holder asked.
// EVERY AGENT SEAT IN THE CHART, in handle order, uncapped: a cut list hid
// the seats a reader looked for. A seat no node holds is still a row
// (`held_by: none`), since a missing seat reads as one that does not exist.
//
// DECIDED PER SEAT by [authz.ActionSeatTrailRead]: every seat for an admin, a
// lead's own agents, an empty list (not a refusal) for anybody else; decided
// once on no seat, the owner-or-lead rule refuses everybody. A SEAT THIS NODE
// CANNOT DECIDE MAKES THE WHOLE ANSWER UNAVAILABLE: a short list reads as
// memory gone.
func (s Sources) memoryOverview(ctx context.Context, _ Params) (any, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return nil, unresolved(ctx, "memory_overview")
	}
	organization := s.organization()
	var seats []memread.Seat
	if organization != nil {
		for role := range organization.AllRoles() {
			seat, ok := memorySeat(organization, role.Handle())
			if !ok {
				continue
			}
			d := authz.Decide(ctx, principal, authz.ActionSeatTrailRead,
				authz.Object{Kind: authz.KindPerson, ID: seat.Handle, Owner: seat.Handle},
				s.Chart, s.clock())
			switch {
			case d.Unknown():
				return nil, fmt.Errorf("%w: this node cannot yet decide whose "+
					"memory the caller may read (%s on %s): %w", ErrUnavailable,
					authz.ActionSeatTrailRead, seat.Handle, d.Err)
			case d.Allowed:
				seats = append(seats, seat)
			}
		}
	}
	slices.SortFunc(seats, func(a, b memread.Seat) int { return cmp.Compare(a.Handle, b.Handle) })
	return s.Memory.Overview(ctx, seats)
}

// readOverASocket reports whether a surface's arrivals come over a websocket the
// engine holds open — Mattermost's, one per seat — rather than to a route.
func readOverASocket(kind string) bool { return kind == "mattermost" }

// countOrNil renders an outcome count, or null when nothing was counted.
func countOrNil(counts map[string]int, kind string) any {
	if counts == nil {
		return nil
	}
	return counts[kind]
}

// reconcileStates reads what the loop last found, keyed by surface.
//
// The second result is whether this node could say at all, kept apart from an
// empty map for the same reason [Sources.Routed] keeps them apart: a
// coordination read that failed has nothing to report, and rendering that as
// "no surface has been reconciled" would put an alarming claim on a screen over
// a store that was briefly unreachable.
func (s Sources) reconcileStates(ctx context.Context) (map[string]integration.State, bool) {
	return keyedStates(ctx, s.Reconciles)
}

// reconcileStatuses is [Sources.reconcileStates] without the findings
// ([Sources.ReconcileStatuses]).
func (s Sources) reconcileStatuses(ctx context.Context) (map[string]integration.State, bool) {
	return keyedStates(ctx, s.ReconcileStatuses)
}

// keyedStates is what read answers keyed by surface, and false where it cannot
// say (nil, or answering nil).
func keyedStates(ctx context.Context,
	read func(context.Context) []integration.State) (map[string]integration.State, bool) {

	if read == nil {
		return nil, false
	}
	states := read(ctx)
	if states == nil {
		return nil, false
	}
	out := make(map[string]integration.State, len(states))
	for _, state := range states {
		out[state.Kind.String()] = state
	}
	return out, true
}

// reconcileRow renders one surface's status for the wire.
//
// The FINDINGS travel as well as the report, because the two answer different
// questions: the report says what to do next, and the findings say what is
// actually wrong. A company with a broken webhook and four under-granted
// seats reports the webhook, and an operator who fixes it should not have to
// wait a full pass to discover there were four more things behind it.
func reconcileRow(state integration.State) map[string]any {
	// THE REPORT A READER SHOULD SEE, which is the stored one except in the
	// window between somebody pressing Disconnect and the first teardown
	// pass: the stored phase is then still whatever the last reconcile
	// concluded, and showing it reports a connected integration somebody
	// has already asked to remove.
	report := state.Reported()
	row := map[string]any{
		"phase": string(report.Phase),
		// The phase in a reader's words, decided HERE rather than on the
		// client. A screen that mapped six phase values to five labels
		// would be a second place that has to know what they mean, and it
		// could not label a phase a newer node wrote at all.
		"phase_label": report.Phase.Label(),
		"actor":       string(report.Actor),
		"detail":      report.Detail,
		"action_url":  report.ActionURL,
		// The INTENT, separately from the phase it produces. A reader
		// needs to know a disconnect was asked for even on a build whose
		// phase vocabulary it does not share.
		"disconnecting": state.TearingDown(),
		"outcome":       string(state.Outcome),
		"attempts":      state.Attempts,
		"last_error":    state.LastError,
		// WHAT A FAILED DISCONNECT WAS HELD BY (`vendor` or `settings`), ""
		// where none failed, so a screen names the real cause.
		"teardown_held_by": string(state.TeardownHeldBy),
		"findings":         reconcileFindings(state.Findings),
		// WHAT THE FINDINGS RECORD LEFT OUT to fit its bound, each finding
		// counted once, so a list cut short says so: findings dropped from
		// its tail, an agent's own it kept without their words (each agent's
		// row draws those), and the others it kept without theirs. Zero
		// where it kept everything.
		"findings_omitted":      state.FindingsRecord.Omitted,
		"seat_findings_omitted": state.FindingsRecord.SeatOmitted,
		"words_omitted":         state.FindingsRecord.WordsOmitted,
		// TRUE WHERE THE FINDINGS COULD NOT BE READ beside the status (a pass
		// wrote between the two reads, or their record is gone): the list is
		// then empty and says nothing, until the next read.
		"findings_unread": state.FindingsUnread,
	}
	// Rendered as instants, so an absent one is absent rather than the
	// zero time, which prints as 1970 and reads as a real answer.
	row["last_attempt_at"] = instantOrNil(state.LastAttemptAt)
	row["settled_at"] = instantOrNil(state.SettledAt)
	row["next_attempt_at"] = instantOrNil(state.NextAttemptAt)
	return row
}

// reconcileFindings renders the findings list, never nil so a view does not
// have to read undefined.
func reconcileFindings(findings []integration.Finding) []map[string]any {
	out := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		// THE VERDICT TRAVELS WITH THE FINDING, from
		// [integration.FindingKind.Verdict], the one table that says what a
		// kind MEANS. A reader with only the kind string must keep a second
		// copy of the closed set; the dashboard did, and badged a healthy
		// agent amber for a spare permission (grant_excess and
		// registration_orphaned are PhaseReady). A kind this build does not
		// know still gets a verdict from Verdict's default arm: a newer peer
		// may write one, and an advisory would hide a real problem.
		phase, actor := f.Kind.Verdict()
		row := map[string]any{
			"kind":       string(f.Kind),
			"subject":    f.Subject,
			"detail":     f.Detail,
			"action_url": f.ActionURL,
			"phase":      string(phase),
			"actor":      string(actor),
		}
		// WHAT TO DO, SEPARATE FROM WHAT IS WRONG, so a card can lay the
		// two out rather than render one paragraph carrying both. Absent
		// where a surface honestly has no instruction to give.
		if f.Remedy != "" {
			row["remedy"] = f.Remedy
		}
		// THE LEAD AN ADVISORY CARRIES, which the notes list sets in bold
		// ahead of the sentence. Absent on a finding that has none.
		if f.Title != "" {
			row["title"] = f.Title
		}
		// THE FINDING IN A FEW WORDS, on every one, for a list that gives a
		// finding one line (Ask Crewlet's): the title, or its kind's words.
		row["label"] = f.Label()
		// AN AGENT'S OWN, which its row on the Agents tab draws from its own
		// record, so the integration's list leaves it there.
		if f.Seat {
			row["seat"] = true
		}
		// THE WHOLE LIST, where a finding is about many things and its own
		// sentence gives only the count. See [integration.Finding.Subjects]:
		// the detail is the card's status line and is capped, so a finding
		// that listed thirty-six addresses inline was a wall cut off
		// mid-address. Absent rather than an empty array for the ordinary
		// finding about one thing, so a view can ask whether there is a
		// list at all.
		if len(f.Subjects) > 0 {
			row["subjects"] = f.Subjects
		}
		// HOW MANY MORE IT IS ABOUT than the list its record kept names.
		// Absent where the list is whole.
		if f.SubjectsOmitted > 0 {
			row["subjects_omitted"] = f.SubjectsOmitted
		}
		// WHEN IT LAPSES, as an instant a reader can count down to, on the
		// one kind that has a date. Absent everywhere else.
		if !f.ExpiresAt.IsZero() {
			row["expires_at"] = f.ExpiresAt.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}

// instantOrNil renders a timestamp, or null for one that was never set.
func instantOrNil(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at.UTC().Format(time.RFC3339)
}

// growthRoles is kind's catalog's roles, each with what it allows, for the
// setup card ([growth.Catalog.RoleOffers]); nil for a kind with no catalog.
func growthRoles(kind string) []growth.RoleOffer {
	c, ok := growth.CatalogOf(kind)
	if !ok {
		return nil
	}
	return c.RoleOffers()
}
