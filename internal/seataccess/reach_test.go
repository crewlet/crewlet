package seataccess_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// answering is a session whose Options answers scope as given, whatever the
// document says: the shape of a session that restates its vendor's reach
// wrongly, as App Store Connect's once did with no app list.
type answering struct {
	scope []string
	open  bool
}

func (a answering) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: lane, Scope: slices.Clone(a.scope), ScopeOpen: a.open}, nil
}

func (answering) Observe(context.Context, seataccess.ObserveInput) (seataccess.Observation, error) {
	return seataccess.Observation{}, nil
}

func (answering) Apply(context.Context, seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, nil
}

// listingCompany names a list at every lane whose reach the document writes,
// and turns on every lane whose session opens from the document alone.
func listingCompany() *config.Company {
	return &config.Company{
		Integrations: config.Integrations{
			GitLab:     &config.GitLab{Provisioning: &config.GitLabProvisioning{Projects: []string{"acme/api"}}},
			GitHub:     &config.GitHub{Provisioning: &config.GitHubProvisioning{Repos: []string{"acme/web"}}},
			Mattermost: &config.Mattermost{Provisioning: &config.MattermostProvisioning{Channels: []string{"ops"}}},
			SSH: &config.SSH{Enabled: true, Hosts: []config.SSHHost{{ID: "api-1"}},
				Provisioning: &config.SSHProvisioning{CertificateAuthority: "${SSH_CA}"}},
			GoogleCloud:     &config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{IdentityProject: "acme-identity"}},
			AppStoreConnect: &config.AppStoreConnect{Enabled: true},
			AppleAds:        &config.AppleAds{Enabled: true, AdAccounts: []string{"4567"}},
			MetaAds:         &config.MetaAds{Enabled: true, AdAccounts: []string{"act_1234"}},
			AdSense:         &config.AdSense{Enabled: true, Accounts: []string{"pub-1234567890123456"}},
			AdMob:           &config.AdMob{Enabled: true, Accounts: []string{"pub-6543210987654321"}},
			GoogleAnalytics: &config.GoogleAnalytics{Enabled: true, Properties: []string{"123456789"}},
			GoogleAds:       &config.GoogleAds{Enabled: true, Customers: []config.GoogleAdsCustomer{{ID: "1234567890"}}},
			GooglePlay:      &config.GooglePlay{Enabled: true, Apps: []string{"com.example.app"}},
			SearchConsole:   &config.SearchConsole{Enabled: true, Sites: []string{"sc-domain:example.com"}},
		},
		Roles: []config.Role{{Name: "SRE", Handle: "sre", Project: "ENG", Space: "OPS"}},
	}
}

// openCompany is listingCompany with every list absent: every lane whose
// reach the document writes reaches whatever its vendor accepts.
func openCompany() *config.Company {
	c := listingCompany()
	i := &c.Integrations
	i.GitLab.Provisioning.Projects, i.GitHub.Provisioning.Repos = nil, nil
	i.Mattermost.Provisioning.Channels = nil
	i.AppleAds.AdAccounts, i.MetaAds.AdAccounts = nil, nil
	i.AdSense.Accounts, i.AdMob.Accounts, i.GoogleAnalytics.Properties = nil, nil, nil
	i.GoogleAds.Customers, i.GooglePlay.Apps, i.SearchConsole.Sites = nil, nil, nil
	c.Roles = []config.Role{{Name: "SRE", Handle: "sre"}}
	return c
}

// EVERY LANE'S OFFER AND ITS REACH AGREE: whatever a session's Options says of
// the scope, the offer the sync and the access routes judge a seat by
// ([seataccess.ReadOptions]) reaches exactly what the vendor's document reach
// does, open where the document names nothing; a vendor that reads its reach
// from itself keeps its session's answer. Each lane whose session opens from
// the document alone is read through its real session too.
//
// Mutation: drop WithReach from ReadOptions, and every document lane takes its
// session's word, and App Store Connect's own session, which names no reach,
// refuses every app of a company naming none (the sync's refusal this fixed).
func TestEveryLanesOfferAgreesWithItsReach(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// NO VENDOR IS CALLED: a session that would call one fails at once.
	offline, cancel := context.WithCancel(ctx)
	cancel()
	real := map[seataccess.Lane]int{}
	for _, c := range []*config.Company{listingCompany(), openCompany()} {
		roster, err := c.Organization()
		if err != nil {
			t.Fatal(err)
		}
		in := seataccess.OpenInput{Config: c, Org: roster}
		for _, v := range seataccess.Registered() {
			doc, documented := v.(seataccess.DocumentReach)
			session, opened := openOffline(offline, v, in)
			for _, spec := range v.Catalog().Lanes {
				if !spec.Scoped {
					continue
				}
				for _, s := range []answering{{}, {scope: []string{"elsewhere"}, open: true}} {
					got, err := seataccess.ReadOptions(ctx, v, s, in, spec.Lane)
					if err != nil {
						t.Fatalf("%s: %v", spec.Lane, err)
					}
					if !documented {
						if got.ScopeOpen != s.open || !slices.Equal(got.Scope, s.scope) {
							t.Errorf("%s reads its reach from its vendor, and the offer changed it to %v (open %v)",
								spec.Lane, got.Scope, got.ScopeOpen)
						}
						continue
					}
					agree(t, spec.Lane, got, doc.Reach(c, roster, spec.Lane))
				}
				if !documented || !opened {
					continue
				}
				got, err := seataccess.ReadOptions(offline, v, session, in, spec.Lane)
				if err != nil {
					continue
				}
				agree(t, spec.Lane, got, doc.Reach(c, roster, spec.Lane))
				real[spec.Lane]++
			}
		}
	}
	// EVERY LANE THE ENGINE ALONE HOLDS opens from the document, so none drops
	// out of the real-session half unseen.
	for _, v := range seataccess.Registered() {
		for _, spec := range v.Catalog().Lanes {
			if spec.EngineHeld && real[spec.Lane] != 2 {
				t.Errorf("%s's own session was read for %d of the 2 companies", spec.Lane, real[spec.Lane])
			}
		}
	}
	if real[seataccess.LaneSSH] != 2 {
		t.Errorf("ssh's own session was read for %d of the 2 companies", real[seataccess.LaneSSH])
	}
}

// openOffline opens v's session where it opens without calling the vendor.
func openOffline(ctx context.Context, v seataccess.Vendor, in seataccess.OpenInput) (seataccess.Session, bool) {
	s, err := v.Open(ctx, in)
	return s, err == nil && s != nil
}

// agree holds an offer to the document's reach r: the same entries within it,
// an open reach open, and any other entry outside a listed one.
func agree(t *testing.T, lane seataccess.Lane, got seataccess.Options, r seataccess.Reach) {
	t.Helper()
	want := seataccess.Options{Lane: lane, Scope: r.Scope, ScopeOpen: r.Open}
	for _, entry := range append(slices.Clone(r.Scope), "elsewhere", "9876543210") {
		if got.Reaches(entry) != want.Reaches(entry) {
			t.Errorf("%s: the offer reaches %q = %v, the document's reach %v (%+v)", lane, entry,
				got.Reaches(entry), want.Reaches(entry), r)
		}
	}
	if got.ScopeOpen != r.Open || !got.ScopeLive {
		t.Errorf("%s: the offer is open %v, read %v; the document's reach is open %v", lane, got.ScopeOpen,
			got.ScopeLive, r.Open)
	}
}
