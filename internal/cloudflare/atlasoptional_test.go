package cloudflare_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// optionalCanary is planted in every field of an optional listing the read
// must never decode.
const optionalCanary = "atlas-canary-optional-field"

// undecodedOptional is what a row of an optional listing carries beside its
// name or id: each field holds value, so a decoder that starts mapping one
// puts value in the read.
func undecodedOptional(value string) map[string]any {
	return map[string]any{
		"deployment_configs": map[string]any{"production": map[string]any{
			"env_vars": map[string]any{"API_TOKEN": map[string]any{"type": "plain_text", "value": value}},
		}},
		"build_config":      map[string]any{"build_command": value},
		"source":            map[string]any{"config": map[string]any{"owner": value, "repo_name": value}},
		"latest_deployment": map[string]any{"url": "https://" + value + ".pages.dev", "env_vars": map[string]any{"K": value}},
		"subdomain":         value,
		"domains":           []any{value},
		"routes":            []any{map[string]any{"id": value, "pattern": value}},
		"tail_consumers":    []any{map[string]any{"service": value}},
		"handlers":          []any{value},
		"etag":              value,
		"tag":               value,
		"tags":              []any{value},
		"connections":       []any{map[string]any{"origin_ip": value, "client_id": value}},
		"metadata":          map[string]any{"note": value},
		"account_tag":       value,
		"credentials_file":  map[string]any{"TunnelSecret": value},
		"location":          value,
		"creation_date":     value,
	}
}

func withUndecoded(row map[string]any) map[string]any {
	maps.Copy(row, undecodedOptional(optionalCanary))
	return row
}

// optionalFake is the atlas's account with one of everything the optional
// permissions show, every row carrying the canary in the fields the read
// never decodes, and a deleted tunnel Cloudflare still lists.
func optionalFake() *fakeCloudflare {
	f := atlasFake()
	f.workers = []map[string]any{withUndecoded(map[string]any{"id": "edge-api"})}
	f.pages = []map[string]any{withUndecoded(map[string]any{"name": "docs"})}
	f.buckets = []map[string]any{withUndecoded(map[string]any{"name": "assets"})}
	f.tunnels = []map[string]any{
		withUndecoded(map[string]any{"id": "f70ff985-a4ef-4643-bbbc-4a0ed4fc8415", "name": "office"}),
		withUndecoded(map[string]any{"id": "0a1b2c3d-a4ef-4643-bbbc-4a0ed4fc8415", "name": "gone",
			"deleted_at": "2026-10-01T00:00:00Z"}),
	}
	return f
}

func viewOf(read collect.Read) atlas.View {
	return atlas.Join(atlas.Fold(read.Claims, atlasNow), atlas.Joined{}, atlasNow)
}

// decided is the standing a read leaves over what the log already holds.
func decided(live []atlas.Claim, read collect.Read, at time.Time) collect.Plan {
	return collect.Decide(live, read, collect.Status{Kind: integration.KindCloudflare}, at)
}

// WHERE THE TOKEN HOLDS THE OPTIONAL READ GROUPS, THE ATLAS READS WORKERS,
// PAGES, R2 AND TUNNELS, EACH BY NAME: a script, a project and a tunnel are
// workloads that run on the account, a bucket belongs to it, the account is
// said to do compute and object storage, a deleted tunnel is not listed, and
// nothing beside a name or an id reaches the read. Every call is a GET, and
// none asks for a script's content, settings or secrets, a project's
// deployments, a tunnel's token or configuration, or an object.
//
// Mutation: decode a Pages project's `deployment_configs` or a Worker's
// routes into a claim, and the canary reaches the read.
func TestTheAtlasReadsWorkersPagesBucketsAndTunnelsWhereTheTokenMay(t *testing.T) {
	t.Parallel()
	f := optionalFake()
	read := readAtlas(t, f)
	if len(read.Unread) != 0 || len(read.Withheld) != 0 {
		t.Fatalf("the read left %+v unread and withheld %+v", read.Unread, read.Withheld)
	}
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), optionalCanary) || strings.Contains(string(out), provisioner) {
		at := strings.Index(string(out), optionalCanary)
		t.Fatalf("a field the read never decodes reached it near %q", string(out)[max(0, at-120):max(0, at)])
	}
	for _, request := range f.requests {
		if !strings.HasPrefix(request, "GET ") {
			t.Errorf("the read made %s", request)
		}
		for _, never := range []string{"/content", "/settings", "/secrets", "/bindings", "/deployments", "/token",
			"/configurations", "/connections", "/objects", "/versions"} {
			if strings.Contains(request, never) {
				t.Errorf("the read asked for %s", request)
			}
		}
	}
	if read.Calls != len(f.requests) {
		t.Errorf("the read counted %d calls and made %d", read.Calls, len(f.requests))
	}
	v := viewOf(read)
	account := "cloudflare/" + testAccount
	for key, name := range map[string]string{
		"cloudflare/workers/edge-api": "edge-api", "cloudflare/pages/docs": "docs",
		"cloudflare/tunnels/f70ff985-a4ef-4643-bbbc-4a0ed4fc8415": "office",
	} {
		e := find(t, v, atlas.KindWorkload, key)
		if e.Name != name || !slices.Equal(related(v, e, atlas.RelationRunsOn), []string{account}) {
			t.Errorf("%s: named %q, runs on %v", key, e.Name, related(v, e, atlas.RelationRunsOn))
		}
	}
	bucket := find(t, v, atlas.KindBucket, "cloudflare/r2/assets")
	if bucket.Name != "assets" || !slices.Equal(related(v, bucket, atlas.RelationBelongsTo), []string{account}) {
		t.Errorf("the bucket: %q, belongs to %v", bucket.Name, related(v, bucket, atlas.RelationBelongsTo))
	}
	if _, listed := v.Find(atlas.MustRef(atlas.KindWorkload, "cloudflare/tunnels/0a1b2c3d-a4ef-4643-bbbc-4a0ed4fc8415").ID()); listed {
		t.Error("a deleted tunnel was read")
	}
	if got := factOf(find(t, v, atlas.KindAccount, account), atlas.FactCapability); !slices.Equal(got, []string{"compute", "object_storage"}) {
		t.Errorf("the account does %v", got)
	}
	if plan := decided(read.Claims, read, atlasNow.Add(time.Hour)); len(plan.Records) != 0 || len(plan.Status.Unread) != 0 {
		t.Errorf("an unchanged read recorded %+v and left %+v unread", plan.Records, plan.Status.Unread)
	}
}

