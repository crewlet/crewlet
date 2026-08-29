package plane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
)

// Publishing a directory of authored markdown into a workspace.
//
// # One walk, two destinations, decided by the file
//
// A file whose frontmatter declares a `trigger:` is a TOOL SKILL and goes
// to the skills project with the leading YAML block the engine parses back
// out. Everything else is a KNOWLEDGE DOC and goes to the project named by
// its parent directory, as clean prose the query-time search returns.
//
// The routing is the FILE'S, not the directory's, because a skill is
// identified by what it declares — an operator who files one under `ENG/`
// still means a skill, and publishing it there as prose would put an
// instruction meant for one phase of one turn into a planner's context.
//
// # The importer never creates projects
//
// A missing project fails the run BEFORE a single page is written, naming
// what to create. Creating one here would guess a name for a container the
// whole company then works in, and it is one flag away on the provisioner.

// Item is one file, routed.
type Item struct {
	// Path is where it came from, for the report.
	Path string
	// Container is the project identifier it belongs in.
	Container string
	// Title is the page name.
	Title string
	// ExternalID is its identity at the backend.
	ExternalID string
	// HTML is the body to publish.
	HTML string
	// Skill marks a tool-skill page, which the prune predicate needs:
	// only skills are ever deleted.
	Skill bool
}

// Plan is a walk's result, before anything is published.
type Plan struct {
	Items []Item
	Notes []string
}

// Containers is every project the plan writes to, sorted.
func (p *Plan) Containers() []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range p.Items {
		key := strings.ToUpper(item.Container)
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Walk routes every markdown file under root.
//
// # A file that cannot be published stops the walk
//
// Unlike a page-level failure, a malformed file is a thing the operator
// fixes in their editor before running again — and a run that skipped it
// would report success while silently leaving a skill unpublished, which
// looks from the agent's side like a skill nobody wrote.
func Walk(root string, cfg *config.Plane, skillsProject string) (*Plan, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, errors.New("plane: the company config does not enable plane")
	}
	paths, err := knowledge.CollectMarkdown(root)
	if err != nil {
		return nil, err
	}
	plan := &Plan{}
	seen := map[string]string{}
	for _, path := range paths {
		item, err := route(path, skillsProject)
		if err != nil {
			return nil, err
		}
		key := strings.ToUpper(item.Container) + "\x00" + item.ExternalID
		if other, clash := seen[key]; clash {
			// TWO FILES, ONE IDENTITY. The second would overwrite the
			// first on every run, and which one wins depends on walk
			// order — so the published page would flip between them
			// with nothing reporting it.
			return nil, fmt.Errorf(
				"plane: %s and %s both publish as %q in %s — one of them has "+
					"to change its title or its key",
				other, item.Path, item.ExternalID, item.Container)
		}
		seen[key] = item.Path
		plan.Items = append(plan.Items, item)
	}
	plan.Notes = append(plan.Notes, unsupportedFrontmatter(paths)...)
	if len(plan.Items) == 0 {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"no markdown files under %s, so there is nothing to publish", root))
	}
	return plan, nil
}

// route decides what one file is and renders it.
func route(path, skillsProject string) (Item, error) {
	doc, err := knowledge.ParseDoc(path)
	if err != nil {
		return Item{}, err
	}
	// THE WHOLE FILE, frontmatter included, is what the skill parser
	// reads — ParseDoc strips it, so the raw text is re-read here rather
	// than reassembled from parts that could differ.
	raw, err := readFile(path)
	if err != nil {
		return Item{}, err
	}
	if !skills.IsSkill(raw) {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		html, err := knowledge.RenderMarkdown(doc.Markdown)
		if err != nil {
			return Item{}, fmt.Errorf("plane: %s: %w", path, err)
		}
		return Item{
			Path: path, Container: doc.Container, Title: doc.Title,
			ExternalID: DocExternalID(doc.Title), HTML: html,
		}, nil
	}

	skill, err := skills.Parse(raw, skills.Source{})
	if err != nil {
		return Item{}, fmt.Errorf("plane: %s: %w", path, err)
	}
	body, err := knowledge.RenderMarkdown(skill.Body)
	if err != nil {
		return Item{}, fmt.Errorf("plane: %s: %w", path, err)
	}
	frontmatter := knowledge.FrontmatterPattern.FindStringSubmatch(raw)[1]
	title := skill.Title
	if title == "" {
		title = skill.Key
	}
	if skillsProject == "" {
		// TOOL SKILLS ARE OFF for this company, and a skill file has
		// nowhere to go. Publishing it into its parent directory's
		// project instead would put an instruction written for one phase
		// of one turn into every planner's knowledge search — the exact
		// thing the exclusion exists to prevent.
		return Item{}, fmt.Errorf(
			"plane: %s declares a tool-skill trigger but this company has no "+
				"tool-skills project — integrations.plane.skills_project is "+
				"set to the empty string, which turns tool skills off. Pass "+
				"-project ID to publish it anyway, or remove that setting",
			path)
	}
	return Item{
		Path: path, Container: skillsProject, Title: title,
		ExternalID: SkillExternalID(skill.Key),
		HTML:       EncodeSkillPage(frontmatter, body), Skill: true,
	}, nil
}

