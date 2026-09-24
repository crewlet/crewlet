package skills

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/prompts"
)

// The registry: the live set of skills this company has published.
//
// Populated by the knowledge backend's sync (a complete walk of the skills
// container, and a single page whenever one page changed), so editing a skill
// is a wiki edit and nothing else: no restart, no deploy, no config push.
//
// # A page is the identity, and a key is only what a model asks for
//
// Everything that changes the registry names a PAGE: a walk returns pages, a
// webhook names the page that moved, and a deletion names the page that went.
// None of them can name a key, because a key is inside the page, and a page
// whose key was edited no longer says what it used to hold. So the registry
// keeps what each page holds, and the key-addressed set every reader sees is
// DERIVED from that.
//
// Deriving it is also what makes a one-page update the same answer as a walk.
// Two pages declaring one key are an authoring error, and a registry keyed by
// key could keep only one of them: when the winner was edited to another key
// the loser would be gone, and only the next walk would bring it back. Kept
// by page, the loser is still there and takes the key the moment it is free,
// so any sequence of page updates ends where a walk of the same pages would.

// Registry is the in-memory store of tool skills.
//
// READS TAKE NO LOCK BEYOND A SNAPSHOT SWAP, deliberately: the prompt-build
// path consults it once per phase and can accept an eventually-consistent
// answer (a skill that changed mid-build lands in this prompt or the next, and
// both are correct). Writes are serialised because a walk and a page update
// may reach one registry from two goroutines.
type Registry struct {
	mu sync.Mutex

	// pages is what each page holds, keyed by [Skill.SourcePageID]. It is
	// the source of truth, a shadowed duplicate included, and only writers
	// read it.
	pages map[string]Skill

	// skills and variables are swapped WHOLE rather than mutated, so a
	// lock-free reader sees one consistent set: a reader holding a map
	// being written to would see a half-applied edit, which for the
	// variable map means a body rendered with some substitutions applied
	// and some not.
	skills    map[string]Skill
	variables map[string]string
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		pages: map[string]Skill{}, skills: map[string]Skill{},
		variables: map[string]string{},
	}
}

// SetVariables installs the operator-defined ${var} substitution map.
//
// Called on every apply, boot included. Re-checking every registered skill
// against the new map is what makes a variable REMOVED by a config push
// surface immediately rather than on that skill's next edit, which might be
// never.
func (r *Registry) SetVariables(variables map[string]string) {
	if r == nil {
		return
	}
	next := maps.Clone(variables)
	if next == nil {
		next = map[string]string{}
	}
	r.mu.Lock()
	r.variables = next
	current := slices.Collect(maps.Values(r.skills))
	r.mu.Unlock()

	for _, skill := range current {
		warnUnresolved(skill, next)
	}
}

// warnUnresolved reports a skill referencing a variable nobody defined.
//
// Substitution deliberately leaves an unknown reference as literal ${name},
// which is visible and greppable, but the only place that literal is ever
// seen is inside an LLM prompt, and no operator reads those. This turns a
// silent prompt defect into a log line at the moment it becomes true.
func warnUnresolved(s Skill, variables map[string]string) {
	for field, text := range map[string]string{
		"title": s.Title, "summary": s.Summary, "body": s.Body,
	} {
		for _, name := range VariableRefs(text) {
			if _, ok := variables[name]; !ok {
				log.Warn("skill_variable_unresolved", "skill", s.Key,
					"field", field, "variable", name)
			}
		}
	}
}

// PageChange is what recording one page did to the registry.
type PageChange struct {
	// Before is the key the page held until now, and After the key it holds
	// from now on. Either is empty when the page held no skill, so a page
	// that became a skill has only After and a page that stopped being one
	// has only Before.
	Before, After string

	// Shadowed reports that After is served from another page: two pages
	// declare that key, and the other one keeps it (see [comparePageIDs]).
	Shadowed bool
}

// Changed reports whether the page holds a different key than it did.
//
// An edit that kept its key reports false even though its body may have
// moved, because the key is what a log line about a page can usefully name.
func (c PageChange) Changed() bool { return c.Before != c.After }