// A TOKEN WITHOUT THE OPTIONAL PERMISSIONS IS WHOLE, AND THE REST IS A
// SUGGESTION: the read finishes, every name and certificate is read, the
// source's standing names no refused part, the Test button says the token
// works and adds one fact saying what to add to see more, and the pass
// reports one advisory on a ready integration. A token that holds them is
// told nothing.
//
// Mutation: put the optional groups among the provisioner's, and the Test
// button refuses a token that manages every agent's token correctly.
func TestATokenWithoutTheOptionalPermissionsIsWholeAndSuggested(t *testing.T) {
	t.Parallel()
	with := optionalFake()
	whole := readAtlas(t, with)
	f := optionalFake()
	f.without(cloudflare.AtlasGroups()...)
	read := readAtlas(t, f)
	if len(read.Unread) != 4 {
		t.Fatalf("the read left %d parts unread, want the four optional ones: %+v", len(read.Unread), read.Unread)
	}
	for _, u := range read.Unread {
		if !u.Optional || !strings.HasPrefix(u.Detail, "See more in Atlas: add ") || strings.Contains(u.Detail, "Authentication error") {
			t.Errorf("the unread part %s: optional %v, %q", u.Part, u.Optional, u.Detail)
		}
	}
	v := viewOf(read)
	find(t, v, atlas.KindHostname, "api.example.com")
	if _, listed := v.Find(atlas.MustRef(atlas.KindWorkload, "cloudflare/workers/edge-api").ID()); listed {
		t.Error("a Worker was read without the permission")
	}
	// Nothing was ever read from them, so the standing names none: the
	// source is read, not partly read.
	plan := decided(read.Claims, read, atlasNow.Add(time.Hour))
	if len(plan.Status.Unread) != 0 || plan.Status.UnreadOverflow != 0 || len(plan.Records) != 0 {
		t.Errorf("the standing names %+v and the read recorded %+v", plan.Status.Unread, plan.Records)
	}
	// The Test button: works either way, and only the one without is told.
	without, err := probe(t, f, typed)
	if err != nil || without.Outcome != setup.VerifyWorks || without.Warnings != nil {
		t.Fatalf("the check of a token without them = %+v, %v", without, err)
	}
	i := slices.IndexFunc(without.Facts, func(fact setup.Fact) bool { return fact.Label == "See more in Atlas" })
	if i < 0 {
		t.Fatalf("the check suggests nothing: %+v", without.Facts)
	}
	for _, name := range []string{"Workers Scripts: Read", "Cloudflare Pages: Read", "Workers R2 Storage: Read",
		"Cloudflare Tunnel: Read", "optional"} {
		if !strings.Contains(without.Facts[i].Value, name) {
			t.Errorf("the suggestion does not say %q: %s", name, without.Facts[i].Value)
		}
	}
	held, err := probe(t, with, typed)
	if err != nil || held.Outcome != setup.VerifyWorks ||
		slices.ContainsFunc(held.Facts, func(fact setup.Fact) bool { return fact.Label == "See more in Atlas" }) {
		t.Errorf("the check of a token that holds them = %+v, %v", held, err)
	}
	// The pass: one advisory, and the integration is ready with it.
	findings := func(f *fakeCloudflare) []integration.Finding {
		c, err := cloudflare.NewClient(cloudflare.ClientOptions{AccountID: testAccount, Token: provisioner, HTTP: f.server(t)})
		if err != nil {
			t.Fatal(err)
		}
		w := convergedCloudflare(t, t)
		w.opt.Client = c
		res, err := cloudflare.Reconcile(t.Context(), w.opt)
		if err != nil {
			t.Fatal(err)
		}
		return res.Findings()
	}
	got := findings(f)
	if len(got) != 1 || got[0].Kind != integration.FindingCoveragePartial || got[0].Title != "See more in Atlas" ||
		len(got[0].Subjects) != 4 {
		t.Fatalf("the pass found %+v, want the one suggestion", got)
	}
	if phase, _ := got[0].Kind.Verdict(); phase != integration.PhaseReady {
		t.Errorf("the suggestion is %s, want a ready integration", phase)
	}
	if got := findings(with); len(got) != 0 {
		t.Errorf("a token that holds them is told %+v", got)
	}
	_ = whole
}

