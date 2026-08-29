package confluence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/knowledge"
)

// Publishing a directory of authored markdown into spaces.
//
// # One walk, two destinations, decided by the file
//
// A file whose frontmatter declares a `trigger:` is a TOOL SKILL and goes to
// the skills space with the leading code block the engine parses back out.
// Everything else is a KNOWLEDGE DOC and goes to the space named by its
// parent directory, as prose the query-time search returns.
//
// The routing is the FILE'S, not the directory's, because a skill is
// identified by what it declares — an operator who files one under `ENG/`
// still means a skill, and publishing it there as prose would put an
// instruction meant for one phase of one turn into every planner's context.
//
// # Identity is the TITLE, and that is Confluence's limitation not a choice
//
// The other knowledge backend stamps an external id on every page it
// publishes and matches on that, so retitling a page in the UI never orphans
// it. Confluence has no such field: a page is identified by its title within
// its space, so a re-import after somebody renamed a page publishes a second
// one. Reported rather than worked around — a hidden marker page or a label
// convention would be this tool inventing state the instance does not have.

// Item is one file, routed.
type Item struct {
	Path     string
	Space    string
	Title    string
	Storage  string
	Skill    bool
	SkillKey string
}

// Plan is what an import intends to write.
type Plan struct {
	Items []Item
	Notes []string
}

// Spaces are the space keys this plan writes into, sorted.
func (p *Plan) Spaces() []string {
	seen := map[string]bool{}
	for _, item := range p.Items {
		seen[strings.ToUpper(item.Space)] = true
	}
	out := make([]string, 0, len(seen))
	for space := range seen {
		out = append(out, space)
	}
	sort.Strings(out)
	return out
}

// Walk reads a directory tree into a plan.
func Walk(root, skillsSpace string) (*Plan, error) {
	paths, err := knowledge.CollectMarkdown(root)
	if err != nil {
		return nil, err
	}
	plan := &Plan{}
	seen := map[string]string{}
	for _, path := range paths {
		item, err := route(path, skillsSpace)
		if err != nil {
			return nil, err
		}
		if item.Space == "" {
			if item.Skill {
				// TOOL SKILLS ARE OFF for this company. Filing the page
				// under its parent directory's space instead would put
				// an instruction written for one phase of one turn into
				// every planner's knowledge search — the exact thing the
				// skills space exists to keep out.
				return nil, fmt.Errorf(
					"confluence: %s declares a tool-skill trigger but this "+
						"company has no tool-skills space — "+
						"integrations.confluence.skills_space is set to the "+
						"empty string, which turns tool skills off. Pass "+
						"-space KEY to publish it anyway, or remove that "+
						"setting", path)
			}
			return nil, fmt.Errorf(
				"confluence: %s is not under a space directory — a page "+
					"with no space has nowhere to go", path)
		}
		key := strings.ToUpper(item.Space) + "\x00" + strings.ToLower(item.Title)
		if other, clash := seen[key]; clash {
			// TWO FILES, ONE PAGE. The second would overwrite the first
			// on every run, and which one wins depends on walk order —
			// so the published page would flip between them with nothing
			// reporting it.
			return nil, fmt.Errorf(
				"confluence: %s and %s both publish as %q in %s — one of them "+
					"has to change its title", other, item.Path, item.Title, item.Space)
		}
		seen[key] = item.Path
		plan.Items = append(plan.Items, item)
	}
	if len(plan.Items) == 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"no markdown files under %s, so there is nothing to publish", root))
	}
	return plan, nil
}

// route decides what one file is and renders it.
func route(path, skillsSpace string) (Item, error) {
	doc, err := knowledge.ParseDoc(path)
	if err != nil {
		return Item{}, err
	}
	// THE WHOLE FILE, frontmatter included, is what the skill parser
	// reads — ParseDoc strips it, so the raw text is re-read here rather
	// than reassembled from parts that could differ.
	raw, err := os.ReadFile(path)
	if err != nil {
		return Item{}, fmt.Errorf("confluence: read %s: %w", path, err)
	}
	if !skills.IsSkill(string(raw)) {
		storage, renderErr := knowledge.RenderMarkdown(doc.Markdown)
		if renderErr != nil {
			return Item{}, fmt.Errorf("confluence: %s: %w", path, renderErr)
		}
		return Item{
			Path: path, Space: strings.ToUpper(doc.Container),
			Title: doc.Title, Storage: storage,
		}, nil
	}

	skill, err := skills.Parse(string(raw), skills.Source{})
	if err != nil {
		return Item{}, fmt.Errorf("confluence: %s: %w", path, err)
	}
	body, err := knowledge.RenderMarkdown(skill.Body)
	if err != nil {
		return Item{}, fmt.Errorf("confluence: %s: %w", path, err)
	}
	frontmatter := knowledge.FrontmatterPattern.FindStringSubmatch(string(raw))[1]
	title := skill.Title
	if title == "" {
		title = skill.Key
	}
	return Item{
		Path: path, Space: strings.ToUpper(strings.TrimSpace(skillsSpace)),
		Title: title, Storage: EncodeSkillPage(frontmatter, body),
		Skill: true, SkillKey: skill.Key,
	}, nil
}

