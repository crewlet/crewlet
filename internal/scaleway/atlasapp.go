package scaleway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/words"
)

// The atlas's own IAM application (ADR-0144): one application named for the
// atlas, one policy of read-only permission sets on every project, and one
// API key, made by the provisioner when a company switches
// `integrations.scaleway.atlas` on, and deleted when it switches it off.
//
// NO SEAT IS BOUND TO IT AND NO FIELD NAMES IT: the key is sealed under fixed
// names only this package reads, so what the atlas reads shows at Scaleway as
// the atlas's, and deleting the application there revokes the atlas alone.

// The sealed names the atlas application is kept under: its API key's secret,
// the access key that names that key, and the application's own ID, which is
// what proves an application at Scaleway is the one this engine made.
const (
	AtlasSecretName      = "SCALEWAY_ATLAS_SECRET_KEY"
	AtlasAccessName      = "SCALEWAY_ATLAS_ACCESS_KEY"
	AtlasApplicationName = "SCALEWAY_ATLAS_APPLICATION_ID"
)

// AtlasSealedNames is every sealed name of the atlas application.
func AtlasSealedNames() []string {
	return []string{AtlasAccessName, AtlasApplicationName, AtlasSecretName}
}

// atlasSubject is what a finding about the atlas application is filed under:
// the setting that makes it.
const atlasSubject = "integrations.scaleway.atlas"

// atlasKeyDescription is the description every key the engine mints on the
// atlas application carries, which tells its own keys from any a person added.
const atlasKeyDescription = "crewlet atlas"

// atlasDescription is what the engine writes on the application and its policy.
const atlasDescription = "Crewlet Atlas, read-only. Managed by Crewlet: switch it off on the Scaleway connection."

// atlasSet is one permission set the atlas application holds, and the part of
// the read it is for ([atlasParts]).
type atlasSet struct{ name, part string }

// atlasSets is the atlas application's whole grant: one read-only set per
// product the atlas draws, each named, never AllProductsReadOnly. Bucket
// configuration and names, never object contents.
//
//nolint:gochecknoglobals // an immutable table, not state
var atlasSets = []atlasSet{
	{"InstancesReadOnly", partInstances},
	{"KubernetesReadOnly", partKubernetes},
	{"RelationalDatabasesReadOnly", partDatabases},
	{"RedisReadOnly", partRedis},
	{"LoadBalancersReadOnly", partLoadBalancers},
	{"ObjectStorageBucketsRead", partBuckets},
	{"DomainsDNSReadOnly", partDNS},
	{"ContainersReadOnly", partContainers},
	{"FunctionsReadOnly", partFunctions},
}

// AtlasSets is the permission sets the atlas application's policy grants on
// every project, sorted.
func AtlasSets() []string {
	out := make([]string, 0, len(atlasSets))
	for _, s := range atlasSets {
		out = append(out, s.name)
	}
	slices.Sort(out)
	return out
}

// atlasSetOf is the permission set a part of the read needs.
func atlasSetOf(part string) string {
	for _, s := range atlasSets {
		if s.part == part {
			return s.name
		}
	}
	return ""
}

// AtlasName is the name the atlas application and its policy are created
// under, so a person reading the Organization's IAM sees whose it is. It
// labels them and never identifies them.
func AtlasName(company string) string {
	if slug := org.Slugify(company); slug != "" {
		return "crewlet-atlas-" + slug
	}
	return "crewlet-atlas"
}

// AtlasOptions is what one convergence of the atlas application needs.
type AtlasOptions struct {
	// Client carries the provisioner key, and Company the company's name.
	Client  *Client
	Company string

	// Want is the company's setting: on makes and keeps the application, off
	// deletes it.
	Want bool

	// ApplicationID and AccessKey are what is sealed for the application, ""
	// where nothing is, and SecretHeld whether its key's secret is sealed
	// too. The secret itself is never handed to a pass.
	ApplicationID string
	AccessKey     string
	SecretHeld    bool

	// Sink seals what is minted and forgets what is deleted. NIL IS A DRY
	// RUN: nothing is written at Scaleway, and what a pass would do is said.
	Sink provision.TokenSink

	// Now is the clock a key's expiry is judged by; nil is the wall clock.
	Now func() time.Time
}

