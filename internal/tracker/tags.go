package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A project's tag set — the one catalogue any seat may add to.
//
// # Adding is a SEAT's, and archiving and renaming are not
//
// A tag is how work is grouped for a week: a seat filing a task under
// `regression` while it triages a run of them is the tag set doing its job,
// and a company whose tags could only be declared by a person would be a
// company whose tags were never declared. So an ADD is open to every seat.
//
// A RENAME and an ARCHIVE are not, and the difference is who they reach: a
// rename changes the word on every task already filed under it, and an archive
// takes a filter out of everybody's board. Both are decisions about how the
// company groups its work rather than about one task, which is exactly the
// line the lead rule draws.
//
// # An archive is one-way, like a field's
//
// The tasks keep the tag and every filter on it still answers — what an
// archive buys is a tag that takes no NEW work, which is what stops a
// deprecated grouping quietly accumulating. Un-archiving is refused for the
// reason a field's is: a tag that came back would silently re-admit work filed
// against a meaning nobody has looked at in a year, and bringing one back
// means declaring it again under its own name.
//
// # Why the tag set is its OWN object rather than a field of the project
//
// Because of who writes each. Any seat adds a tag, and only a lead edits a
// project's field declarations and its sprint policy — so on one object every
// tag add would contend with every policy edit on one subject, and the gate
// could only ever be "may you write the whole thing". Two subjects make the
// two authorities two arbitration units.

// TagEdit is one change to a project's tag set.
//
// THREE FACETS RATHER THAN A WHOLE SET, for that same reason one level down: a
// verb that took the set would make every add a rewrite of everybody else's
// tags, and a seat's add would silently revert a lead's archive.
type TagEdit struct {
	// Add declares tags that do not exist. Naming one that does is not an
	// error — it is the ordinary outcome of two seats reaching for the
	// same word, and the set already has it.
	Add []Tag

	// Rename changes a tag's LABEL, keyed by slug. The slug is immutable:
	// it is what every task's row holds, so renaming it would orphan them.
	Rename map[string]string

	// Archive takes tags out of what may be filed under, one-way.
	Archive []string
}

// Empty reports an edit with nothing in it, so a caller can refuse before it
// publishes a record saying nothing.
func (e TagEdit) Empty() bool {
	return len(e.Add) == 0 && len(e.Rename) == 0 && len(e.Archive) == 0
}

// TagAuthority is what a caller may do to a tag set.
//
// A VALUE RATHER THAN A BOOL ON THE WRITER, for the reason the other authority
// values are: "may I archive this project's tags" is a question about a PAIR —
// who is asking and about which project — and the answer for one pair says
// nothing about another.
type TagAuthority struct {
	// Lead reports whether the actor leads the project or sits above it.
	// Resolved by the caller from the org chart, because this package has
	// no chart.
	Lead bool
}