// PutPage records the skill one page now holds, replacing whatever it held.
//
// A REPLACE rather than a merge, because a page IS the skill: an edit that
// removed a trigger leaf must remove it here, and a merge would keep the skill
// matching a surface its author has just stopped claiming. A skill whose key
// the page no longer declares is gone with the same write, which is the case a
// key-addressed upsert could never see.
//
// The skill must name its page; one that does not has no identity a later
// change could reach, so it is refused rather than stored.
func (r *Registry) PutPage(s Skill) (PageChange, error) {
	if r == nil {
		return PageChange{}, nil
	}
	if err := admissible(s); err != nil {
		return PageChange{}, err
	}
	r.mu.Lock()
	next := maps.Clone(r.pages)
	before := next[s.SourcePageID].Key
	next[s.SourcePageID] = s
	served, duplicates := derive(next)
	r.pages, r.skills = next, served
	variables := r.variables
	r.mu.Unlock()

	reportDuplicates(duplicates, s.SourcePageID)
	warnUnresolved(s, variables)
	return PageChange{
		Before: before, After: s.Key,
		Shadowed: served[s.Key].SourcePageID != s.SourcePageID,
	}, nil
}

// DropPage records that a page holds no skill: it was deleted, moved out of the
// container, or edited into an ordinary page.
//
// A duplicate this page was shadowing takes the key in the same write, which
// is what a walk that no longer carried this page would have served.
func (r *Registry) DropPage(pageID string) PageChange {
	if r == nil {
		return PageChange{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	held, ok := r.pages[pageID]
	if !ok {
		return PageChange{}
	}
	next := maps.Clone(r.pages)
	delete(next, pageID)
	r.pages, r.skills = next, served(next)
	return PageChange{Before: held.Key}
}

// HoldsPage reports whether a page contributes a skill, a shadowed duplicate
// included.
//
// For a sync deciding whether a change it heard about concerns this registry:
// a page that left the skills container is announced from the container it
// moved to, and this is how that announcement is still recognised.
func (r *Registry) HoldsPage(pageID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.pages[pageID]
	return ok
}

// Replace swaps the whole set, for a complete walk of the container.
//
// ATOMIC, which is what makes a walk safe to run against a registry that is
// already serving: a walk applied skill by skill would leave a window where
// half the company's guidance exists.
//
// A caller that could not complete its walk must NOT call this: a wholesale
// replace from a partial enumeration silently deletes every skill it did not
// reach.
func (r *Registry) Replace(skills []Skill) {
	if r == nil {
		return
	}
	next := make(map[string]Skill, len(skills))
	for _, s := range skills {
		if err := admissible(s); err != nil {
			log.Warn("skill_refused", "skill", s.Key, "page", s.SourcePageID,
				"error", err.Error())
			continue
		}
		next[s.SourcePageID] = s
	}
	current, duplicates := derive(next)
	r.mu.Lock()
	r.pages, r.skills = next, current
	variables := r.variables
	r.mu.Unlock()

	reportDuplicates(duplicates, "")
	for _, s := range current {
		warnUnresolved(s, variables)
	}
	log.Info("skills_replaced", "count", len(current), "pages", len(next))
}

// admissible is what the registry refuses before it stores anything.
func admissible(s Skill) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(s.SourcePageID) == "" {
		return fmt.Errorf("skills: skill %q names no source page, and a page is "+
			"what every later change to it is addressed by", s.Key)
	}
	return nil
}

// duplicate is one page whose key another page keeps.
type duplicate struct {
	key, kept, shadowed string
}

// derive is the key-addressed set a registry serves from what its pages hold.
//
// ONE WINNER PER KEY, chosen by page id rather than by arrival, so every node
// serves the same page for a duplicated key whatever order its walks and
// updates happened to come in. The losers are reported so an author learns
// that one of their pages is not being served.
func derive(pages map[string]Skill) (map[string]Skill, []duplicate) {
	out := make(map[string]Skill, len(pages))
	var duplicates []duplicate
	for _, id := range slices.SortedFunc(maps.Keys(pages), comparePageIDs) {
		s := pages[id]
		if kept, taken := out[s.Key]; taken {
			duplicates = append(duplicates, duplicate{
				key: s.Key, kept: kept.SourcePageID, shadowed: id,
			})
			continue
		}
		out[s.Key] = s
	}
	return out, duplicates
}

// served is [derive] without the report, for a write that cannot create a new
// duplicate: removing a page only ever resolves one.
func served(pages map[string]Skill) map[string]Skill {
	out, _ := derive(pages)
	return out
}

// comparePageIDs orders page ids so the oldest page keeps a duplicated key.
//
// SHORTER FIRST, then lexically, because Confluence numbers pages in the order
// they were created and a lexical comparison alone would rank page 9 after
// page 10. For ids of one length (a UUID, a time-ordered id) it is plain
// lexical order. Either way it is a total order every node computes the same.
func comparePageIDs(a, b string) int {
	if len(a) != len(b) {
		return cmp.Compare(len(a), len(b))
	}
	return strings.Compare(a, b)
}

// reportDuplicates logs the pages whose key another page keeps.
//
// A single-page write passes the page it wrote, and only the duplicates that
// page takes part in are reported: the rest were reported by the walk or the
// write that created them, and repeating every one on every edit would bury
// the line that is new.
func reportDuplicates(duplicates []duplicate, page string) {
	for _, d := range duplicates {
		if page != "" && d.kept != page && d.shadowed != page {
			continue
		}
		log.Warn("skill_key_duplicated", "skill", d.key, "served_page", d.kept,
			"shadowed_page", d.shadowed,
			"detail", "two pages declare this key and only one can be served; "+
				"give the shadowed page a key of its own or remove it")
	}
}

// Get returns one skill by key.
func (r *Registry) Get(key string) (Skill, bool) {
	if r == nil {
		return Skill{}, false
	}
	r.mu.Lock()
	skills := r.skills
	r.mu.Unlock()
	s, ok := skills[key]
	return s, ok
}

// Len is how many skills are registered.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.skills)
}

