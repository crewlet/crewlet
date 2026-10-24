package engine

import (
	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/slack"
)

// SetupProbes is which vendor check answers each integration's Test button
// on the setup surface: one read-only identity call per kind, made with a
// credential somebody typed and has not stored.
//
// HERE, BESIDE THE PASSES, because this package is where a seam meets the
// concrete client that satisfies it, and a probe is that for the setup
// surface's verify route exactly as a pass is for its provision route.
//
// NOTHING OF THE RUNNING ENGINE IS READ, which is why it is a function
// rather than a method: a probe is handed every value it uses by the
// request (and the company document the request is answered against), so
// it holds no epoch, no resolver and no store a stale reading could come
// from. Every client is built with its production transport, through
// internal/httpx.
func SetupProbes() map[integration.Kind]setup.Probe {
	return map[integration.Kind]setup.Probe{
		integration.KindAtlassian:  atlassian.Probe(atlassian.ClientOptions{}),
		integration.KindConfluence: confluence.Probe(nil),
		integration.KindDatadog:    datadog.Probe(nil),
		integration.KindGitHub:     github.Probe(nil),
		integration.KindGitLab:     gitlab.Probe(nil),
		integration.KindJira:       jira.Probe(nil),
		integration.KindMattermost: mattermost.Probe(nil, nil),
		integration.KindSlack:      slack.Probe(nil),
	}
}
