package worklet

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// A project's tag set: the one catalogue any seat may add to.
//
// Adding is open to every seat: a tag groups work for a week, and tags only a
// person could declare would never be declared. A rename or an archive is the
// lead's: it reaches every task filed under the tag and everybody's board.
//
// An archive is one-way, like a field's: the tasks keep the tag and filters
// still answer, but no new work is filed under it. Bringing one back means
// declaring it again, not re-admitting work against a meaning nobody checked.
//
// The tag set is its own subject, not a field of the project, so a seat's add
// and a lead's policy edit are two arbitration units rather than one.

// TagEdit is one change to a project's tag set.
//
// Three facets rather than a whole set: a verb taking the set would make every
// add a rewrite of everybody else's tags, and could revert a lead's archive.
type TagEdit struct {
	// Add declares tags that do not exist. Naming one that does is not an
	// error: two seats reaching for the same word is the ordinary case.
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

// TagClash is an add or a rename refused because its label is already another
// tag's label or slug in the project: two groupings a person cannot tell
// apart, which the work would split between at random.
//
// A type so a caller several steps away (a cross-project move declaring its
// subtree's tags) can name the tag, the one it clashes with and the project.
// It is [ErrTaken] under [errors.Is], and [Sentence] reads it without the
// package prefix, so a caller ignoring the type still words it as a collision.
type TagClash struct {
	Project string
	// Slug and Label are the tag the add declared, or the rename gave.
	Slug, Label string
	// Other is the project's own tag whose label or slug it collides with.
	Other Tag
}

func (e *TagClash) Error() string { return "worklet: " + e.sentence() }

// Unwrap classes the clash as the collision it is.
func (e *TagClash) Unwrap() error { return ErrTaken }

// sentence names the tag that holds the label and the two ways past it: the
// remedy, not the reason.
func (e *TagClash) sentence() string {
	return fmt.Sprintf("%s already has a tag labelled %q (%s). Use that tag if "+
		"it means the same, or give %s a label of its own", e.Project,
		e.Other.Label, e.Other.Slug, e.Slug)
}

// TagsFull is an add refused because the project already keeps
// [MaxTagsPerProject] tags — a type for [TagClash]'s reason.
type TagsFull struct {
	Project string
	// Slug is the first tag the add could not fit.
	Slug string
}

func (e *TagsFull) Error() string { return "worklet: " + e.sentence() }

// Unwrap classes the full set as the PROJECT'S STATE ([ErrState]): the add
// is well formed, and it lands once a tag is archived to make room.
func (e *TagsFull) Unwrap() error { return ErrState }

func (e *TagsFull) sentence() string {
	return fmt.Sprintf("%s already has %d tags, the most a project can. "+
		"Archive one it no longer files under first", e.Project, MaxTagsPerProject)
}

// TagAuthority is what a caller may do to a tag set: a value rather than a
// bool on the writer, since the answer is about a pair (who asks, about which
// project), and it states the decision rather than its inputs, as
// [ProjectAuthority] does.
type TagAuthority struct {
	// Policy reports a caller authorized to RENAME or ARCHIVE a tag of
	// this project: its lead, or an admin. Adding takes nothing: the zero
	// value is what an inline declare passes.
	Policy bool
}

// WriteTags applies one edit to a project's tag set.
//
// The caveats it returns (the near-duplicates it declared) are ADVISORY: a
// refusal with no override would block `apis` behind `api` for ever. The apply
// must never depend on one: every data node applies the record, and only this
// node saw the caveat.
func (w *Writer) WriteTags(ctx context.Context, opID, project string,
	edit TagEdit, authority TagAuthority) (WriteResult, error) {

	project = ProjectKey(project)
	switch {
	case project == "":
		return WriteResult{}, invalid("a tag edit names no project")
	case edit.Empty():
		return WriteResult{}, invalid("a tag edit of %s adds, "+
			"renames and archives nothing", project)
	case !authority.Policy &&
		(len(edit.Rename) > 0 || len(edit.Archive) > 0):
		// The refusal names who can: the seat was tidying up, and the
		// answer is "ask the lead".
		return WriteResult{}, forbidden("renaming or archiving a tag of %s is "+
			"the project lead's, or a person's own: it changes the word on "+
			"every task already filed under it, and takes a filter off "+
			"everybody's board. Adding a tag is open to every seat", project)
	}
	subject := TagsSubject(project)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	var caveats []caveat.Caveat
	var declared []string
	result, err := w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			current, held, err := readTagSet(ctx, tx, project)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !held {
				// A missing tag set is an empty one: `EnsureProjects`
				// does not write it, and the first write self-heals it,
				// as a key counter's first mint does.
				current = TagSet{V: DocumentVersion, Project: project}
			}
			next, added, warned, changed, err := applyTagEdit(current, edit,
				w.Actor, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			// Assigned from the winning attempt, not appended: a
			// re-decide may find a shorter add list or another
			// near-duplicate, and the caller is told what landed.
			caveats, declared = warned, added
			if !changed {
				// Nothing changed (two seats adding one word is the
				// ordinary case): an empty decision, not a conflict.
				return statelog.Decision{}, nil
			}
			decision, err := w.decide(ctx, tx, stamp, subject, OpPatch, ChangeTags, scope, opID,
				next, nil, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
	result.Caveats = caveats
	result.Declared = declared
	return result, err
}

// applyTagEdit is the whole rule, as a pure function over one set, so every
// refusal is testable without a broker.
//
// It reports the next set, the slugs it DECLARED, the advisory caveats, and
// whether anything changed. The declared list comes from here, inside the
// snapshot, not from a read before the write: a list computed outside it names
// tags somebody else declared a moment earlier.
func applyTagEdit(current TagSet, edit TagEdit, actor string, at time.Time) (
	TagSet, []string, []caveat.Caveat, bool, error) {

	next := current
	next.Tags = slices.Clone(current.Tags)
	next.Project = current.Project
	if next.V == 0 {
		next.V = DocumentVersion
	}

	// One index over labels and slugs: a new label colliding with either is
	// the same failure, two groupings a person cannot tell apart.
	taken := map[string]int{}
	bySlug := map[string]int{}
	for i, tag := range next.Tags {
		bySlug[tag.Slug] = i
		taken[strings.ToLower(tag.Label)] = i
		taken[tag.Slug] = i
	}
	changed := false
	var warnings []caveat.Caveat
	var declared []string

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
			return TagSet{}, nil, nil, false, invalid("a tag needs a "+
				"name, and %q normalises to nothing. A tag slug is lowercase "+
				"letters, digits and hyphens", want.Label)
		case !ValidTagSlug(slug):
			return TagSet{}, nil, nil, false, invalid("%q is not a tag "+
				"slug. A tag starts with a letter or a digit and carries "+
				"lowercase letters, digits and hyphens, up to 64 characters",
				slug)
		case len(label) > MaxTagLabel:
			return TagSet{}, nil, nil, false, invalid("the tag %s is "+
				"labelled with %d characters and at most %d are stored. A tag "+
				"is rendered on every row it is on, and one this long pushes "+
				"the title off the screen", slug, len(label), MaxTagLabel)
		}
		if _, exists := bySlug[slug]; exists {
			// Already there is not an error: refusing the second of two
			// seats reaching for one word would make every seat read the
			// set before it could file anything.
			continue
		}
		if other, clash := taken[strings.ToLower(label)]; clash {
			return TagSet{}, nil, nil, false, &TagClash{Project: current.Project,
				Slug: slug, Label: label, Other: next.Tags[other]}
		}
		if len(next.Tags) >= MaxTagsPerProject {
			return TagSet{}, nil, nil, false, &TagsFull{Project: current.Project,
				Slug: slug}
		}
		if near := nearTags(next.Tags, slug); len(near) > 0 {
			warnings = append(warnings, caveat.TagNearDuplicate(current.Project,
				slug, near))
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

	// Sorted, so a rename map (random iteration order) refuses and warns the
	// same way on every node and every retry.
	for _, raw := range sortedKeys(edit.Rename) {
		slug := TagSlug(raw)
		at, exists := bySlug[slug]
		if !exists {
			return TagSet{}, nil, nil, false, invalid("%s has no tag "+
				"%q. A rename names the tag by its SLUG, which never changes "+
				"because it is what every task's row holds",
				current.Project, raw)
		}
		label := strings.TrimSpace(edit.Rename[raw])
		switch {
		case label == "":
			return TagSet{}, nil, nil, false, invalid("renaming %s to "+
				"nothing would leave a tag nobody can read", slug)
		case len(label) > MaxTagLabel:
			return TagSet{}, nil, nil, false, invalid("renaming %s to "+
				"%d characters is past the %d a label carries",
				slug, len(label), MaxTagLabel)
		}
		if other, clash := taken[strings.ToLower(label)]; clash && other != at {
			return TagSet{}, nil, nil, false, &TagClash{Project: current.Project,
				Slug: slug, Label: label, Other: next.Tags[other]}
		}
		if next.Tags[at].Label == label {
			continue
		}
		delete(taken, strings.ToLower(next.Tags[at].Label))
		taken[strings.ToLower(label)] = at
		// The slug stays taken: a rename never frees the old word for
		// another tag, since the slug still answers to it.
		taken[next.Tags[at].Slug] = at
		next.Tags[at].Label = label
		changed = true
	}

	for _, raw := range sortedStrings(edit.Archive) {
		slug := TagSlug(raw)
		at, exists := bySlug[slug]
		if !exists {
			return TagSet{}, nil, nil, false, invalid("%s has no tag %q",
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
	// The version moves on every change: a task's tag rows are stamped with
	// it and re-exploded against the set, so a silent change would leave
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
// One spelling, because a filter compares `tracker_task_tags.slug` exactly. It
// normalises and never truncates: a slug past the grammar's 64 characters is
// refused, since a cut slug is a different tag that looks like the asked one.
func TagSlug(raw string) string {
	out := make([]rune, 0, len(raw))
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		case r == ' ', r == '_', r == '.', r == '/':
			// A separator becomes a hyphen, so "needs design" and
			// "needsdesign" stay two tags. Underscore too: the other slug
			// grammar uses it, and a status-shaped slug gets its tag.
			out = append(out, '-')
		}
	}
	// Collapsed, so a doubled separator nobody can see makes no new tag.
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
// Two, the distance a typo travels: `api`/`apis` is one edit and a
// transposition two, which one would miss; three would put every three-letter
// tag within reach of every other.
const NearTagDistance = 2

// NearTagsNamed is how many a warning lists.
//
// Three, as every other "nearest" this surface reports: a list to act on at
// once is one a person can hold.
const NearTagsNamed = 3

// nearTags is the tags a new slug is within a typo of, nearest first.
//
// Over the slugs, not the labels: a slug is what a filter compares and a row
// holds, and labels that slug the same are already refused.
func nearTags(tags []Tag, slug string) []string {
	type near struct {
		slug string
		d    int
	}
	var found []near
	for _, tag := range tags {
		if tag.Archived {
			// An archived tag is no near miss: declaring beside it is how
			// a renamed grouping starts.
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
// Every caller asks "within two edits", so the length check discards most
// candidates without allocating (a declare against a 512-tag set is not 512
// matrices), and two rows replace the matrix: the recurrence reads only the
// previous one.
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
			// A row's minimum never decreases, so the answer is
			// already out of range.
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
// Pure and before the snapshot: it is about the argument, not the project.
// The declared check has to read, so it is separate ([declaredTags]).
func normaliseTags(project string, raw []string) ([]string, error) {
	out, err := tagSlugs(raw)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxTagsPerTask {
		return nil, invalid("%d tags on one task in %s and at most "+
			"%d are carried. A task filed under more groupings than that is "+
			"not grouped", len(out), project, MaxTagsPerTask)
	}
	return out, nil
}

// tagSlugs is every tag a caller named, as the rows hold it ([TagSlug]), each
// once and in the order first named, or the content refusal ([invalid]) of
// the first that is not a tag. A write ([normaliseTags]) and a filter
// ([Query.parseTags]) share it, so a filter never answers about tags no task
// could carry.
func tagSlugs(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, r := range raw {
		slug := TagSlug(r)
		switch {
		case slug == "":
			return nil, invalid("%q is not a tag. A tag is "+
				"lowercase letters, digits and hyphens, and this normalises "+
				"to nothing", r)
		case !ValidTagSlug(slug):
			return nil, invalid("%q is not a tag slug. A tag "+
				"starts with a letter or a digit and carries lowercase "+
				"letters, digits and hyphens, up to 64 characters", slug)
		case seen[slug]:
			// A duplicate is dropped, not refused: `API` beside `api` is
			// one tag typed twice.
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out, nil
}

// declaredTags refuses a task carrying a tag its project has not declared.
//
// A task write does not just create the tag: a meant `regression` and a typo'd
// `regresion` look identical at the call, and declaring whichever arrived
// fills the filter strip with misspellings (why [declaredType] exists). The
// refusal names both ways out: declare it, or say so at the write. It checks
// intent, never authority. An archived tag is refused for new work and left
// alone on old, which is what archiving is for.
func declaredTags(ctx context.Context, tx *sql.Tx, project string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	set, held, err := readTagSet(ctx, tx, project)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	archived := map[string]string{}
	if held {
		for _, tag := range set.Tags {
			if tag.Archived {
				archived[tag.Slug] = cmp.Or(tag.Label, tag.Slug)
				continue
			}
			live[tag.Slug] = true
		}
	}
	var unknown []string
	for _, slug := range tags {
		switch {
		case live[slug]:
		case archived[slug] != "":
			return invalidSaying(caveat.LabelArchived(project, archived[slug]), "the tag %s is archived in %s, so no "+
				"new work is filed under it. The tasks already under it keep "+
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
		hint = fmt.Sprintf(" Did you mean %s?", words.Or(near))
	}
	// One sentence for both readers, as every content refusal here: a model
	// reads it in a tool's answer and a person on a write surface as it
	// stands, so the remedy names the two arguments that declare a tag and
	// agrees in number with what it is about.
	named, it := "the tag "+words.And(unknown), "it"
	if len(unknown) > 1 {
		named, it = "the tags "+words.And(unknown), "them"
	}
	its := " Its tags are " + words.And(declared) + "."
	if len(declared) == 0 {
		its = " It has no tags yet."
	}
	// A PERSON READS LABELS, the names the project's tags show under, where
	// the sentence above names the slugs a write sends.
	labelOf := map[string]string{}
	if held {
		for _, tag := range set.Tags {
			labelOf[tag.Slug] = cmp.Or(tag.Label, tag.Slug)
		}
	}
	labels := func(slugs []string) []string {
		out := make([]string, len(slugs))
		for i, slug := range slugs {
			out[i] = cmp.Or(labelOf[slug], slug)
		}
		return out
	}
	return invalidSaying(caveat.LabelsNotDeclared(project, unknown, labels(declared), labels(near)),
		"%s doesn't have %s.%s%s Add %s to %s's tags first (`tags_add`), "+
			"or set `labels_create_missing` to add %s as this work is filed",
		project, named, its, hint, it, project, it)
}

// EnsureTags declares the tags a write is about to use that the project lacks:
// the inline half of [declaredTags]'s rule. It returns the slugs it actually
// created, so a tool can report them: an unexpected create is a typo the
// caller can still fix.
//
// One append for the whole set, before the task's own: a record per tag would
// contend N times on the subject every seat in the project writes.
func (w *Writer) EnsureTags(ctx context.Context, opID, project string,
	tags []string) ([]string, []caveat.Caveat, error) {

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
		// The slug is the label: an inline tag was typed as a word, and a
		// lead's later rename moves the label without touching the rows.
		add = append(add, Tag{Slug: slug, Label: slug})
		created = append(created, slug)
	}
	if len(add) == 0 {
		return nil, nil, nil
	}
	// Authority is the add's, which every seat holds. An unknown outcome is
	// a step that did not resolve ([resolved]), never "nothing new": the
	// write after this would then be refused over a tag that may have landed.
	result, err := w.WriteTags(ctx, opID, project, TagEdit{Add: add},
		TagAuthority{})
	if err = resolved(fmt.Sprintf("the declaration of %s in %s", words.And(created),
		project), result, err); err != nil {
		return nil, nil, fmt.Errorf("worklet: declare %s in %s: %w",
			words.And(created), project, err)
	}
	// What the write declared, never what the read above guessed: that read
	// is outside any snapshot, and only skips the publish when nothing is
	// missing.
	return result.Declared, result.Caveats, nil
}

// tagsOf reads a project's tag set outside any decision.
func (w *Writer) tagsOf(ctx context.Context, project string) (TagSet, error) {
	var set TagSet
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		stored, held, err := readTagSet(ctx, tx, project)
		if err != nil {
			return err
		}
		if !held {
			stored = TagSet{V: DocumentVersion, Project: project}
		}
		set = stored
		return nil
	})
	return set, err
}