// Matching returns the skills whose trigger fires for a surface in a phase.
//
// KEY-SORTED. The prompt package sorts again — its byte-stability is its own
// promise to keep — but answering in map order here would make every other
// caller's output move between two identical builds for no reason anybody
// could see.
func (r *Registry) Matching(phase prompts.Phase, surface prompts.Surface) []Skill {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	skills := r.skills
	r.mu.Unlock()

	var out []Skill
	for _, s := range skills {
		if s.AppliesTo(phase) && s.Trigger.Matches(surface) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Skill) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// SkillsFor implements [prompts.SkillCatalogue].
func (r *Registry) SkillsFor(phase prompts.Phase, surface prompts.Surface) []prompts.Skill {
	return entries(r.Matching(phase, surface))
}

// entries is what a prompt's catalogue renders of each skill.
func entries(matching []Skill) []prompts.Skill {
	out := make([]prompts.Skill, 0, len(matching))
	for _, s := range matching {
		out = append(out, prompts.Skill{
			Key: s.Key, Summary: s.Summary, Required: s.Required,
		})
	}
	return out
}

// Offer is the registry as ONE phase's prompts see it, recording what each
// prompt it rendered was offered.
//
// Which skills a catalogue offered is a fact only the render knows: the phase
// and the surface it is matched against are the prompt builder's, and the
// registry is live, so asking it again afterwards is asking a second question
// that can get a different answer. Recording inside the render is what makes
// "this prompt carried these skills' summaries" a statement about the prompt
// that was sent.
//
// Each render is one OFFERING, kept apart from the others: a phase that builds
// several prompts (a delegate call's workers, each with its own) offered each
// of them, and a set merged across them would say one prompt carried what two
// did.
type Offer struct {
	registry *Registry

	mu        sync.Mutex
	offerings [][]Skill
}

// Offer starts recording what this registry's catalogue offers. Nil for a nil
// registry, which [prompts.SkillCatalogue] callers must not wrap in an
// interface — see [Offer.Catalogue].
func (r *Registry) Offer() *Offer {
	if r == nil {
		return nil
	}
	return &Offer{registry: r}
}

// Catalogue is the offer as a prompt takes it, or a nil interface for a nil
// offer. A TYPED NIL WOULD NOT BE NIL: the prompt builder checks its catalogue
// against nil, and a non-nil interface over a nil offer would render a header
// over nothing.
func (o *Offer) Catalogue() prompts.SkillCatalogue {
	if o == nil {
		return nil
	}
	return o
}

// SkillsFor implements [prompts.SkillCatalogue], recording the offering. A
// render that matched nothing records nothing: an empty catalogue is not an
// offer.
func (o *Offer) SkillsFor(phase prompts.Phase, surface prompts.Surface) []prompts.Skill {
	matching := o.registry.Matching(phase, surface)
	if len(matching) > 0 {
		o.mu.Lock()
		o.offerings = append(o.offerings, matching)
		o.mu.Unlock()
	}
	return entries(matching)
}

// Render implements [prompts.SkillCatalogue].
func (o *Offer) Render(text string) string { return o.registry.Render(text) }

// Drain returns every offering recorded since the last drain, oldest first,
// and forgets them — so an offering is reported exactly once however many
// callers drain it. Nil-safe.
func (o *Offer) Drain() [][]Skill {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	out := o.offerings
	o.offerings = nil
	return out
}

// Render implements [prompts.SkillCatalogue].
func (r *Registry) Render(text string) string {
	if r == nil {
		return text
	}
	r.mu.Lock()
	variables := r.variables
	r.mu.Unlock()
	return Substitute(text, variables)
}

// Audit reports every skill whose trigger names a tool this deployment does
// not have.
//
// Run against the real registry once per apply, because exact-string trigger
// matching is validated nowhere else: an upstream MCP server renaming a tool
// silently disables the skill AND the guard that was enforcing it, and
// nothing raises. See [Trigger.Classify] for why a partially-live skill is a
// warning and a wholly-dead one is a note.
func (r *Registry) Audit(knownTools, knownServers []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	skills := r.skills
	r.mu.Unlock()

	for _, key := range slices.Sorted(maps.Keys(skills)) {
		verdict := skills[key].Trigger.Classify(knownTools, knownServers)
		switch {
		case len(verdict.Dangling) == 0:
		case verdict.Live:
			log.Warn("skill_trigger_partially_dangling", "skill", key,
				"unknown_tools", verdict.Dangling,
				"detail", "some of this trigger still matches, so these names "+
					"are most likely drift after a tool was renamed upstream")
		default:
			log.Info("skill_trigger_matches_nothing", "skill", key,
				"unknown_tools", verdict.Dangling,
				"detail", "no part of this trigger matches this deployment; it "+
					"may be authored for a stack this company does not run")
		}
	}
}

// Loaded is one skill as load_tool_skill hands it over: the rendered body and
// the page it was read from.
//
// ONE VALUE rather than a body and a separate provenance lookup, because a
// page can be edited between two reads of a live registry and a load recorded
// against the page that followed it would name a page the model never saw.
type Loaded struct {
	// Body is the rendered skill, with its title and summary as a header.
	Body string

	// PageID, Backend, Container and Title are the page the body came from
	// — see [Skill.SourcePageID] and [Skill.SourceContainer].
	PageID    string
	Backend   string
	Container string
	Title     string
}

// Load renders a skill for loading, or reports that it is not there.
//
// The rendered body carries its TITLE and its trigger's subject, because a
// model that asked for a key gets back prose with no header otherwise — and
// a body it cannot attribute is a body it cannot decide to trust.
func (r *Registry) Load(key string) (Loaded, bool) {
	s, ok := r.Get(key)
	if !ok {
		return Loaded{}, false
	}
	var b strings.Builder
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = s.Key
	}
	b.WriteString("# " + r.Render(title) + "\n\n")
	if summary := strings.TrimSpace(s.Summary); summary != "" {
		b.WriteString(r.Render(summary) + "\n\n")
	}
	b.WriteString(r.Render(s.Body))
	return Loaded{
		Body:   strings.TrimRight(b.String(), "\n") + "\n",
		PageID: s.SourcePageID, Backend: s.SourceBackend,
		Container: s.SourceContainer, Title: s.SourceTitle,
	}, true
}