// ReconcileAtlas brings the atlas application to the company's setting and
// reports what a person may want to know, every finding an advisory: the
// atlas is optional reading, so nothing here degrades the integration.
//
// AN APPLICATION SOMEBODY DELETED AT SCALEWAY IS NOT MADE AGAIN: that is a
// person revoking the atlas there, which no pass undoes unasked. Its policy
// and its key are the engine's to keep, and are written again where missing.
func ReconcileAtlas(ctx context.Context, opts AtlasOptions) ([]integration.Finding, error) {
	if opts.Client == nil {
		return nil, errors.New("scaleway: no client")
	}
	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now()
	}
	a := atlasApp{client: opts.Client, sink: opts.Sink, name: AtlasName(opts.Company), now: now}
	if !opts.Want {
		return nil, a.remove(ctx, strings.TrimSpace(opts.ApplicationID), strings.TrimSpace(opts.AccessKey))
	}
	access := strings.TrimSpace(opts.AccessKey)
	if !opts.SecretHeld {
		// AN ACCESS KEY WITH NO SECRET BESIDE IT names a key nothing can use.
		access = ""
	}
	return a.converge(ctx, strings.TrimSpace(opts.ApplicationID), access)
}

// RemoveAtlas deletes the atlas application the engine recorded, with its
// keys and its policies, for a disconnect. One already gone is the state
// asked for. It forgets nothing: the caller owns the sealed names.
func RemoveAtlas(ctx context.Context, client *Client, applicationID string) error {
	if client == nil {
		return errors.New("scaleway: no client")
	}
	a := atlasApp{client: client}
	return a.delete(ctx, strings.TrimSpace(applicationID))
}

// atlasApp is one convergence of the atlas application.
type atlasApp struct {
	client *Client
	sink   provision.TokenSink
	name   string
	now    time.Time
}

// advisory is a finding about the atlas application, which never degrades
// the integration: the atlas reads less, and everything else works.
func advisory(title, detail, remedy, url string) integration.Finding {
	return integration.Finding{
		Kind: integration.FindingCoveragePartial, Subject: atlasSubject,
		Title: title, Detail: detail, Remedy: remedy, ActionURL: url,
	}
}

// remove deletes the application and forgets its sealed names. A dry run
// deletes nothing.
func (a atlasApp) remove(ctx context.Context, appID, access string) error {
	if a.sink == nil || (appID == "" && access == "") {
		return nil
	}
	if err := a.delete(ctx, appID); err != nil {
		return err
	}
	if err := a.sink.Forget(ctx, AtlasSealedNames()...); err != nil {
		return fmt.Errorf("scaleway: forget the atlas application's sealed key: %w", err)
	}
	return nil
}

// delete deletes every key on the application, every policy whose principal
// it is, and the application. EVERY KEY AND POLICY, a stranger's included:
// the application is the engine's and is going, and nothing outlives it.
func (a atlasApp) delete(ctx context.Context, appID string) error {
	if appID == "" {
		return nil
	}
	app, found, err := a.client.Application(ctx, appID)
	if err != nil {
		return fmt.Errorf("scaleway: read the atlas application: %w", reject(err))
	}
	if !found {
		return nil
	}
	keys, err := a.client.APIKeysOf(ctx, app.ID)
	if err != nil {
		return fmt.Errorf("scaleway: read the atlas application's keys: %w", reject(err))
	}
	for _, k := range keys {
		if err = a.client.DeleteAPIKey(ctx, k.AccessKey); err != nil {
			return fmt.Errorf("scaleway: delete the atlas application's key: %w", reject(err))
		}
	}
	policies, err := a.client.PoliciesOf(ctx, app.ID)
	if err != nil {
		return fmt.Errorf("scaleway: read the atlas application's policies: %w", reject(err))
	}
	for _, p := range policies {
		if err = a.client.DeletePolicy(ctx, p.ID); err != nil {
			return fmt.Errorf("scaleway: delete the atlas application's policy: %w", reject(err))
		}
	}
	if err = a.client.DeleteApplication(ctx, app.ID); err != nil {
		return fmt.Errorf("scaleway: delete the atlas application: %w", reject(err))
	}
	return nil
}

