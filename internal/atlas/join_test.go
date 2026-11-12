package atlas_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
)

// AN ENTRY NOTHING USED FOR A MONTH IS NOT USED RECENTLY, said only of a kind
// whose use the engine records, and only when that use was read in full: an
// unread usage history is no evidence of disuse.
func TestAnEntryNothingUsedIsNotUsedRecently(t *testing.T) {
	t.Parallel()
	m := must(t)
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	zone := atlas.MustRef(atlas.KindDomain, "acme.example")
	v := atlas.Fold([]atlas.Claim{m(atlas.Exists(web, settings, now)), m(atlas.Exists(zone, settings, now))}, now)
	used := func(ago time.Duration) atlas.Joined {
		return atlas.Joined{Counted: map[atlas.UseChannel]bool{atlas.UseCommand: true},
			Use: map[atlas.Ref][]atlas.Use{web: {{Channel: atlas.UseCommand, Counted: true, Month: 3, Last: now.Add(-ago),
				Seats: []string{"sre"}}}}}
	}
	for name, c := range map[string]struct {
		joined atlas.Joined
		quiet  bool
	}{
		"used two days ago":    {used(2 * 24 * time.Hour), false},
		"used forty days ago":  {used(40 * 24 * time.Hour), true},
		"never used, counted":  {atlas.Joined{Counted: map[atlas.UseChannel]bool{atlas.UseCommand: true}}, true},
		"never used, unread":   {atlas.Joined{}, false},
		"counted another kind": {atlas.Joined{Counted: map[atlas.UseChannel]bool{atlas.UseToolCall: true}}, false},
	} {
		got := atlas.Join(v, c.joined, now)
		if e := entry(t, got, web); e.NotUsedRecently != c.quiet {
			t.Errorf("%s: not used recently %v, want %v", name, e.NotUsedRecently, c.quiet)
		}
		if e := entry(t, got, zone); e.NotUsedRecently {
			t.Errorf("%s: a zone, whose use the engine does not record, is not used recently", name)
		}
	}
}

// WHAT A RECORD SAYS JOINS THE ENTRY ITS REF MERGED INTO: two members' use is
// one entry's, the counts added, the latest use kept, the seats together.
func TestWhatARecordSaysJoinsTheMergedEntry(t *testing.T) {
	t.Parallel()
	m := must(t)
	gh := atlas.MustRef(atlas.KindRepository, "github.com/acme/api")
	mirror := atlas.MustRef(atlas.KindRepository, "gitlet/api")
	v := atlas.Fold([]atlas.Claim{
		m(atlas.Exists(gh, settings, now)),
		m(atlas.Exists(mirror, gitlet, now)),
		m(atlas.Says(mirror, atlas.FactPullMirrorOf, "https://github.com/acme/api", settings, now)),
	}, now)
	joined := atlas.Join(v, atlas.Joined{
		Use: map[atlas.Ref][]atlas.Use{
			mirror: {{Channel: atlas.UsePush, Last: now.Add(-time.Hour)}},
			gh:     {{Channel: atlas.UsePush, Last: now.Add(-48 * time.Hour)}},
		},
		Signals: map[atlas.Ref][]atlas.Signal{gh: {{Kind: atlas.SignalServerTestFailed, Severity: atlas.SeverityDanger,
			Audience: atlas.AudienceAdmins, Since: now}}},
	}, now)
	e := entry(t, joined, gh)
	if len(e.Use) != 1 || !e.Use[0].Last.Equal(now.Add(-time.Hour)) {
		t.Errorf("the merged repository's use: %+v", e.Use)
	}
	if len(e.Signals) != 1 || e.Signals[0].Audience != atlas.AudienceAdmins {
		t.Errorf("the merged repository's signals: %+v", e.Signals)
	}
}