// WriteTags applies one edit to a project's tag set.
//
// The warnings it returns are the near-duplicates the edit declared, and they
// are ADVISORY: a lead can merge two tags and a refusal with no override would
// block `apis` behind `api` for ever. THE APPLY MUST NEVER DEPEND ON ONE —
// every node applies the same record, and only this node saw the warning.
func (w *Writer) WriteTags(ctx context.Context, opID, project string,
	edit TagEdit, authority TagAuthority) (WriteResult, error) {

	project = ProjectKey(project)
	switch {
	case project == "":
		return WriteResult{}, fmt.Errorf("tracker: a tag edit names no project")
	case edit.Empty():
		return WriteResult{}, fmt.Errorf("tracker: a tag edit of %s adds, "+
			"renames and archives nothing", project)
	case !authority.Lead && (len(edit.Rename) > 0 || len(edit.Archive) > 0):
		// THE REFUSAL NAMES WHO CAN, because a seat that hit it was
		// tidying up and the answer is "ask the lead" rather than
		// "you cannot".
		return WriteResult{}, fmt.Errorf("tracker: renaming or archiving a tag "+
			"of %s is the project lead's — it changes the word on every task "+
			"already filed under it, and takes a filter off everybody's board. "+
			"Adding a tag is open to every seat", project)
	}
	subject := TagsSubject(project)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	var warnings, declared []string
	result, err := w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readTagSet(ctx, tx, project)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !held {
				// A MISSING TAG SET IS AN EMPTY ONE, not a refusal:
				// `EnsureProjects` does not write it and the first
				// write self-heals it, exactly as a key counter's
				// first mint does.
				current = TagSet{V: DocumentVersion, Project: project}
			}
			next, added, warned, changed, err := applyTagEdit(current, edit,
				w.Actor, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			// BOTH ARE TAKEN FROM THE WINNING ATTEMPT, and assigned
			// rather than appended: a re-decide against another
			// writer's set finds a shorter add list and possibly a
			// near-duplicate the first pass did not, and what the
			// caller is told has to be what actually landed.
			warnings, declared = warned, added
			if !changed {
				// NOTHING TO SAY. Two seats reaching for the same word
				// is the ordinary case, and the second one's add is a
				// no-op rather than a conflict — an empty decision is
				// a legitimate outcome.
				return statelog.Decision{}, nil
			}
			decision, err := w.decide(subject, OpPatch, ChangeTags, scope, opID,
				next, nil, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
	result.Warnings = warnings
	result.Declared = declared
	return result, err
}

// applyTagEdit is the whole rule, as a pure function over one set.
//
// PURE, so every refusal is testable without a broker: the collisions this
// enforces — a label another tag already uses, a label that is another tag's
// slug — are what a seat meets most often, and a rule only reachable through a
// published record is a rule nobody re-measures.
//
// It reports the next set, the slugs it DECLARED, the advisory warnings, and
// whether anything changed.
//
// The declared list is what a caller is told it created, and it comes from
// here rather than from a read before the write for the reason the whole
// package is built on: two seats reaching for the same word is the ordinary
// case, so a list computed outside the snapshot names tags somebody else
// declared a moment earlier.
func applyTagEdit(current TagSet, edit TagEdit, actor string, at time.Time) (
	TagSet, []string, []string, bool, error) {

	next := current
	next.Tags = slices.Clone(current.Tags)
	next.Project = current.Project
	if next.V == 0 {
		next.V = DocumentVersion
	}

	// ONE INDEX OVER BOTH SPELLINGS. A new tag's label may collide with
	// another tag's LABEL or with another tag's SLUG, and both collisions
	// are the same failure: two groupings a person cannot tell apart, that
	// the work then splits between at random.
	taken := map[string]int{}
	bySlug := map[string]int{}
	for i, tag := range next.Tags {
		bySlug[tag.Slug] = i
		taken[strings.ToLower(tag.Label)] = i
		taken[tag.Slug] = i
	}
	changed := false
	var warnings, declared []string

	for _, want := range edit.Add {
		slug := TagSlug(want.Slug)
		if slug == "" {
			slug = TagSlug(want.Label)
		}
		label := strings.TrimSpace(want.Label)
		if label == "" {
			label = slug
		}
		switch {
		case slug == "":
			return TagSet{}, nil, nil, false, fmt.Errorf("tracker: a tag needs a "+
				"name, and %q normalises to nothing — a tag slug is lowercase "+
				"letters, digits and hyphens", want.Label)
		case !ValidTagSlug(slug):
			return TagSet{}, nil, nil, false, fmt.Errorf("tracker: %q is not a tag "+
				"slug — a tag starts with a letter or a digit and carries "+
				"lowercase letters, digits and hyphens, up to 64 characters",
				slug)
		case len(label) > MaxTagLabel:
			return TagSet{}, nil, nil, false, fmt.Errorf("tracker: the tag %s is "+
				"labelled with %d characters and at most %d are stored — a tag "+
				"is rendered on every row it is on, and one this long pushes "+
				"the title off the screen", slug, len(label), MaxTagLabel)
		}
		if _, exists := bySlug[slug]; exists {
			// ALREADY THERE IS NOT AN ERROR. Two seats reaching for the
			// same word on the same afternoon is the ordinary case, and
			// refusing the second would make a tag set something every
			// seat has to read before it can file anything.
			continue
		}
		if other, clash := taken[strings.ToLower(label)]; clash {
			return TagSet{}, nil, nil, false, fmt.Errorf("tracker: %s already has a "+
				"tag %s labelled %q — two tags a person cannot tell apart split "+
				"the work between them at random",
				current.Project, next.Tags[other].Slug, next.Tags[other].Label)
		}
		if len(next.Tags) >= MaxTagsPerProject {
			return TagSet{}, nil, nil, false, fmt.Errorf("tracker: %s already has "+
				"%d tags, which is the most a project keeps — archive what is "+
				"no longer filed under before declaring more",
				current.Project, MaxTagsPerProject)
		}
		if near := nearTags(next.Tags, slug); len(near) > 0 {
			warnings = append(warnings, fmt.Sprintf("%s is within a typo of %s "+
				"— declared anyway; merge them if that was not deliberate",
				slug, strings.Join(near, ", ")))
		}
		bySlug[slug] = len(next.Tags)
		taken[strings.ToLower(label)] = len(next.Tags)
		taken[slug] = len(next.Tags)
		next.Tags = append(next.Tags, Tag{
			Slug: slug, Label: label, Color: strings.TrimSpace(want.Color),
			Description: want.Description, CreatedBy: actor, CreatedAt: at,
		})
		declared = append(declared, slug)
		changed = true
	}

	// SORTED, so a rename map — which Go iterates in a random order —
	// refuses and warns the same way on every node and on every run. A
	// two-rename edit that collided would otherwise name a different tag
	// in its refusal each time it was retried.
	for _, raw := range sortedKeys(edit.Rename) {
		slug := TagSlug(raw)
		at, exists := bySlug[slug]
		if !exists {
			return TagSet{}, nil, nil, false, fmt.Errorf("tracker: %s has no tag "+
				"%q — a rename names the tag by its SLUG, which never changes "+
				"because it is what every task's row holds",
				current.Project, raw)
		}
		label := strings.TrimSpace(edit.Rename[raw])
		switch {
		case label == "":
			return TagSet{}, nil, nil, false, fmt.Errorf("tracker: renaming %s to "+
				"nothing would leave a tag nobody can read", slug)
		case len(label) > MaxTagLabel:
			return TagSet{}, nil, nil, false, fmt.Errorf("tracker: renaming %s to "+
				"%d characters is past the %d a label carries",
				slug, len(label), MaxTagLabel)
		}
		if other, clash := taken[strings.ToLower(label)]; clash && other != at {
			return TagSet{}, nil, nil, false, fmt.Errorf("tracker: %s already has a "+
				"tag %s labelled %q", current.Project,
				next.Tags[other].Slug, next.Tags[other].Label)
		}
		if next.Tags[at].Label == label {
			continue
		}
		delete(taken, strings.ToLower(next.Tags[at].Label))
		taken[strings.ToLower(label)] = at
		// THE SLUG STAYS TAKEN. A rename never frees the old label for
		// re-use by a different tag in the same edit, because the slug
		// it still answers to is that word.
		taken[next.Tags[at].Slug] = at
		next.Tags[at].Label = label
		changed = true
	}

	for _, raw := range sortedStrings(edit.Archive) {
		slug := TagSlug(raw)
		at, exists := bySlug[slug]
		if !exists {
			return TagSet{}, nil, nil, false, fmt.Errorf("tracker: %s has no tag %q",
				current.Project, raw)
		}
		if next.Tags[at].Archived {
			continue
		}
		next.Tags[at].Archived = true
		changed = true
	}

	if !changed {
		return current, declared, warnings, false, nil
	}
	// THE VERSION MOVES ON EVERY CHANGE, because it is what a task's own
	// tag rows are stamped with: the applier re-explodes a task's tags
	// against the set, and a set that changed without saying so would leave
	// every task carrying the old spelling.
	next.TagsVersion = current.TagsVersion + 1
	next.UpdatedAt = at
	return next, declared, warnings, true, nil
}

// sortedKeys and sortedStrings make a map's and a slice's traversal order the
// same on every node and every retry.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return out
}

// TagSlug normalises what somebody typed into what the column stores.
//
// ONE SPELLING, because a tag reaches the rows through `tracker_task_tags.slug`
// and a filter compares it exactly: `Regression` and `regression` are one
// grouping, and two spellings of it are two columns on a board.
//
// IT NORMALISES AND NEVER TRUNCATES. A slug past the grammar's own 64
// characters is refused naming it rather than silently cut, because a cut slug
// is a DIFFERENT tag that looks like the one somebody asked for.
func TagSlug(raw string) string {
	out := make([]rune, 0, len(raw))
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		case r == ' ', r == '_', r == '.', r == '/':
			// A SEPARATOR BECOMES A HYPHEN rather than vanishing, so
			// "needs design" and "needsdesign" are not one tag: a tag
			// somebody typed with a space is a two-word grouping.
			// Underscore is here because the OTHER slug grammar uses
			// it, and a model that typed a status-shaped slug should
			// get the tag it meant rather than one word.
			out = append(out, '-')
		}
	}
	// COLLAPSED, so "needs  design" and "needs design" are one tag rather
	// than two that differ by a character nobody can see.
	collapsed := make([]rune, 0, len(out))
	for i, r := range out {
		if r == '-' && i > 0 && out[i-1] == '-' {
			continue
		}
		collapsed = append(collapsed, r)
	}
	return strings.Trim(string(collapsed), "-")
}