// A PERMISSION SOMEBODY TOOK AWAY KEEPS WHAT WAS READ AND SAYS SO: the part
// an earlier read recorded entries from is named on the source's standing,
// nothing of it is missed or retired however long it lasts, and the parts
// the token still reads are read. One the token never held stays unnamed
// beside it.
//
// Mutation: read the refused listing as an empty one, and every Worker is
// retired a day after somebody narrows the token.
func TestAnOptionalPermissionTakenAwayKeepsWhatWasReadAndSaysSo(t *testing.T) {
	t.Parallel()
	before := optionalFake()
	before.without("Cloudflare Tunnel Read")
	whole := readAtlas(t, before)
	f := optionalFake()
	f.without("Cloudflare Tunnel Read", "Workers Scripts Read")
	read := readAtlas(t, f)
	status := collect.Status{Kind: integration.KindCloudflare}
	for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour), atlasNow.Add(60 * time.Hour)} {
		plan := collect.Decide(whole.Claims, read, status, at)
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 {
			t.Fatalf("at %s the read recorded %+v and missed %+v", at, plan.Records, plan.Status.Missing)
		}
		if len(plan.Status.Unread) != 1 || plan.Status.Unread[0].Part != "workers" ||
			!strings.Contains(plan.Status.Unread[0].Detail, "Workers Scripts: Read") {
			t.Fatalf("at %s the standing names %+v, want the Workers alone", at, plan.Status.Unread)
		}
		status = plan.Status
	}
	empty := read
	empty.Unread = nil
	first := decided(whole.Claims, empty, atlasNow.Add(time.Hour))
	if plan := collect.Decide(whole.Claims, empty, first.Status, atlasNow.Add(26*time.Hour)); plan.Status.Retired != 1 {
		t.Errorf("the control retired %d entries, want the Worker", plan.Status.Retired)
	}
}

// AN OPTIONAL LISTING IS READ WHOLE OR THE READ FAILS: Pages at ten a page,
// R2 by its cursor, every page to the last, and a listing Cloudflare fails
// with anything but a refusal fails the read rather than retiring what it
// did not reach.
//
// Mutation: stop the R2 listing at its first page, and the buckets past it
// are retired a day later.
func TestAnOptionalListingIsReadWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	f := optionalFake()
	f.r2PageSize = 2
	for i := range 25 {
		f.pages = append(f.pages, map[string]any{"name": fmt.Sprintf("site-%02d", i)})
	}
	for i := range 5 {
		f.buckets = append(f.buckets, map[string]any{"name": fmt.Sprintf("bucket-%d", i)})
	}
	v := viewOf(readAtlas(t, f))
	projects, buckets := 0, 0
	for _, e := range v.Entries {
		switch {
		case strings.HasPrefix(e.Ref.Key, "cloudflare/pages/"):
			projects++
		case strings.HasPrefix(e.Ref.Key, "cloudflare/r2/"):
			buckets++
		}
	}
	if projects != 26 || buckets != 6 {
		t.Errorf("the paged listings read %d projects and %d buckets, want 26 and 6", projects, buckets)
	}
	for _, route := range []string{"workers", "pages", "r2", "tunnels"} {
		f := optionalFake()
		f.refuse[route] = http.StatusBadGateway
		src, _ := collect.Lookup(integration.KindCloudflare)
		if read, err := src.Collect(t.Context(), atlasInput(t, f)); err == nil || len(read.Claims) != 0 {
			t.Errorf("%s: a listing that failed answered %d claims and %v", route, len(read.Claims), err)
		}
	}
}
