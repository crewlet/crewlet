package cloudflare_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// grantOf is a stored grant of group on zones, lapsing at expires (zero for
// permanent), as the Agents tab's save writes one.
func grantOf(id, group string, expires time.Time, zones ...string) config.SeatGrant {
	return config.SeatGrant{
		ID: id, Permission: group, Scope: zones, ExpiresAt: expires,
		GrantedBy: "Ada", GrantedAt: time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC), Reason: "an origin rule",
	}
}

// holdsOn reports whether a token's policy allows group on exactly the
// resources: the account, or these zone IDs.
func holdsOn(tok fakeToken, group string, zoneIDs ...string) bool {
	want := map[string]any{"com.cloudflare.api.account." + testAccount: "*"}
	if len(zoneIDs) > 0 {
		want = map[string]any{}
		for _, id := range zoneIDs {
			want["com.cloudflare.api.account.zone."+id] = "*"
		}
	}
	for _, p := range tok.Policies {
		if p.Effect != "allow" || len(p.Resources) != len(want) {
			continue
		}
		same := true
		for k := range want {
			if _, ok := p.Resources[k]; !ok {
				same = false
			}
		}
		if same && slices.ContainsFunc(p.PermissionGroups, func(g cloudflare.GroupRef) bool { return g.ID == groupID(group) }) {
			return true
		}
	}
	return false
}

// A GRANT RIDES THE TOKEN BESIDE ITS LEVEL: the SRE on Secure that Cloudflare
// refused an origin rule is granted Origin Write on one zone, and its token
// holds Secure's groups and that one group on that zone, rewritten in place
// so its value stays; a converged pass writes nothing and reads the token back
// as the level and the grant. Mutation: drop the grants from policiesFor and
// the token never holds Origin Write.
func TestAGrantRidesTheTokenBesideItsLevel(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelSecure)
	w.sync()
	id := w.record().Account.ID
	value, _ := w.sink.Held(sealed)

	w.seat().Grants = []config.SeatGrant{grantOf("gaaaaaaaaaa", "Origin Write", time.Time{}, zoneExampleName)}
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("granting reported %+v", report.Findings)
	}
	tok, _ := w.f.token(id)
	if !holdsOn(tok, "Origin Write", zoneExampleID) {
		t.Fatalf("the token's policy is %+v, want Origin Write on example.com", tok.Policies)
	}
	if !holdsOn(tok, "Zone WAF Write", zoneExampleID) || !holdsOn(tok, "Account Rule Lists Write") {
		t.Errorf("the grant took Secure's groups away: %+v", tok.Policies)
	}
	if after, _ := w.sink.Held(sealed); after != value || tok.Value != value {
		t.Error("the grant rolled the token's value; it is rewritten in place")
	}
	lr := w.record().Lanes[seataccess.LaneCloudflare]
	if lr.Observed.Level != config.CloudflareLevelSecure || len(lr.Observed.Grants) != 1 ||
		lr.Observed.Grants[0].Permission != "Origin Write" {
		t.Errorf("observed %q with %+v, want secure with Origin Write", lr.Observed.Level, lr.Observed.Grants)
	}

	writes := len(w.f.wrote())
	if report := w.sync(); len(report.Findings) != 0 {
		t.Errorf("a converged pass reported %+v", report.Findings)
	}
	if got := w.f.wrote(); len(got) != writes {
		t.Errorf("a converged pass wrote %q", got[writes:])
	}
	if len(w.drift) != 0 {
		t.Errorf("a grant read as drift: %+v", w.drift)
	}
}