// A MONITOR'S STATE IS A SIGNAL ON IT AND ON WHAT IT WATCHES, read off the
// claims on every read: alert is danger, warn and no data are warnings, ok
// and unknown raise nothing, the signal dates from the vendor's own instant
// where it gave one, and a retired monitor raises nothing anywhere.
//
// Mutation: drop watch from Join, and an alerting monitor leaves the service
// it watches quiet.
func TestAMonitorsStateIsASignalOnWhatItWatches(t *testing.T) {
	t.Parallel()
	m := must(t)
	datadog := atlas.IntegrationSource("datadog")
	monitor := atlas.MustRef(atlas.KindMonitor, "datadog/monitor/456")
	api := atlas.MustRef(atlas.KindService, "api")
	other := atlas.MustRef(atlas.KindService, "worker")
	began := now.Add(-3 * time.Hour)
	base := []atlas.Claim{
		m(atlas.Exists(monitor, datadog, now)),
		m(atlas.Exists(other, settings, now)),
		m(atlas.Relates(api, atlas.RelationMonitoredBy, monitor, datadog, now)),
	}
	for state, want := range map[string]struct {
		kind     atlas.SignalKind
		severity atlas.Severity
		health   atlas.Health
	}{
		atlas.MonitorAlert:   {atlas.SignalMonitorAlerting, atlas.SeverityDanger, atlas.HealthAlerting},
		atlas.MonitorWarn:    {atlas.SignalMonitorWarning, atlas.SeverityWarning, atlas.HealthNeedsLook},
		atlas.MonitorNoData:  {atlas.SignalMonitorNoData, atlas.SeverityWarning, atlas.HealthNeedsLook},
		atlas.MonitorOK:      {"", "", atlas.HealthQuiet},
		atlas.MonitorUnknown: {"", "", atlas.HealthQuiet},
	} {
		claims := append(slices.Clone(base), m(atlas.Says(monitor, atlas.FactMonitorState, state, datadog, now)))
		v := atlas.Join(atlas.Fold(claims, now), atlas.Joined{}, now)
		for _, ref := range []atlas.Ref{monitor, api} {
			e := entry(t, v, ref)
			if e.Health() != want.health {
				t.Errorf("%s: %s is %s, want %s", state, ref, e.Health(), want.health)
			}
			if want.kind == "" {
				if len(e.Signals) != 0 {
					t.Errorf("%s: %s carries %+v", state, ref, e.Signals)
				}
				continue
			}
			if len(e.Signals) != 1 || e.Signals[0].Kind != want.kind || e.Signals[0].Severity != want.severity ||
				e.Signals[0].Audience != atlas.AudienceHolders || !e.Signals[0].Since.Equal(now) {
				t.Errorf("%s: %s carries %+v", state, ref, e.Signals)
			}
		}
		if e := entry(t, v, other); len(e.Signals) != 0 {
			t.Errorf("%s: a service the monitor does not watch carries %+v", state, e.Signals)
		}
	}
	alerting := append(slices.Clone(base),
		m(atlas.Says(monitor, atlas.FactMonitorState, atlas.MonitorAlert, datadog, now)),
		m(atlas.Says(monitor, atlas.FactStateSince, atlas.Instant(began).Text, datadog, now)))
	v := atlas.Join(atlas.Fold(alerting, now), atlas.Joined{}, now)
	if e := entry(t, v, api); len(e.Signals) != 1 || !e.Signals[0].Since.Equal(began) {
		t.Errorf("the signal's instant is not the vendor's own: %+v", e.Signals)
	}
	gone := m(atlas.Exists(monitor, datadog, now.Add(time.Minute)))
	gone.Denied = true
	v = atlas.Join(atlas.Fold(append(alerting, gone), now.Add(time.Minute)), atlas.Joined{}, now.Add(time.Minute))
	if e, ok := v.Find(api.ID()); ok && len(e.Signals) != 0 {
		t.Errorf("a retired monitor still signals on what it watched: %+v", e.Signals)
	}
}

// A PIPELINE WHOSE LAST RESULT IS A FAILURE IS A SIGNAL ON IT AND ON WHAT IT
// DEPLOYS, read off the claims on every read: failed and errored are a
// warning, since a failed run leaves what ran before it in place, every other
// result raises nothing, and a retired pipeline raises nothing anywhere. What
// a pipeline is only built from carries nothing.
//
// Mutation: drop deliver from Join, and a failed deployment leaves the name
// it deployed quiet.
func TestAFailingPipelineIsASignalOnWhatItDeploys(t *testing.T) {
	t.Parallel()
	m := must(t)
	github := atlas.IntegrationSource("github")
	repo := atlas.MustRef(atlas.KindRepository, "github.com/acme/api")
	environment := atlas.MustRef(atlas.KindPipeline, "github.com/acme/api#environment:production")
	name := atlas.MustRef(atlas.KindHostname, "api.acme.example")
	address := atlas.MustRef(atlas.KindEndpoint, "https://api.acme.example/")
	other := atlas.MustRef(atlas.KindHostname, "www.acme.example")
	base := []atlas.Claim{
		m(atlas.Exists(repo, github, now)),
		m(atlas.Exists(environment, github, now)),
		m(atlas.Exists(other, settings, now)),
		m(atlas.Relates(environment, atlas.RelationBuiltFrom, repo, github, now)),
		m(atlas.Relates(name, atlas.RelationDeployedBy, environment, github, now)),
		m(atlas.Relates(address, atlas.RelationDeployedBy, environment, github, now)),
	}
	for _, result := range atlas.FactLastResult.Spec().Words {
		failing := result == atlas.ResultFailed || result == atlas.ResultErrored
		claims := append(slices.Clone(base), m(atlas.Says(environment, atlas.FactLastResult, result, github, now)))
		v := atlas.Join(atlas.Fold(claims, now), atlas.Joined{}, now)
		for _, ref := range []atlas.Ref{environment, name, address} {
			e := entry(t, v, ref)
			if !failing {
				if len(e.Signals) != 0 || e.Health() != atlas.HealthQuiet {
					t.Errorf("%s: %s carries %+v", result, ref, e.Signals)
				}
				continue
			}
			if len(e.Signals) != 1 || e.Signals[0].Kind != atlas.SignalPipelineFailing ||
				e.Signals[0].Severity != atlas.SeverityWarning || e.Signals[0].Audience != atlas.AudienceHolders ||
				!e.Signals[0].Since.Equal(now) || e.Health() != atlas.HealthNeedsLook {
				t.Errorf("%s: %s carries %+v and is %s", result, ref, e.Signals, e.Health())
			}
		}
		for _, ref := range []atlas.Ref{repo, other} {
			if e := entry(t, v, ref); len(e.Signals) != 0 {
				t.Errorf("%s: %s, which the pipeline does not deploy, carries %+v", result, ref, e.Signals)
			}
		}
	}
	failed := append(slices.Clone(base), m(atlas.Says(environment, atlas.FactLastResult, atlas.ResultFailed, github, now)))
	gone := m(atlas.Exists(environment, github, now.Add(time.Minute)))
	gone.Denied = true
	v := atlas.Join(atlas.Fold(append(failed, gone), now.Add(time.Minute)), atlas.Joined{}, now.Add(time.Minute))
	if e, ok := v.Find(name.ID()); ok && len(e.Signals) != 0 {
		t.Errorf("a retired pipeline still signals on what it deployed: %+v", e.Signals)
	}
}
