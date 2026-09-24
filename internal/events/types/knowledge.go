package types

import (
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/textcut"
)

// What an agent read from the company's knowledge base.
//
// # Why reads are a record
//
// A page's HISTORY says who wrote it and a search's ranking says what it
// matched, and neither says whether anybody ever acted on it. "Which runbooks
// does this company's staff actually open" is the question a knowledge base is
// curated against — a page nobody has read in a quarter is a page to retire, a
// page every turn loads is one whose mistakes are expensive — and before this
// event the only evidence was the tool call buried inside a phase's
// transcript, which names a query string rather than a page and is not
// indexed by anything.
//
// # One event per ACT, listing its pages
//
// A search that surfaced six pages is ONE read with six pages, not six
// events, because what happened is one act by one seat: the rank of each page
// is a fact about that act, and splitting it would leave six rows each unable
// to say what else was on the list. The same holds for a phase whose skill
// catalogue offered several pages at once.
//
// # Nothing is recorded for nothing
//
// A search with no hits, a page that was not found and a catalogue that
// offered no skill publish nothing. The absence of a read is not a read, and a
// row carrying an empty page list would count toward "reads today" on every
// screen that sums them.

func init() {
	events.Register[KnowledgeRead]()
}

// KnowledgeReadVia is HOW a page reached an agent.
//
// A named string with a closed set of values, so a value a newer build
// produces decodes as a value rather than failing — [KnowledgeReadVia.Valid]
// is what a consumer switching on it checks.
type KnowledgeReadVia string

// The five ways a page reaches a seat. Two are acts the model chose (a direct
// read and a search), two are the engine choosing on its behalf (the
// turn-start prefetch and the skill catalogue a phase was offered), and one is
// the model acting on the engine's offer (loading a skill's body).
const (
	// ReadViaGetPage is get_page: the whole page, body and all, asked for
	// by id or by address.
	ReadViaGetPage KnowledgeReadVia = "get_page"
	// ReadViaSearch is search_knowledge: ranked titles and snippets, the
	// query the model wrote.
	ReadViaSearch KnowledgeReadVia = "search"
	// ReadViaPrefetch is the turn-start knowledge block: ranked titles and
	// snippets, searched on the auxiliary model's query before the first
	// phase ran.
	ReadViaPrefetch KnowledgeReadVia = "prefetch"
	// ReadViaSkillLoaded is load_tool_skill: a tool skill's whole body.
	ReadViaSkillLoaded KnowledgeReadVia = "skill_loaded"
	// ReadViaSkillInjected is a phase's tool-skill catalogue: each offered
	// skill's one-line summary, rendered into the system prompt.
	ReadViaSkillInjected KnowledgeReadVia = "skill_injected"
)

// Valid reports whether this build knows the value.
func (v KnowledgeReadVia) Valid() bool {
	switch v {
	case ReadViaGetPage, ReadViaSearch, ReadViaPrefetch,
		ReadViaSkillLoaded, ReadViaSkillInjected:
		return true
	}
	return false
}

// KnowledgeReadQueryMax bounds the query a read carries, in bytes.
//
// Two hundred: the query is a label a reader recognises a search by ("the one
// that looked for `deploy rollback`"), not an input anything re-runs. The
// model's own query is already capped at 400 bytes by search_knowledge and the
// prefetch's is one line of keywords, so the cap bites only on the long tail —
// and it is what keeps a read's row, which is written for every search every
// seat makes, from carrying a pasted thread.
const KnowledgeReadQueryMax = 200

// KnowledgeReadQuery is the query as a read records it: trimmed and clipped to
// [KnowledgeReadQueryMax] on a rune boundary, with the marker counted inside
// the cap so a clipped query says so.
//
// ONE RULE for every producer, and here rather than at each of them, because
// two producers clipping differently would make one search look like two to
// anything that groups on the text.
func KnowledgeReadQuery(query string) string {
	return textcut.Within(strings.TrimSpace(query), KnowledgeReadQueryMax)
}

// KnowledgeReadPage is one page a read reached.
type KnowledgeReadPage struct {
	// ID is the backend's own page id. Never empty: a hit with no id is
	// dropped by the producer, because a page nothing can address is not
	// a page any later reader can count.
	ID string `json:"id"`

	// Container is the space the page lives in, empty where the backend
	// did not say.
	Container string `json:"container,omitempty"`

	// Title is the page's title as it read at the time — a label, never
	// an address, since a rename does not rewrite history.
	Title string `json:"title,omitempty"`

	// Rank is the page's 1-based position in a ranked answer (a search or
	// the prefetch), and absent for a read that was not ranked.
	Rank int `json:"rank,omitempty"`
}

// KnowledgeRead fires when an agent reads from the knowledge base.
type KnowledgeRead struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`

	// Phase is the phase the read happened in. The prefetch runs before
	// any phase and names none.
	Phase Phase `json:"phase,omitempty"`

	Via KnowledgeReadVia `json:"via"`

	// Backend is the knowledge backend that answered ("native",
	// "confluence"), because a page id is only an address inside one.
	Backend string `json:"backend"`

	// Query is what a search searched for, through [KnowledgeReadQuery].
	// Empty for a read that was not a search.
	Query string `json:"query,omitempty"`

	// Pages are what the read reached, in rank order where it was ranked.
	Pages []KnowledgeReadPage `json:"pages"`
}

// EventType is the "knowledge_read" wire type.
func (KnowledgeRead) EventType() string { return "knowledge_read" }

// Role is the seat that read.
func (e KnowledgeRead) Role() string { return e.RoleName }

// AgentID is the instance that read.
func (e KnowledgeRead) AgentID() string { return e.Agent }

// SummaryFor names the page for a single-page read and counts them otherwise,
// with the query a search ran — which is what a feed line about a read is
// scanned for.
func (e KnowledgeRead) SummaryFor(actor string) string {
	what := strconv.Itoa(len(e.Pages)) + " pages"
	if len(e.Pages) == 1 {
		what = "'" + pageLabel(e.Pages[0]) + "'"
	}
	switch e.Via {
	case ReadViaSearch:
		return lead(actor, "searched knowledge for \""+e.Query+"\" ("+what+")")
	case ReadViaPrefetch:
		return lead(actor, "was given "+what+" at turn start")
	case ReadViaSkillLoaded:
		return lead(actor, "loaded tool skill "+what)
	case ReadViaSkillInjected:
		return lead(actor, "was offered tool skills from "+what)
	default:
		return lead(actor, "read "+what)
	}
}

// pageLabel is the title, or the id where the page had none.
func pageLabel(p KnowledgeReadPage) string {
	if title := strings.TrimSpace(p.Title); title != "" {
		return title
	}
	return p.ID
}