// converge makes the application where none is recorded, then holds its
// policy to the read-only sets and its key to one that works.
func (a atlasApp) converge(ctx context.Context, appID, access string) ([]integration.Finding, error) {
	catalogue, err := a.client.PermissionSets(ctx)
	if err != nil {
		return nil, fmt.Errorf("scaleway: read the Organization's permission sets: %w", reject(err))
	}
	sets, unlisted := atlasGrantable(catalogue)
	var out []integration.Finding
	if len(unlisted) > 0 {
		out = append(out, advisory("Atlas reads less at Scaleway",
			"Scaleway lists no permission set named "+words.And(unlisted)+" for projects, so the Atlas "+
				"application's policy leaves "+pronoun(len(unlisted))+" out and Atlas does not read what "+
				pronoun(len(unlisted))+" would show",
			"Check the Organization can use the products these permission sets belong to", PoliciesURL))
	}
	if len(sets) == 0 {
		return out, nil
	}
	rules := []RuleSpec{{PermissionSetNames: sets, OrganizationID: a.client.OrganizationID()}}
	if appID == "" {
		if a.sink == nil {
			return append(out, a.pending()), nil
		}
		if appID, err = a.create(ctx, rules); err != nil {
			return out, err
		}
	}
	app, found, err := a.client.Application(ctx, appID)
	if err != nil {
		return out, fmt.Errorf("scaleway: read the atlas application: %w", reject(err))
	}
	if !found {
		return append(out, advisory("Atlas application deleted at Scaleway",
			"The IAM application Crewlet made for Atlas is gone at Scaleway, so Atlas reads the "+
				"Organization's projects and nothing inside them",
			"To make it again, switch \"See more in Atlas\" off on the Scaleway connection, then on",
			ApplicationsURL)), nil
	}
	strangers, err := a.policy(ctx, app, rules)
	if err != nil {
		return out, err
	}
	if len(strangers) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantExcess, Subject: atlasSubject,
			Title: "Policies Crewlet did not write on the Atlas application",
			Detail: "Scaleway holds " + integration.Count(len(strangers), "policy") + " on the Atlas " +
				"application that Crewlet did not write, so its key may read more than Atlas needs",
			Remedy:    "Review them at Scaleway and delete the ones it should not hold. Crewlet never deletes a policy it did not write",
			ActionURL: PoliciesURL, Subjects: strangers,
		})
	}
	extra, finding, err := a.key(ctx, app, access)
	if err != nil {
		return out, err
	}
	if finding != nil {
		out = append(out, *finding)
	}
	if len(extra) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantExcess, Subject: atlasSubject,
			Title: "API keys beside the Atlas application's own",
			Detail: "Scaleway holds " + integration.Count(len(extra), "API key") + " on the Atlas application " +
				"besides the one Crewlet sealed, and each works until it expires",
			Remedy:    "Delete the ones nobody uses at Scaleway",
			ActionURL: APIKeysURL, Subjects: extra,
		})
	}
	return out, nil
}

// pending is what a dry run says of an application a pass would make.
func (a atlasApp) pending() integration.Finding {
	return advisory("Atlas application not made yet",
		"The next check of the Scaleway connection makes the read-only IAM application "+a.name+
			", its policy and its API key",
		"Nothing to do: it is made within a few minutes", "")
}

// atlasGrantable splits the atlas sets into the ones the Organization's
// catalogue lists for projects, sorted, and the ones it does not.
func atlasGrantable(catalogue []PermissionSet) (sets, unlisted []string) {
	scope := map[string]string{}
	for _, s := range catalogue {
		scope[s.Name] = s.ScopeType
	}
	for _, name := range AtlasSets() {
		if scope[name] == "projects" {
			sets = append(sets, name)
		} else {
			unlisted = append(unlisted, name)
		}
	}
	return sets, unlisted
}