// NearTagDistance is how close a new slug has to be to an existing one to be
// worth saying something about.
//
// TWO, which is the distance a typo travels: a transposition and a doubled
// letter are one edit each, and `api`/`apis` — the pair the advisory rule
// exists for — is one. Three would put every three-letter tag within reach of
// every other, and one would miss `recieved` beside `received`.
const NearTagDistance = 2

// NearTagsNamed is how many a warning lists.
//
// THREE, the same as every other "nearest" this surface reports — a project
// refusal names three keys and a catalogue refusal three slugs — because a
// list somebody is meant to act on immediately is a list they can hold.
const NearTagsNamed = 3

// nearTags is the tags a new slug is within a typo of, nearest first.
//
// OVER THE SLUGS AND NOT THE LABELS, because a slug is what a filter compares
// and what a task's row holds: two labels that read differently but slug the
// same are already refused above, and two labels a person would not confuse
// are not what splits a board.
func nearTags(tags []Tag, slug string) []string {
	type near struct {
		slug string
		d    int
	}
	var found []near
	for _, tag := range tags {
		if tag.Archived {
			// AN ARCHIVED TAG IS NOT A NEAR MISS. It takes no new work
			// by design, so declaring something beside it is the
			// ordinary way a renamed grouping starts.
			continue
		}
		if d := editDistance(tag.Slug, slug, NearTagDistance); d > 0 {
			found = append(found, near{tag.Slug, d})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].d != found[j].d {
			return found[i].d < found[j].d
		}
		return found[i].slug < found[j].slug
	})
	out := make([]string, 0, NearTagsNamed)
	for _, f := range found {
		if len(out) == NearTagsNamed {
			break
		}
		out = append(out, f.slug)
	}
	return out
}