// PublishResult is what one import did.
type PublishResult struct {
	Created []string
	Updated []string
	Pruned  []string
	// Failed names the pages that could not be written, with why. Page
	// failures are ISOLATED — a locked page or one 403 must not cost the
	// other forty — and reported, so the run's exit code can be honest.
	Failed []string
	Notes  []string
}

// PublishOptions are one import's inputs.
type PublishOptions struct {
	Client *Client
	Config *config.Plane
	Plan   *Plan

	// Prune deletes managed SKILL pages whose key no local file
	// publishes. Skills only, and only pages this tool marked: a
	// knowledge doc removed from the tree is far more likely to have
	// moved than to be dead, and an unmarked page was somebody's own.
	Prune bool
}

// Publish writes the plan into the workspace.
func Publish(ctx context.Context, opts PublishOptions) (*PublishResult, error) {
	if opts.Client == nil {
		return nil, errors.New("plane: no client")
	}
	if opts.Plan == nil || len(opts.Plan.Items) == 0 {
		return &PublishResult{Notes: notesOfPlan(opts.Plan)}, nil
	}
	projects, err := resolveContainers(ctx, opts)
	if err != nil {
		return nil, err
	}

	// ONE ENUMERATION PER PROJECT, before any write: the index decides
	// create-versus-update for every item in it, and re-reading per item
	// would be a request per file for an answer that does not change.
	index := map[string]map[string]PageRef{}
	for identifier, project := range projects {
		pages, err := opts.Client.PageIndex(ctx, project.ID)
		if err != nil {
			return nil, fmt.Errorf("plane: enumerate %s: %w", identifier, err)
		}
		index[identifier] = byIdentity(pages)
	}

	res := &PublishResult{Notes: notesOfPlan(opts.Plan)}
	published := map[string]map[string]bool{}
	for _, item := range opts.Plan.Items {
		container := strings.ToUpper(item.Container)
		project := projects[container]
		existing, found := lookup(index[container], item)
		var err error
		switch {
		case found:
			err = opts.Client.UpdatePage(ctx, project.ID, existing.ID,
				item.Title, item.HTML, item.ExternalID)
			if err == nil {
				res.Updated = append(res.Updated, item.Title)
			}
		default:
			_, err = opts.Client.CreatePage(ctx, project.ID,
				item.Title, item.HTML, item.ExternalID)
			if err == nil {
				// The created page is deliberately NOT written
				// back into the index. The index answers
				// create-versus-update for the items still to
				// come, and no later item can match this page:
				// Walk refuses a plan whose files collide on one
				// page, so two items in one container never share
				// an external id or a title. Re-indexing here
				// would be machinery for a state the planner
				// makes unreachable.
				res.Created = append(res.Created, item.Title)
			}
		}
		if err != nil {
			// ISOLATED: one page's failure costs one page. The run
			// keeps going and the exit code carries the truth.
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", item.Path, err))
			continue
		}
		if published[container] == nil {
			published[container] = map[string]bool{}
		}
		published[container][item.ExternalID] = true
	}

	if opts.Prune {
		pruned, failed := prune(ctx, opts, projects, index, published)
		res.Pruned = append(res.Pruned, pruned...)
		res.Failed = append(res.Failed, failed...)
	}
	return res, nil
}

