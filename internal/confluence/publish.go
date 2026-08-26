package confluence

import (
	"context"
	"errors"
	"fmt"
	"os"
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
			return nil, fmt.Errorf(
				"confluence: %s is not under a space directory, and a skill "+
					"needs integrations.confluence.skills_space set — a page "+
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

	for _, item := range opts.Plan.Items {
		existing, found, err := opts.Client.PageByTitle(ctx, item.Space, item.Title)
		if err != nil {
			res.Failed = append(res.Failed, item.Title+": "+err.Error())
			continue
		}
		if !found {
			if _, err := opts.Client.CreatePage(ctx, item.Space, item.Title, item.Storage, ""); err != nil {
				res.Failed = append(res.Failed, item.Title+": "+err.Error())
				continue
			}
			res.Created = append(res.Created, item.Space+"/"+item.Title)
			continue
		}
		if _, err := opts.Client.UpdatePage(ctx, existing.ID, item.Title,
			item.Storage, existing.Version); err != nil {
			res.Failed = append(res.Failed, item.Title+": "+err.Error())
			continue
		}
		res.Updated = append(res.Updated, item.Space+"/"+item.Title)
	}
	return res, nil
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
