package types

import (
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/named"
)

// What an agent read from the company's knowledge base.
//
// Which pages staff actually open is what a knowledge base is curated against;
// history and ranking say neither, and a phase's transcript names only a query
// string, not a page. One event per ACT, listing its pages: a search that
// surfaced six is ONE read with six, since each page's rank is a fact about
// that act (likewise a skill catalogue offering several). A search with no
// hits, a page not found and a catalogue with no skill publish nothing, or an
// empty list would count toward "reads today".

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

// KnowledgeReadQuery is the query as a read records it: trimmed, and
// otherwise WHOLE.
//
// It used to be clipped to two hundred bytes as "a label a reader recognises a
// search by" — but every producer already bounds it where it is written
// (search_knowledge refuses a query past four hundred bytes; the prefetch's is
// one line of keywords), so the clip changed nothing but the long tail, where
// it made two different searches that shared an opening read as one search
// to anything grouping on the text.
//
// ONE RULE for every producer, and here rather than at each of them, because
// two producers normalising differently would make one search look like two.
func KnowledgeReadQuery(query string) string { return strings.TrimSpace(query) }

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

// PhraseFor names the page for a single-page read and counts them otherwise,
// with the query a search ran — which is what a feed line about a read is
// scanned for. A Doclet page is named as one; another backend's is its words.
func (e KnowledgeRead) PhraseFor(actor string) named.Line {
	what := named.Words(strconv.Itoa(len(e.Pages)) + " pages")
	if len(e.Pages) == 1 {
		page := named.Ref{Text: pageLabel(e.Pages[0])}
		if e.Backend == KnowledgeNative {
			page = named.PageRef(e.Pages[0].ID, page.Text)
		}
		what = named.Words("'").Name(page).Words("'")
	}
	switch e.Via {
	case ReadViaSearch:
		return leadLine(actor, named.Words("searched knowledge for \""+e.Query+"\" (").Then(what).Words(")"))
	case ReadViaPrefetch:
		return leadLine(actor, named.Words("was given ").Then(what).Words(" at turn start"))
	case ReadViaSkillLoaded:
		return leadLine(actor, named.Words("loaded tool skill ").Then(what))
	case ReadViaSkillInjected:
		return leadLine(actor, named.Words("was offered tool skills from ").Then(what))
	default:
		return leadLine(actor, named.Words("read ").Then(what))
	}
}

// KnowledgeNative is the Backend of a read Doclet answered (doclet.Backend),
// whose page ids are Doclet's own.
const KnowledgeNative = "native"

// pageLabel is the title, or the id where the page had none.
func pageLabel(p KnowledgeReadPage) string {
	if title := strings.TrimSpace(p.Title); title != "" {
		return title
	}
	return p.ID
}