// byIdentity indexes a project's pages by external id and by name.
//
// BOTH, because the name is the ADOPTION path: a page an operator created
// by hand under the same title carries no external id, and matching it lets
// this run take it over instead of publishing a second page beside it.
//
// # Only an UNCLAIMED page is adoptable
//
// A page carrying any external identity belongs to whoever set it — this
// tool under a different id, or another tool whose ids happen to look like
// these. Adopting one would overwrite somebody else's page with this
// company's content, and on a re-run the two would fight over it for ever.
// So the name index holds only pages with no external identity at all.
func byIdentity(pages []PageRef) map[string]PageRef {
	// SORTED, so two pages sharing a title resolve the same way on every
	// run: map iteration order would otherwise make the adopted page a
	// coin flip.
	ordered := slices.Clone(pages)
	slices.SortFunc(ordered, func(a, b PageRef) int { return strings.Compare(a.ID, b.ID) })

	out := make(map[string]PageRef, len(ordered)*2)
	for _, page := range ordered {
		if page.Managed() && page.ExternalID != "" {
			out[page.ExternalID] = page
		}
	}
	for _, page := range ordered {
		if page.ExternalID != "" || page.ExternalSource != "" {
			continue
		}
		key := "name:" + strings.ToLower(strings.TrimSpace(page.Name))
		if _, taken := out[key]; !taken {
			out[key] = page
		}
	}
	return out
}

// lookup finds the page an item should be written to.
//
// IDENTITY FIRST: a managed page found by its id is the right one even when
// somebody has since retitled a different page to match.
func lookup(index map[string]PageRef, item Item) (PageRef, bool) {
	if page, ok := index[item.ExternalID]; ok {
		return page, true
	}
	page, ok := index["name:"+strings.ToLower(strings.TrimSpace(item.Title))]
	return page, ok
}

// resolveContainers maps every project the plan needs to its id.
//
// BEFORE ANY WRITE and ALL OR NOTHING, naming what the workspace has: half
// an import is worse than none, because the half that landed looks like a
// complete knowledge base with holes in it.
func resolveContainers(ctx context.Context, opts PublishOptions) (map[string]Project, error) {
	all, err := opts.Client.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("plane: list projects: %w", err)
	}
	have := make(map[string]Project, len(all))
	known := make([]string, 0, len(all))
	for _, project := range all {
		have[strings.ToUpper(strings.TrimSpace(project.Identifier))] = project
		known = append(known, project.Identifier)
	}
	out := map[string]Project{}
	var missing []string
	for _, want := range opts.Plan.Containers() {
		project, ok := have[want]
		if !ok {
			missing = append(missing, want)
			continue
		}
		out[want] = project
	}
	if len(missing) > 0 {
		sort.Strings(known)
		return nil, fmt.Errorf(
			"plane: this workspace has no project %s, and the importer never "+
				"creates one — a container the whole company works in should "+
				"not be named by a guess. Create them, or run `crewlet plane "+
				"provision -create-projects`. The workspace has: %s",
			strings.Join(missing, ", "), strings.Join(known, ", "))
	}
	return out, nil
}

// prune deletes managed skill pages no local file publishes.
//
// # A positive marker, and skills only
//
// The predicate is "this tool published it AND its key is gone", never
// "the tree does not have it". A knowledge doc absent from this run is far
// more likely to have moved than to be dead, and a page with no marker was
// somebody's own — neither is this tool's to remove.
//
// # Archive, then delete, and put it back if the delete is refused
//
// Plane requires the archive first, and deletion is owner-or-admin only. An
// archive that lands without its delete leaves the page invisible to every
// agent AND behind an external id that 409s every future import of the same
// skill — so a prune that cannot finish undoes its own archive.
func prune(ctx context.Context, opts PublishOptions, projects map[string]Project,
	index map[string]map[string]PageRef, published map[string]map[string]bool,
) ([]string, []string) {
	var pruned, failed []string
	for _, container := range sortedContainers(projects) {
		project := projects[container]
		for _, key := range sortedKeysOf(index[container]) {
			page := index[container][key]
			switch {
			// THE INDEX HOLDS EACH PAGE TWICE — once by identity and
			// once by name, for the adoption path — so a walk that did
			// not skip the name keys would archive the same page twice
			// and count it twice. It also carries the managed check:
			// only a page this tool published is indexed by its
			// identity, so reaching here at all means it is ours.
			case key != page.ExternalID,
				!strings.HasPrefix(page.ExternalID, SkillPrefix),
				published[container][page.ExternalID]:
				continue
			}
			// AN ALREADY-ARCHIVED ORPHAN IS STILL DELETED. Archiving is
			// only Plane's precondition for the delete, and an orphan
			// left archived keeps its external id — which 409s every
			// future import of the same skill. Skipping the archive
			// call is the only difference.
			if strings.TrimSpace(page.ArchivedAt) == "" {
				if err := opts.Client.ArchivePage(ctx, project.ID, page.ID); err != nil {
					failed = append(failed, fmt.Sprintf("archive %q: %v", page.Name, err))
					continue
				}
			}
			if err := opts.Client.DeletePage(ctx, project.ID, page.ID); err != nil {
				if back := opts.Client.UnarchivePage(ctx, project.ID, page.ID); back != nil {
					failed = append(failed, fmt.Sprintf(
						"%q could not be deleted (%v) and could not be restored "+
							"(%v) — it is archived, invisible to every agent, and "+
							"its external id will refuse the next import of this "+
							"skill. Restore it in Plane", page.Name, err, back))
					continue
				}
				failed = append(failed, fmt.Sprintf("delete %q: %v", page.Name, err))
				continue
			}
			pruned = append(pruned, page.Name)
		}
	}
	return pruned, failed
}

