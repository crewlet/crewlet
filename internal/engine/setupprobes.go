package engine

import (
	"github.com/crewlet/crewlet/internal/admob"
	"github.com/crewlet/crewlet/internal/adsense"
	"github.com/crewlet/crewlet/internal/appleads"
	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/atlassian"
	awsint "github.com/crewlet/crewlet/internal/aws"
	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/digitalocean"
	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/googleads"
	"github.com/crewlet/crewlet/internal/googleanalytics"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/googleplay"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/metaads"
	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/searchconsole"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/slack"
	sshint "github.com/crewlet/crewlet/internal/ssh"
)

// SetupProbes is which vendor check answers each integration's Test button
// on the setup surface: one read-only identity call per kind, made with a
// credential somebody typed and has not stored.
// It lives beside the passes: a probe is the verify route's concrete client
// as a pass is the provision route's.
//
// A function, not a method: a probe uses only what the request hands it, so
// it holds no epoch, resolver or store a stale reading could come from.
// Every client uses its production transport (internal/httpx).
func SetupProbes() map[integration.Kind]setup.Probe {
	return map[integration.Kind]setup.Probe{
		integration.KindAtlassian:       atlassian.Probe(atlassian.ClientOptions{}),
		integration.KindMetaAds:         metaads.Probe(),
		integration.KindAppStoreConnect: appstoreconnect.Probe(),
		integration.KindAppleAds:        appleads.Probe(),
		integration.KindCloudflare:      cloudflare.Probe(nil),
		integration.KindConfluence:      confluence.Probe(nil),
		integration.KindDatadog:         datadog.Probe(nil),
		integration.KindDigitalOcean:    digitalocean.Probe(nil),
		integration.KindExpo:            expo.Probe(nil),
		integration.KindAWS:             awsint.Probe(nil),
		integration.KindGitHub:          github.Probe(nil),
		integration.KindGitLab:          gitlab.Probe(nil),
		integration.KindGoogleCloud:     googlecloud.Probe(nil),
		integration.KindGoogleAds:       googleads.Probe(nil),
		integration.KindGooglePlay:      googleplay.Probe(nil),
		integration.KindJira:            jira.Probe(nil),
		integration.KindMattermost:      mattermost.Probe(nil, nil),
		integration.KindScaleway:        scaleway.Probe(nil),
		integration.KindSearchConsole:   searchconsole.Probe(nil),
		integration.KindSlack:           slack.Probe(nil),
		integration.KindSSH:             sshint.Probe(nil),
		integration.KindGoogleAnalytics: googleanalytics.Probe(nil),
		integration.KindAdMob:           admob.Probe(nil),
		integration.KindAdSense:         adsense.Probe(nil),
	}
}

// SetupDrawers is which integration redraws a live view from what its form
// holds: what answers POST /setup/integrations/{kind}/draw.
func SetupDrawers() map[integration.Kind]setup.Drawer {
	return map[integration.Kind]setup.Drawer{integration.KindGoogleCloud: googlecloud.Draw}
}