// editDistance is Levenshtein, bounded: it answers -1 rather than the true
// distance once the distance is known to exceed max.
//
// BOUNDED BECAUSE THE ANSWER IS A THRESHOLD. Every caller asks "is this within
// two edits", never "how far is it", so the length check below discards most
// candidates without allocating at all — which is what keeps a declare against
// a 512-tag set from being 512 full matrices.
//
// TWO ROWS RATHER THAN A MATRIX for the same reason: the recurrence reads only
// the previous row, so the full matrix is a table nobody looks at.
func editDistance(a, b string, max int) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) < len(br) {
		ar, br = br, ar
	}
	if len(ar)-len(br) > max {
		return -1
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		best := curr[0]
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
			best = min(best, curr[j])
		}
		if best > max {
			// EVERY REMAINING ROW IS AT LEAST THIS BIG, because a
			// row's minimum never decreases: the recurrence only ever
			// adds. So the answer is already out of range.
			return -1
		}
		prev, curr = curr, prev
	}
	if prev[len(br)] > max {
		return -1
	}
	return prev[len(br)]
}

// normaliseTags turns what a caller typed into what the rows store, and
// refuses a set too long to render.
//
// PURE AND BEFORE THE SNAPSHOT, because it is about the ARGUMENT rather than
// about the project: `Regression`, `regression` and ` regression ` are one tag
// however the company declared it, and a caller that sent all three sent one.
// The declared check is separate and has to read, which is why the two are not
// one function.
func normaliseTags(project string, raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, r := range raw {
		slug := TagSlug(r)
		switch {
		case slug == "":
			return nil, fmt.Errorf("tracker: %q is not a tag — a tag is "+
				"lowercase letters, digits and hyphens, and this normalises "+
				"to nothing", r)
		case !ValidTagSlug(slug):
			return nil, fmt.Errorf("tracker: %q is not a tag slug — a tag "+
				"starts with a letter or a digit and carries lowercase "+
				"letters, digits and hyphens, up to 64 characters", slug)
		case seen[slug]:
			// A DUPLICATE IS DROPPED RATHER THAN REFUSED. `API` beside
			// `api` is one tag typed twice, and refusing it would make
			// a caller normalise before it could write.
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	if len(out) > MaxTagsPerTask {
		return nil, fmt.Errorf("tracker: %d tags on one task in %s and at most "+
			"%d are carried — a task filed under more groupings than that is "+
			"not grouped", len(out), project, MaxTagsPerTask)
	}
	return out, nil
}

// declaredTags refuses a task carrying a tag its project has not declared.
//
// # Why a task write does not just create the tag
//
// Because the two failures look identical at the call and are opposite in
// effect. A seat that MEANT to declare `regression` and a seat that typo'd
// `regresion` both send one unknown slug, and a write that declared whichever
// arrived would fill a board's filter strip with every misspelling anybody ever
// typed — which is exactly what happened to the task TYPE catalogue before
// [declaredType] existed, leaving `Bug`, `bugfix` and `BUG` beside `bug`.
//
// So an unknown tag is refused, and the refusal names both ways out: declare it
// deliberately, or say so at the write. Adding one is open to every seat — this
// is a check on INTENT and never on authority.
//
// AN ARCHIVED TAG IS REFUSED FOR NEW WORK and left alone on old, which is what
// archiving a tag is FOR: the tasks already under it still render and still
// filter.
func declaredTags(ctx context.Context, tx *sql.Tx, project string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	set, held, err := readTagSet(ctx, tx, project)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	archived := map[string]bool{}
	if held {
		for _, tag := range set.Tags {
			if tag.Archived {
				archived[tag.Slug] = true
				continue
			}
			live[tag.Slug] = true
		}
	}
	var unknown []string
	for _, slug := range tags {
		switch {
		case live[slug]:
		case archived[slug]:
			return fmt.Errorf("tracker: the tag %s is archived in %s, so no "+
				"new work is filed under it — the tasks already under it keep "+
				"it", slug, project)
		default:
			unknown = append(unknown, slug)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	declared := make([]string, 0, len(live))
	for slug := range live {
		declared = append(declared, slug)
	}
	sort.Strings(declared)
	near := nearTags(set.Tags, unknown[0])
	hint := ""
	if len(near) > 0 {
		hint = fmt.Sprintf(" Did you mean %s?", strings.Join(near, " or "))
	}
	return fmt.Errorf("tracker: %s does not declare the tag %v — its tags are "+
		"%v.%s Declare one with write_project(tags.add), or pass "+
		"`labels_create_missing` to declare it at this write",
		project, unknown, declared, hint)
}

// EnsureTags declares the tags a write is about to use that the project lacks.
//
// THE INLINE HALF of the rule [declaredTags] states: a seat that meant to
// declare a grouping says so at the write and gets it, and a seat that typo'd
// is refused. It returns the slugs it actually created, so a tool can report
// them — a create the caller did not expect is a typo they can still fix.
//
// ONE APPEND FOR THE WHOLE SET, before the task's own: a record per new tag
// would contend N times on the one subject every seat in the project writes.
func (w *Writer) EnsureTags(ctx context.Context, opID, project string,
	tags []string) ([]string, []string, error) {

	if len(tags) == 0 {
		return nil, nil, nil
	}
	project = ProjectKey(project)
	set, err := w.tagsOf(ctx, project)
	if err != nil {
		return nil, nil, err
	}
	have := map[string]bool{}
	for _, tag := range set.Tags {
		have[tag.Slug] = true
	}
	var add []Tag
	var created []string
	for _, slug := range tags {
		if have[slug] {
			continue
		}
		have[slug] = true
		// THE SLUG IS THE LABEL. A tag declared inline was typed as a
		// word rather than chosen from a list, so there is no other
		// spelling of it to carry — and a lead renaming it later moves
		// the label without touching what the rows hold.
		add = append(add, Tag{Slug: slug, Label: slug})
		created = append(created, slug)
	}
	if len(add) == 0 {
		return nil, nil, nil
	}
	// AUTHORITY IS THE ADD's, which every seat holds. An inline declare
	// renames and archives nothing, so it can never reach the lead gate.
	result, err := w.WriteTags(ctx, opID, project, TagEdit{Add: add},
		TagAuthority{})
	if err != nil {
		return nil, nil, fmt.Errorf("tracker: declare %v in %s: %w",
			created, project, err)
	}
	// WHAT THE WRITE DECLARED, never what the read above guessed: the read
	// is outside any snapshot, so a colleague declaring the same word in
	// between would leave this call claiming to have created a tag it
	// found. The pre-read's only job is to skip the publish entirely when
	// nothing is missing.
	return result.Declared, result.Warnings, nil
}