// create makes the application with its policy and seals its ID. A policy
// Scaleway refuses, or an ID that cannot be sealed, takes the application
// back with it, so none is left that nothing records.
func (a atlasApp) create(ctx context.Context, rules []RuleSpec) (string, error) {
	app, err := a.client.CreateApplication(ctx, a.name, atlasDescription)
	if err != nil {
		return "", fmt.Errorf("scaleway: create the atlas application: %w", reject(err))
	}
	undo := func(cause error) error {
		return errors.Join(cause, a.delete(context.WithoutCancel(ctx), app.ID))
	}
	if !uuidPattern().MatchString(app.ID) {
		return "", undo(errors.New("scaleway: created the atlas application and Scaleway returned no ID for it"))
	}
	if _, err := a.client.CreatePolicy(ctx, PolicySpec{
		Name: a.name, Description: atlasDescription, ApplicationID: app.ID, Rules: rules,
	}); err != nil {
		return "", undo(fmt.Errorf("scaleway: create the atlas application's policy: %w", reject(err)))
	}
	if err := a.sink.Record(ctx, AtlasApplicationName, app.ID); err != nil {
		return "", undo(fmt.Errorf("scaleway: seal the atlas application's ID: %w", err))
	}
	return app.ID, nil
}

// policy holds the engine's policy on the application to rules, and answers
// the policies on it somebody else wrote. The engine's is the one under the
// name it writes, or under the application's own name where the company was
// renamed since; a second such policy, which only a lost answer leaves, is
// deleted.
func (a atlasApp) policy(ctx context.Context, app Application, rules []RuleSpec) ([]string, error) {
	held, err := a.client.PoliciesOf(ctx, app.ID)
	if err != nil {
		return nil, fmt.Errorf("scaleway: read the atlas application's policies: %w", reject(err))
	}
	var own []Policy
	var strangers []string
	for _, p := range held {
		if p.Name == a.name || p.Name == app.Name {
			own = append(own, p)
		} else {
			strangers = append(strangers, p.Name)
		}
	}
	slices.Sort(strangers)
	slices.SortFunc(own, func(x, y Policy) int { return createdAt(x.CreatedAt).Compare(createdAt(y.CreatedAt)) })
	if len(own) == 0 {
		if a.sink == nil {
			return strangers, nil
		}
		if _, err = a.client.CreatePolicy(ctx, PolicySpec{
			Name: a.name, Description: atlasDescription, ApplicationID: app.ID, Rules: rules,
		}); err != nil {
			return nil, fmt.Errorf("scaleway: write the atlas application's policy: %w", reject(err))
		}
		return strangers, nil
	}
	current, err := a.client.Rules(ctx, own[0].ID)
	if err != nil {
		return nil, fmt.Errorf("scaleway: read the atlas application's rules: %w", reject(err))
	}
	if a.sink == nil {
		return strangers, nil
	}
	if !sameRules(current, rules) {
		if err = a.client.SetRules(ctx, own[0].ID, rules); err != nil {
			return nil, fmt.Errorf("scaleway: write the atlas application's rules: %w", reject(err))
		}
	}
	for _, dup := range own[1:] {
		if err = a.client.DeletePolicy(ctx, dup.ID); err != nil {
			return nil, fmt.Errorf("scaleway: delete the atlas application's second policy: %w", reject(err))
		}
	}
	return strangers, nil
}

// sameRules reports whether a policy's rules are exactly the ones wanted:
// the same sets on the same scope, with no condition.
func sameRules(have []Rule, want []RuleSpec) bool {
	if len(have) != len(want) {
		return false
	}
	for i, w := range want {
		h := have[i]
		got, sets := slices.Sorted(slices.Values(h.PermissionSetNames)), slices.Sorted(slices.Values(w.PermissionSetNames))
		org := ""
		if h.OnOrganization() {
			org = *h.OrganizationID
		}
		if !slices.Equal(got, sets) || org != w.OrganizationID || strings.TrimSpace(h.Condition) != "" ||
			!slices.Equal(normalProjects(h.ProjectIDs), normalProjects(w.ProjectIDs)) {
			return false
		}
	}
	return true
}