// A GRANT NEVER OUTLIVES ITS EXPIRY: an expiring grant leaves the token on the
// first pass inside [seataccess.LapseLead] of its expiry, before the instant
// a person was told, and the value stays. Mutation: filter grants by their
// bare expiry and the pass a minute before it keeps Origin Write.
func TestAGrantLeavesTheTokenBeforeItsExpiry(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	expires := w.clock.Add(4 * time.Hour)
	w.seat().Grants = []config.SeatGrant{grantOf("gbbbbbbbbbb", "Origin Read", expires)}
	w.sync()
	id := w.record().Account.ID
	if tok, _ := w.f.token(id); !holdsOn(tok, "Origin Read", zoneExampleID) {
		t.Fatalf("the token's policy is %+v, want Origin Read on the company's zone", tok.Policies)
	}
	value, _ := w.sink.Held(sealed)

	w.clock = expires.Add(-seataccess.LapseLead + time.Second)
	w.sync()
	if tok, _ := w.f.token(id); holdsOn(tok, "Origin Read", zoneExampleID) {
		t.Fatal("the pass inside the lapse lead left Origin Read on the token")
	}
	if after, _ := w.sink.Held(sealed); after != value {
		t.Error("the lapse rolled the token's value")
	}
	if len(w.drift) != 0 {
		t.Errorf("a lapse read as drift: %+v", w.drift)
	}
}

// A GRANT THE PROVISIONER CANNOT MAKE IS REPORTED, NAMING THE GROUP TO ADD,
// and the rest of the token still converges: the provisioner without Origin
// Write leaves the seat at its level with grant_short saying Origin Rules:
// Edit. Mutation: drop the catalogue's held check and the grant is asked of
// Cloudflare, which the engine never relies on.
func TestAGrantTheProvisionerLacksIsReportedNamingTheGroup(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelSecure)
	w.f.without("Origin Write")
	w.seat().Grants = []config.SeatGrant{grantOf("gcccccccccc", "Origin Write", time.Time{})}
	report := w.sync()
	i := slices.IndexFunc(report.Findings, func(f integration.Finding) bool {
		return f.Kind == integration.FindingGrantShort
	})
	if i < 0 {
		t.Fatalf("findings = %+v, want grant_short", report.Findings)
	}
	if f := report.Findings[i]; !slices.Contains(f.Subjects, "Origin Rules: Edit") || f.ActionURL == "" {
		t.Errorf("finding = %+v, want it to name Origin Rules: Edit and where to add it", f)
	}
	tok, _ := w.f.token(w.record().Account.ID)
	if holdsOn(tok, "Origin Write", zoneExampleID) || w.levelOf(tok) != config.CloudflareLevelSecure {
		t.Errorf("token policy %+v, want Secure alone", tok.Policies)
	}
}

// GRANTS THAT MAKE UP A HIGHER LEVEL NEVER FLAP: Operate plus Configure's three
// groups is Configure's policy, read back as Operate and its grants because
// that is what the last sync applied, so the second pass writes nothing.
// Mutation: read the token as the highest level it holds and every pass
// rewrites it.
func TestGrantsThatMakeUpAHigherLevelAreReadAsGranted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelOperate)
	w.seat().Grants = []config.SeatGrant{
		grantOf("gddddddddd1", "Zone Settings Write", time.Time{}),
		grantOf("gddddddddd2", "Response Compression Write", time.Time{}),
		grantOf("gddddddddd3", "Zone Transform Rules Write", time.Time{}),
	}
	w.sync()
	tok, _ := w.f.token(w.record().Account.ID)
	if w.levelOf(tok) != config.CloudflareLevelConfigure {
		t.Fatalf("the token reads as %q, want Configure's exact policy", w.levelOf(tok))
	}
	writes := len(w.f.wrote())
	w.sync()
	if got := w.f.wrote(); len(got) != writes {
		t.Errorf("the second pass wrote %q", got[writes:])
	}
}