// PublishResult is what one import did.
type PublishResult struct {
	Created []string
	Updated []string
	// Pruned names the orphaned skill pages this run deleted.
	Pruned []string
	// Failed names the pages that could not be written, with why. Page
	// failures are ISOLATED — a restricted page or one 403 must not cost
	// the other forty — and reported, so the run's exit code is honest.
	Failed []string
	Notes  []string
}

// PublishOptions are one import's inputs.
type PublishOptions struct {
	Client *Client
	Plan   *Plan

	// Prune deletes skill pages in the skills space that this tool
	// published and no local file publishes any more. Skills only, and
	// only labelled pages: a knowledge doc removed from the tree is far
	// more likely to have moved than to be dead, and an unlabelled page
	// was somebody's own.
	Prune bool

	// SkillsSpace is the one space a prune may touch. Empty means no
	// prune, whatever Prune says — a prune with no space to scope it
	// would be a delete pass over the whole instance.
	SkillsSpace string
}

// Publish writes the plan into the instance.
//
// EVERY SPACE IS CHECKED FIRST, before a single page is written: a typo in a
// directory name would otherwise be discovered half way through, leaving an
// operator to work out which pages landed. The importer never CREATES a
// space — that names a container the whole company then works in, and
// guessing it is not this command's job.
func Publish(ctx context.Context, opts PublishOptions) (*PublishResult, error) {
	if opts.Client == nil {
		return nil, errors.New("confluence: no client")
	}
	if opts.Plan == nil || len(opts.Plan.Items) == 0 {
		return &PublishResult{Notes: notesOf(opts.Plan)}, nil
	}
	res := &PublishResult{Notes: notesOf(opts.Plan)}
	for _, space := range opts.Plan.Spaces() {
		exists, err := opts.Client.SpaceExists(ctx, space)
		if err != nil {
			return nil, fmt.Errorf(
				"confluence: space %s could not be read, so nothing was "+
					"published: %w", space, err)
		}
		if !exists {
			return nil, fmt.Errorf(
				"confluence: this instance has no space %s, so nothing was "+
					"published — create it, or fix the directory name. The "+
					"importer never creates a space: that names a container "+
					"the whole company then works in", space)
		}
	}

	// published is the set of skill keys a local file publishes, which is
	// what prune subtracts from the space.
	//
	// FROM THE PLAN, not from the writes that succeeded. The question
	// prune asks is "does a source file still publish this skill?", and
	// that is answered by the tree — a page whose update happened to 403
	// on this run has not stopped having a source file, and deleting it
	// would destroy the live copy of a skill that this run could not even
	// rewrite.
	published := make(map[string]bool, len(opts.Plan.Items))
	for _, item := range opts.Plan.Items {
		if item.Skill {
			published[strings.ToLower(strings.TrimSpace(item.SkillKey))] = true
		}
	}
	for _, item := range opts.Plan.Items {
		existing, found, err := opts.Client.PageByTitle(ctx, item.Space, item.Title)
		if err != nil {
			res.Failed = append(res.Failed, item.Title+": "+err.Error())
			continue
		}
		var id string
		if !found {
			created, err := opts.Client.CreatePage(ctx, item.Space, item.Title, item.Storage, "")
			if err != nil {
				res.Failed = append(res.Failed, item.Title+": "+err.Error())
				continue
			}
			id = created.ID
			res.Created = append(res.Created, item.Space+"/"+item.Title)
		} else {
			if _, err := opts.Client.UpdatePage(ctx, existing.ID, item.Title,
				item.Storage, existing.Version); err != nil {
				res.Failed = append(res.Failed, item.Title+": "+err.Error())
				continue
			}
			id = existing.ID
			res.Updated = append(res.Updated, item.Space+"/"+item.Title)
		}
		if !item.Skill {
			continue
		}
		// STAMPED ON EVERY RUN, not only on create: a page this tool
		// wrote before the label existed, or one an operator recreated
		// by hand under the same title, is adopted by the next import
		// rather than left permanently unprunable.
		if err := opts.Client.AddLabel(ctx, id, ImportedSkillLabel); err != nil {
			// NOT a page failure: the page is published and correct.
			// What is lost is the ability to prune it later, which is
			// a note rather than a non-zero exit.
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s/%s was published but could not be labelled %s (%v), so a "+
					"later -prune will leave it alone even when its source "+
					"file is gone — re-run this import to try again",
				item.Space, item.Title, ImportedSkillLabel, err))
		}
	}

	if opts.Prune {
		pruned, notes, err := pruneSkills(ctx, opts, published)
		res.Pruned = append(res.Pruned, pruned...)
		res.Notes = append(res.Notes, notes...)
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// pruneSkills deletes the skill pages this tool published that no local file
// publishes any more.
//
// # A prune that cannot enumerate deletes NOTHING
//
// The orphan set is "every labelled skill page in the space, minus the ones
// this run wrote", so it is derived by subtraction — and a truncated or
// failed walk makes the first half smaller, which makes the orphan set
// larger and deletes live pages. The walk is therefore all-or-nothing and
// its failure stops the prune rather than shrinking it.
//
// # Three conditions, all of them required
//
// A page is deleted only when it is in the skills space, carries
// [ImportedSkillLabel], and parses as a skill whose key this run did not
// publish. The label is what protects a lead's hand-authored page; the parse
// is what protects an ordinary page somebody filed in the skills space; the
// key comparison is what makes a rename a delete-and-create rather than a
// silent duplicate.
func pruneSkills(ctx context.Context, opts PublishOptions,
	published map[string]bool,
) ([]string, []string, error) {
	space := strings.ToUpper(strings.TrimSpace(opts.SkillsSpace))
	if space == "" {
		return nil, []string{
			"-prune did nothing: this company has no tool-skills space, so " +
				"there is no container a prune could be scoped to",
		}, nil
	}
	pages, err := opts.Client.PagesIn(ctx, space)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"confluence: %s could not be enumerated, so nothing was pruned — "+
				"a prune subtracts what this run published from what the "+
				"space holds, and a partial read of the space deletes live "+
				"pages: %w", space, err)
	}
	// SORTED, so a run's output and its order of deletion are the same
	// every time: map iteration would make a partially failed prune
	// delete a different subset on each attempt.
	ordered := slices.Clone(pages)
	slices.SortFunc(ordered, func(a, b Page) int { return strings.Compare(a.ID, b.ID) })

	var pruned, notes []string
	for _, page := range ordered {
		if !page.HasLabel(ImportedSkillLabel) {
			continue
		}
		text := DecodeSkillPage(page.Body)
		if !skills.IsSkill(text) {
			continue
		}
		skill, err := skills.Parse(text, skills.Source{})
		if err != nil {
			// IT DECLARES A TRIGGER AND DOES NOT PARSE, so its key is
			// unknown and it cannot be matched against this run. Deleting
			// it would be deleting a page on the strength of a guess.
			notes = append(notes, fmt.Sprintf(
				"%s/%s declares a trigger and does not parse, so -prune could "+
					"not tell which skill it is and left it alone: %v",
				space, page.Title, err))
			continue
		}
		if published[strings.ToLower(strings.TrimSpace(skill.Key))] {
			continue
		}
		if err := opts.Client.DeletePage(ctx, page.ID); err != nil {
			notes = append(notes, fmt.Sprintf(
				"%s/%s is orphaned and could not be deleted (%v) — it stays in "+
					"every planner's tool-skill catalogue until it is removed "+
					"by hand", space, page.Title, err))
			continue
		}
		pruned = append(pruned, space+"/"+page.Title)
	}
	return pruned, notes, nil
}