// key holds the application to one key that works: the sealed one while it
// is live and not due for renewal, else a new one, sealed before the engine's
// older keys are deleted. It answers the keys somebody else added, and a
// finding where the Organization refuses the key's lifetime.
func (a atlasApp) key(ctx context.Context, app Application, access string) ([]string, *integration.Finding, error) {
	keys, err := a.client.APIKeysOf(ctx, app.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("scaleway: read the atlas application's keys: %w", reject(err))
	}
	var extra []string
	for _, k := range keys {
		if k.Description != atlasKeyDescription {
			extra = append(extra, k.AccessKey)
		}
	}
	slices.Sort(extra)
	i := slices.IndexFunc(keys, func(k APIKey) bool { return access != "" && k.AccessKey == access })
	if i >= 0 && !renewDue(keys[i].Expires(), a.now) {
		return extra, nil, nil
	}
	if a.sink == nil {
		pending := a.pending()
		return extra, &pending, nil
	}
	minted, err := a.client.CreateAPIKey(ctx, KeySpec{
		ApplicationID: app.ID, Description: atlasKeyDescription, ExpiresAt: expiry(a.now),
	})
	if ExpiryRefused(err) {
		remedy, url := remedyFor(err)
		refused := advisory("Atlas application has no key",
			"Scaleway refused an API key for the Atlas application: the Organization caps an API key's "+
				"lifetime below the 90 days it is minted with. Atlas reads nothing inside the projects until it has one",
			remedy, url)
		return extra, &refused, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("scaleway: mint the atlas application's key: %w", reject(err))
	}
	undo := func(cause error) error {
		if minted.AccessKey == "" {
			return cause
		}
		return errors.Join(cause, a.client.DeleteAPIKey(context.WithoutCancel(ctx), minted.AccessKey))
	}
	if !accessKeyPattern().MatchString(minted.AccessKey) || !uuidPattern().MatchString(minted.SecretKey) {
		return nil, nil, undo(errors.New("scaleway: created the atlas application's key and Scaleway returned no usable secret for it"))
	}
	// THE ACCESS KEY FIRST: where the secret then fails to seal, the sealed
	// access key names a key that is gone, which the next pass replaces. The
	// other order would leave a dead secret beside a live key's name.
	if err := a.sink.Record(ctx, AtlasAccessName, minted.AccessKey); err != nil {
		return nil, nil, undo(fmt.Errorf("scaleway: seal the atlas application's access key: %w", err))
	}
	if err := a.sink.Record(ctx, AtlasSecretName, minted.SecretKey); err != nil {
		// THE OLD ACCESS KEY BACK beside the secret it belongs to, so a
		// working pair stays sealed until the next pass mints again.
		if access != "" {
			err = errors.Join(err, a.sink.Record(context.WithoutCancel(ctx), AtlasAccessName, access))
		}
		return nil, nil, undo(fmt.Errorf("scaleway: seal the atlas application's key: %w", err))
	}
	for _, k := range keys {
		if k.Description != atlasKeyDescription {
			continue
		}
		if err := a.client.DeleteAPIKey(ctx, k.AccessKey); err != nil {
			return nil, nil, fmt.Errorf("scaleway: delete the atlas application's replaced key: %w", reject(err))
		}
	}
	return extra, nil, nil
}

// strayAtlasApplications is every application under the atlas application's
// name that is not the one recorded: what a create whose answer was lost
// leaves. Reported, never touched, since a name is no proof the engine made one.
func strayAtlasApplications(apps []Application, company, recorded string) []Application {
	name := AtlasName(company)
	var out []Application
	for _, app := range apps {
		if app.Name == name && app.ID != recorded {
			out = append(out, app)
		}
	}
	return out
}