// A GRANT TAKEN OFF AT CLOUDFLARE IS PUT BACK AS DRIFT (D2), with the event
// naming the grant as what the token lacked.
func TestAGrantTakenOffAtCloudflareIsRestored(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	w.seat().Grants = []config.SeatGrant{grantOf("geeeeeeeeee", "DNS Write", time.Time{}, zoneExampleName)}
	w.sync()
	id := w.record().Account.ID
	bare, _ := cloudflare.PoliciesFor(testAccount, catalogueOf(w.f), w.f.zones, []string{zoneExampleName},
		config.CloudflareLevelRead)
	w.f.edit(id, func(t *fakeToken) { t.Policies = bare })
	w.sync()
	if tok, _ := w.f.token(id); !holdsOn(tok, "DNS Write", zoneExampleID) {
		t.Fatalf("the grant was not restored: %+v", tok.Policies)
	}
	if len(w.drift) != 1 || len(w.drift[0].Before.Grants) != 0 || len(w.drift[0].After.Grants) != 1 {
		t.Errorf("drift events = %+v, want one putting the grant back", w.drift)
	}
}

// THE CATALOGUE IS WHAT CAN BE MINTED: every group a token can hold at the
// account or a zone, by its builder name where Cloudflare renames it, marked
// write unless it is read-only, with the level that already holds it; never a
// group that manages tokens, members, billing or Access's sign-in; and a group
// the provisioner does not hold is offered unavailable, naming what to add.
func TestTheGrantCatalogueIsWhatTheProvisionerCanMint(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	w.f.without("DNS Write")
	sess, err := w.vendor.Open(t.Context(), seataccess.OpenInput{
		Config: w.company, Env: config.NewResolver(config.MapSource{"CF_TOKEN": provisioner}),
	})
	if err != nil {
		t.Fatal(err)
	}
	opts, err := seataccess.ReadOptions(t.Context(), sess, seataccess.LaneCloudflare)
	if err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{"Account API Tokens Write", "Account API Tokens Read", "Billing Read",
		"Billing Write", "Account Settings Write", "Memberships Write",
		"Access: Organizations, Identity Providers, and Groups Write"} {
		if _, ok := opts.Grantable(never); ok {
			t.Errorf("the catalogue offers %s", never)
		}
	}
	origin, ok := opts.Grantable("Origin Write")
	if !ok || origin.Name != "Origin Rules: Edit" || !origin.Write || origin.ScopeKind != "zone" ||
		origin.Unavailable != "" || origin.InLevel != "" {
		t.Errorf("Origin Write = %+v, want an available zone write named Origin Rules: Edit", origin)
	}
	if read, _ := opts.Grantable("Origin Read"); read.Write {
		t.Error("Origin Read is offered as a write")
	}
	if purge, _ := opts.Grantable("Cache Purge"); !purge.Write || purge.InLevel != config.CloudflareLevelOperate {
		t.Errorf("Cache Purge = %+v, want a write that Operate holds", purge)
	}
	if lists, _ := opts.Grantable("Account Rule Lists Write"); lists.ScopeKind != seataccess.GrantScopeAccount {
		t.Errorf("Account Rule Lists Write = %+v, want an account grant", lists)
	}
	dns, _ := opts.Grantable("DNS Write")
	if dns.Unavailable == "" || !slices.Contains(dns.Missing, "DNS Write") || dns.ActionURL == "" {
		t.Errorf("DNS Write = %+v, want it unavailable naming what to add", dns)
	}
	if !slices.Equal(opts.GrantScope, []string{zoneExampleName}) {
		t.Errorf("grant scope = %q, want the company's one zone", opts.GrantScope)
	}
	if err := seataccess.CheckGrant(opts, config.CloudflareLevelRead,
		grantOf("gffffffffff", "Origin Write", time.Time{}, "example.net")); err == nil ||
		!strings.Contains(err.Error(), "example.net") {
		t.Errorf("a grant on a zone outside the company's was %v, want refused naming it", err)
	}
	if err := seataccess.CheckGrant(opts, config.CloudflareLevelOperate,
		grantOf("gffffffffff", "Cache Purge", time.Time{})); err == nil {
		t.Error("a grant the seat's level already holds was accepted")
	}
}
