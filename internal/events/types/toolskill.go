package types

import "github.com/crewlet/crewlet/internal/events"

// Tool skills: the knowledge-base pages every node's skill registry is read
// from, and the nudge that keeps a fleet's registries agreeing about them.

func init() {
	events.Register[ToolSkillPageChanged]()
}

// ToolSkillPageChanged tells every node that one page in the tool-skills
// container changed, so each re-reads that page into its own registry.
//
// # Why a broadcast, when the change already arrived as a webhook
//
// A vendor webhook reaches ONE node: inbound deliveries are a fleet-wide
// consumer group, so exactly one member parses each one. A registry is per
// node, though, and a node that only ever learned about skills from the
// deliveries it happened to win would serve guidance its peers had already
// replaced. The node that wins the delivery publishes this; every other node
// hears it on an ephemeral broadcast subscription and re-reads the page
// itself.
//
// # Deliberately thin, and a nudge rather than a record
//
// It names the page and never carries its content, nor whether the page is
// gone: each node reads the page from the backend, which is the one authority
// on what it now says and on whether it is there, so a nudge that arrives late
// can install neither a stale body nor a stale removal. Losing one costs
// staleness until the periodic walk every node runs, never a divergence that
// lasts.
type ToolSkillPageChanged struct {
	// Backend is the knowledge backend the page lives in ("confluence").
	// A node on another backend has nothing to re-read.
	Backend string `json:"backend"`

	// Container is the container the delivery named the page in, which
	// for a page that moved out of the skills container is the one it
	// moved to. Empty when the delivery did not say.
	Container string `json:"container"`

	// PageID is the backend's own id for the page.
	PageID string `json:"page_id"`
}

// EventType is the "tool_skill_page_changed" wire type.
func (ToolSkillPageChanged) EventType() string { return "tool_skill_page_changed" }

// Summary names the page; the envelope's source says which node heard the
// delivery.
func (e ToolSkillPageChanged) Summary() string {
	page := e.PageID
	if page == "" {
		page = "(unnamed)"
	}
	return "Tool skill page " + page + " changed"
}