func sortedContainers(projects map[string]Project) []string {
	out := make([]string, 0, len(projects))
	for name := range projects {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOf(index map[string]PageRef) []string {
	out := make([]string, 0, len(index))
	for key := range index {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func notesOfPlan(p *Plan) []string {
	if p == nil {
		return nil
	}
	return p.Notes
}

// readFile is os.ReadFile as a string, kept here so the walk reads a file
// exactly once per concern rather than twice through two helpers.
func readFile(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("plane: %s: %w", path, err)
	}
	return string(body), nil
}

// SkillPages walks a project and returns its pages for admission.
//
// The SAME walk the engine's boot sync runs — one implementation, so
// `resync` can never report a catalogue the engine would not load. Resolving
// the identifier is part of it: the endpoint takes a UUID, and a project
// that does not resolve is a configuration problem rather than an empty
// container.
func SkillPages(ctx context.Context, client *Client, identifier string) ([]skills.Page, error) {
	if client == nil {
		return nil, errors.New("plane: no client")
	}
	projects, err := client.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("plane: list projects: %w", err)
	}
	var id string
	known := make([]string, 0, len(projects))
	for _, project := range projects {
		known = append(known, project.Identifier)
		if strings.EqualFold(project.Identifier, identifier) {
			id = project.ID
		}
	}
	if id == "" {
		sort.Strings(known)
		return nil, fmt.Errorf(
			"plane: this workspace has no project %s, so no tool skill could "+
				"ever load. It has: %s", identifier, strings.Join(known, ", "))
	}
	rows, err := client.ListPages(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]skills.Page, 0, len(rows))
	for _, row := range rows {
		out = append(out, skills.Page{
			ID: row.ID, Title: row.Name, Text: DecodeSkillPage(row.HTML),
		})
	}
	return out, nil
}

// unsupportedFrontmatter names the keys this backend cannot honour.
//
// SAID OUT LOUD, once per run, rather than dropped. Both keys are meaningful
// on the other knowledge backend and a docs tree is meant to publish to
// either — so a file carrying them is not a mistake, it is a file written
// for Confluence. Silently ignoring it produces a workspace that looks like
// the tree and is not, which somebody discovers months later looking for a
// page they are sure they published under another.
//
// A NOTE, NOT A REFUSAL: the content is right and the page belongs in the
// workspace. Only its position and its labels are lost, and neither is worth
// refusing an import over.
func unsupportedFrontmatter(paths []string) []string {
	var nested, labelled []string
	for _, path := range paths {
		doc, err := knowledge.ParseDoc(path)
		if err != nil {
			// The walk itself already reported it, or will.
			continue
		}
		if strings.TrimSpace(doc.Parent) != "" {
			nested = append(nested, path)
		}
		if len(doc.Labels) > 0 {
			labelled = append(labelled, path)
		}
	}
	var notes []string
	if len(nested) > 0 {
		sort.Strings(nested)
		notes = append(notes, fmt.Sprintf(
			"%d file(s) declare a `parent:` and Plane pages have no parent "+
				"chain, so they were published at the project root: %s",
			len(nested), strings.Join(nested, ", ")))
	}
	if len(labelled) > 0 {
		sort.Strings(labelled)
		notes = append(notes, fmt.Sprintf(
			"%d file(s) declare `labels:` and Plane pages have no labels, so "+
				"they were published without them: %s",
			len(labelled), strings.Join(labelled, ", ")))
	}
	return notes
}
