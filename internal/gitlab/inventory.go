package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// What the atlas reads of a group (ADR-0135): its projects, each project's
// newest finished pipeline on its default branch, and its environments with
// the state of each one's newest deployment. Every call is a GET, and every
// row is decoded into the closed fields below: a field nobody mapped (a
// description, a person, a runner token, an import address) has nowhere to
// be read into.

// inventoryPageSize is a page of every listing here, GitLab's largest.
const inventoryPageSize = 100

// inventoryProjectCeiling bounds the projects one read walks. A project that
// runs pipelines costs two requests and one more per environment, and the
// duty gives one source fifteen minutes: a thousand projects is two to three
// thousand requests, which a GitLab that answers in a fifth of a second
// serves inside that with room for the records. A group past it is not read
// at all, since half a listing would retire the other half.
const inventoryProjectCeiling = 1000

// ErrInventoryTooLarge is a group holding more projects than one read walks.
var ErrInventoryTooLarge = errors.New("gitlab: the group holds more projects than one read walks")

// environmentPages is how many pages of a project's available environments a
// read lists: three hundred. A project past that keeps one per branch or per
// customer, and the read takes the ones nearest production from what it
// listed ([maxEnvironments]).
const environmentPages = 3

// InventoryProject is a project as the atlas reads it.
type InventoryProject struct {
	ID            int64  `json:"id"`
	Path          string `json:"path_with_namespace"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
	Visibility    string `json:"visibility"`
	EmptyRepo     bool   `json:"empty_repo"`

	// JobsEnabled and BuildsAccess are the two spellings of whether the
	// project runs pipelines at all; an instance sends one, both or, to a
	// reader below Reporter, neither.
	JobsEnabled        *bool  `json:"jobs_enabled"`
	BuildsAccess       string `json:"builds_access_level"`
	EnvironmentsAccess string `json:"environments_access_level"`
	// RepositoryAccess is whether the project keeps code at all.
	RepositoryAccess string `json:"repository_access_level"`
}

// accessDisabled is a project feature switched off for everybody.
const accessDisabled = "disabled"

// RunsPipelines reports a project whose CI is not switched off. Silence is
// read as on: asking costs one request, and assuming off would retire the
// pipeline of every project on an instance too old to say.
func (p InventoryProject) RunsPipelines() bool {
	return p.BuildsAccess != accessDisabled && (p.JobsEnabled == nil || *p.JobsEnabled)
}

// KeepsEnvironments reports a project whose environments are not switched off.
func (p InventoryProject) KeepsEnvironments() bool { return p.EnvironmentsAccess != accessDisabled }

// HoldsCode reports a project with a repository that has commits on a default
// branch and is not archived: one whose files say what it runs as.
func (p InventoryProject) HoldsCode() bool {
	return !p.Archived && !p.EmptyRepo && p.DefaultBranch != "" && p.RepositoryAccess != accessDisabled
}

// InventoryEnvironment is one environment of a project. Tier is GitLab's own
// word for what the environment is for (production, staging, testing,
// development, other), which it derives from the name where nobody set it.
type InventoryEnvironment struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Tier        string `json:"tier"`
	ExternalURL string `json:"external_url"`
}

func inventoryPage(n int) url.Values {
	return url.Values{"per_page": {strconv.Itoa(inventoryPageSize)}, "page": {strconv.Itoa(n)}}
}

func projectPath(id int64) string { return "/projects/" + strconv.FormatInt(id, 10) }

// InventoryProjects walks every project of a group and of its subgroups,
// archived ones included, to exhaustion.
//
// `archived` IS NOT SENT: omitted, every version lists archived and live
// projects alike, while what `true` means has differed between the finder
// and the endpoint's own description. Projects other groups merely share with
// this one are left out: they are another group's, and belong to its account.
func (c *Client) InventoryProjects(ctx context.Context, groupID int) ([]InventoryProject, error) {
	var out []InventoryProject
	path := "/groups/" + strconv.Itoa(groupID) + "/projects"
	for n := 1; ; n++ {
		params := inventoryPage(n)
		params.Set("include_subgroups", "true")
		params.Set("with_shared", "false")
		// BY ID, ASCENDING: a project created while the walk runs lands
		// past its end rather than pushing a row across a page boundary.
		params.Set("order_by", "id")
		params.Set("sort", "asc")
		var rows []InventoryProject
		if err := c.get(ctx, path, params, &rows); err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < inventoryPageSize {
			return out, nil
		}
		if len(out) >= inventoryProjectCeiling {
			return nil, fmt.Errorf("%w (%d)", ErrInventoryTooLarge, inventoryProjectCeiling)
		}
	}
}

// InventoryProject reads one project by its path.
func (c *Client) InventoryProject(ctx context.Context, path string) (InventoryProject, error) {
	var out InventoryProject
	err := c.get(ctx, "/projects/"+url.PathEscape(path), nil, &out)
	return out, err
}

// LastFinishedPipeline is the status of the newest pipeline that finished on
// ref, and false where none has. A pipeline a merge request started runs on
// the merge request's own ref, so it is never the branch's.
func (c *Client) LastFinishedPipeline(ctx context.Context, project int64, ref string) (string, bool, error) {
	var rows []struct {
		Status string `json:"status"`
	}
	params := url.Values{"per_page": {"1"}, "ref": {ref}, "scope": {"finished"},
		"order_by": {"id"}, "sort": {"desc"}}
	if err := c.get(ctx, projectPath(project)+"/pipelines", params, &rows); err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	return rows[0].Status, true, nil
}

// AvailableEnvironments lists a project's environments that are neither
// stopped nor stopping, over at most [environmentPages] pages. A stopped
// environment is one GitLab keeps the history of and serves nothing from,
// which is what every review environment becomes when its branch merges.
func (c *Client) AvailableEnvironments(ctx context.Context, project int64) ([]InventoryEnvironment, error) {
	var out []InventoryEnvironment
	for n := 1; n <= environmentPages; n++ {
		params := inventoryPage(n)
		params.Set("states", "available")
		var rows []InventoryEnvironment
		if err := c.get(ctx, projectPath(project)+"/environments", params, &rows); err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < inventoryPageSize {
			break
		}
	}
	return out, nil
}

// LastDeploymentStatus is the status of an environment's newest deployment,
// "" for one nothing was deployed to. The listing does not carry it, so it
// is one request an environment. Who deployed, the job that did and its
// commit ride the same answer and are not decoded.
func (c *Client) LastDeploymentStatus(ctx context.Context, project, environment int64) (string, error) {
	var out struct {
		Last *struct {
			Status string `json:"status"`
		} `json:"last_deployment"`
	}
	path := projectPath(project) + "/environments/" + strconv.FormatInt(environment, 10)
	if err := c.get(ctx, path, nil, &out); err != nil || out.Last == nil {
		return "", err
	}
	return out.Last.Status, nil
}

// treePages is how many pages of a commit's tree a read walks: ten thousand
// entries. A repository past that is a monorepo whose first listing alone
// would be a hundred requests at every move of its default branch, and it is
// not read.
const treePages = 100

// TreeEntry is one entry of a commit's tree. GitLab's listing carries no
// size.
type TreeEntry struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// ErrTreeTooLarge is a tree past [treePages] pages.
var ErrTreeTooLarge = errors.New("gitlab: the repository's tree is past what one read walks")

// BranchHead is the commit a project's branch is at.
func (c *Client) BranchHead(ctx context.Context, project int64, branch string) (string, error) {
	var out struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	path := projectPath(project) + "/repository/branches/" + url.PathEscape(branch)
	if err := c.get(ctx, path, nil, &out); err != nil {
		return "", err
	}
	return out.Commit.ID, nil
}

// Tree walks every entry of the tree at ref, by keyset, to exhaustion or to
// [ErrTreeTooLarge].
func (c *Client) Tree(ctx context.Context, project int64, ref string) ([]TreeEntry, error) {
	var out []TreeEntry
	params := url.Values{"ref": {ref}, "recursive": {"true"}, "pagination": {"keyset"},
		"per_page": {strconv.Itoa(inventoryPageSize)}}
	for range treePages {
		var rows []TreeEntry
		next, err := c.getPage(ctx, projectPath(project)+"/repository/tree", params, &rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if next == nil {
			return out, nil
		}
		for key, value := range next {
			params[key] = value
		}
	}
	return nil, ErrTreeTooLarge
}

// Blob reads one object of a project's repository as it is, at most limit
// bytes of it; oversize is one past limit.
func (c *Client) Blob(ctx context.Context, project int64, oid string, limit int64) ([]byte, bool, error) {
	return c.getRaw(ctx, projectPath(project)+"/repository/blobs/"+url.PathEscape(oid)+"/raw", limit)
}
