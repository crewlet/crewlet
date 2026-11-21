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
// A vendor webhook reaches ONE node (inbound deliveries are a fleet-wide
// consumer group) but a registry is per node, so the winner publishes this and
// the others hear it on an ephemeral broadcast subscription.
//
// It is a nudge, deliberately thin: it names the page, never its content or
// whether it is gone, since the backend is the authority on both and a late
// nudge must install neither a stale body nor a stale removal. Losing one costs
// staleness until the periodic walk.
type ToolSkillPageChanged struct {
	// Backend is the knowledge backend the page lives in ("confluence").
	// A node on another backend has nothing to re-read.
	Backend string `json:"backend"`

	// Container is the container the delivery named the page in, which
	// for a page that moved out of the skills container is the one it
	// moved to. Empty when the delivery did not say.
	Container string `json:"container"`

	// PageID is the backend's own id for the page. Empty on a Walk.
	PageID string `json:"page_id"`

	// Walk says the backend knows its skills container moved but not
	// WHICH page (Doclet's applier sees a batch touch a skill page and
	// nothing finer), so a hearer walks the whole
	// container rather than reading one page.
	//
	// It is what reaches a node that holds NO copy of the knowledge base
	// and so runs no applier to notice: without it such a node heard of a
	// skill edit only on its periodic walk, ten minutes later.
	Walk bool `json:"walk,omitempty"`
}

// EventType is the "tool_skill_page_changed" wire type.
func (ToolSkillPageChanged) EventType() string { return "tool_skill_page_changed" }

// Summary names the page; the envelope's source says which node heard the
// delivery.
func (e ToolSkillPageChanged) Summary() string {
	if e.Walk {
		return "Tool skills container " + e.Container + " changed"
	}
	page := e.PageID
	if page == "" {
		page = "(unnamed)"
	}
	return "Tool skill page " + page + " changed"
}