// Promote publishes an auto-drafted skill by moving it out of the drafts
// parent — which IS the review gesture, and is why the read-side exclusion
// keys on the ancestor chain rather than on a flag somebody has to remember
// to clear.
//
// The TITLE PREFIX is cleared with it, because it is the fail-closed
// backstop for a lookup that could not read the chain: leaving it on a
// published page would keep hiding a skill a lead deliberately approved.
func Promote(ctx context.Context, client *Client, pageID string) (Page, error) {
	if client == nil {
		return Page{}, errors.New("confluence: no client")
	}
	page, err := client.PageByID(ctx, pageID)
	if err != nil {
		return Page{}, err
	}
	title := strings.TrimPrefix(page.Title, knowledge.AutoDraftTitlePrefix)
	if err := client.MovePage(ctx, page.ID, title, page.Version, ""); err != nil {
		return Page{}, err
	}
	page.Title = title
	page.Ancestors = nil
	page.Version++
	return page, nil
}

// SkillPages reads a space's pages as the registry's own shape.
func SkillPages(ctx context.Context, client *Client, space string) ([]skills.Page, error) {
	pages, err := client.PagesIn(ctx, space)
	if err != nil {
		return nil, err
	}
	out := make([]skills.Page, 0, len(pages))
	for _, page := range pages {
		out = append(out, skills.Page{
			ID: page.ID, Title: page.Title,
			// The DECODED text, so the skills package stays the one
			// place that decides what a skill is and this one stays the
			// only place that knows how a page is shaped.
			Text: DecodeSkillPage(page.Body),
		})
	}
	return out, nil
}

func notesOf(plan *Plan) []string {
	if plan == nil {
		return nil
	}
	return plan.Notes
}
