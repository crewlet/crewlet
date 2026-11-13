package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// What the atlas reads as one installation (ADR-0131): the repositories it
// covers, each one's workflows with their last result on the default branch,
// and the environments its deployments name.
//
// # Closed rows
//
// Every row decodes the fields the atlas maps and no other. A person's login
// or email (a run's actor, a deployment's creator, a head commit's author), a
// deployment's payload, a status's description and log address are never
// decoded, so nothing a later change could map by accident is in memory.

// InventoryRepo is a repository as the atlas reads it.
type InventoryRepo struct {
	FullName      string `json:"full_name"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	// Visibility is GitHub's own word: public, private or internal. Private
	// is what an Enterprise Server too old to send the word still sends.
	Visibility string `json:"visibility"`
	Private    bool   `json:"private"`
	// PushedAt is the instant of the last push to any branch, as GitHub
	// wrote it: read as a stamp that moves when the default branch may have.
	PushedAt string `json:"pushed_at"`
}

// Workflow is one Actions workflow of a repository.
type Workflow struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// Deployment is one deployment of a repository to an environment.
type Deployment struct {
	ID          int64  `json:"id"`
	Environment string `json:"environment"`
	Transient   bool   `json:"transient_environment"`
}

// DeploymentStatus is the newest status of one deployment.
type DeploymentStatus struct {
	State          string `json:"state"`
	EnvironmentURL string `json:"environment_url"`
}

// inventoryPageSize is GitHub's per_page maximum on every listing here.
const inventoryPageSize = 100

// workflowWalkCeiling stops a workflow walk that is not converging: a
// repository's workflows are the files of one directory, and a thousand is
// more than any holds.
const workflowWalkCeiling = 1000

// deploymentPages is how many pages of a repository's deployments, newest
// first, a read walks: three hundred deployments. GitHub lists a deployment
// environment only through its deployments, so the bound is what makes the
// read cost at most three requests a repository; an environment nothing was
// deployed to within it is one the repository no longer deploys to.
const deploymentPages = 3

// installationReader reads as one installation, under a token minted for it.
type installationReader struct {
	app   *AppClient
	token string
}

func (r installationReader) get(ctx context.Context, path string, params url.Values, out any) error {
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return r.app.send(ctx, r.token, http.MethodGet, path, nil, out)
}

// page is the query of one page of a listing.
func page(n int) url.Values {
	return url.Values{"per_page": {strconv.Itoa(inventoryPageSize)}, "page": {strconv.Itoa(n)}}
}

// inventoryPath is a repository's REST path from its full name.
func inventoryPath(fullName string) string {
	owner, name, _ := strings.Cut(fullName, "/")
	return repoPath(owner, name)
}

// workflows walks a repository's workflows to exhaustion.
func (r installationReader) workflows(ctx context.Context, repo string) ([]Workflow, error) {
	var out []Workflow
	for n := 1; ; n++ {
		var body struct {
			TotalCount int        `json:"total_count"`
			Workflows  []Workflow `json:"workflows"`
		}
		if err := r.get(ctx, inventoryPath(repo)+"/actions/workflows", page(n), &body); err != nil {
			return nil, err
		}
		out = append(out, body.Workflows...)
		if len(body.Workflows) < inventoryPageSize || len(out) >= body.TotalCount {
			return out, nil
		}
		if len(out) > workflowWalkCeiling {
			return nil, fmt.Errorf("github: %s listed more than %d workflows without reaching the total "+
				"GitHub reported", repo, workflowWalkCeiling)
		}
	}
}

// lastConclusions is the conclusion of each workflow's newest completed run
// on branch, by workflow id.
//
// ONE PAGE, by design: the hundred newest completed runs of the branch, which
// GitHub lists newest first. A workflow with no run among them reports no
// result, which is what a workflow that has not run lately has. Runs a pull
// request started are left out: their result is the pull request's, never
// the branch's.
func (r installationReader) lastConclusions(ctx context.Context, repo, branch string) (map[int64]string, error) {
	var body struct {
		Runs []struct {
			WorkflowID int64  `json:"workflow_id"`
			Conclusion string `json:"conclusion"`
		} `json:"workflow_runs"`
	}
	params := page(1)
	params.Set("branch", branch)
	params.Set("status", "completed")
	params.Set("exclude_pull_requests", "true")
	if err := r.get(ctx, inventoryPath(repo)+"/actions/runs", params, &body); err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, run := range body.Runs {
		if _, seen := out[run.WorkflowID]; !seen {
			out[run.WorkflowID] = run.Conclusion
		}
	}
	return out, nil
}

// latestDeployments is the newest deployment to each lasting environment of
// a repository, newest first, over its [deploymentPages] newest pages. A
// transient environment (a preview) is left out: it is gone by design.
func (r installationReader) latestDeployments(ctx context.Context, repo string) ([]Deployment, error) {
	var out []Deployment
	seen := map[string]bool{}
	for n := 1; n <= deploymentPages; n++ {
		var rows []Deployment
		if err := r.get(ctx, inventoryPath(repo)+"/deployments", page(n), &rows); err != nil {
			return nil, err
		}
		for _, d := range rows {
			if d.Transient || d.Environment == "" || seen[d.Environment] {
				continue
			}
			seen[d.Environment] = true
			out = append(out, d)
		}
		if len(rows) < inventoryPageSize {
			break
		}
	}
	return out, nil
}

// deploymentStatus is a deployment's newest status; false for a deployment
// nothing has reported on yet.
func (r installationReader) deploymentStatus(ctx context.Context, repo string, id int64) (DeploymentStatus, bool, error) {
	var rows []DeploymentStatus
	path := inventoryPath(repo) + "/deployments/" + strconv.FormatInt(id, 10) + "/statuses"
	if err := r.get(ctx, path, url.Values{"per_page": {"1"}}, &rows); err != nil {
		return DeploymentStatus{}, false, err
	}
	if len(rows) == 0 {
		return DeploymentStatus{}, false, nil
	}
	return rows[0], true, nil
}

// What the atlas reads of a repository's code (ADR-0136): where its default
// branch is, the tree there and one blob. A commit's message and author and a
// tree's address ride the same answers and are not decoded.

// branchHead is the commit a repository's branch is at.
func (r installationReader) branchHead(ctx context.Context, repo, branch string) (string, error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	parts := strings.Split(branch, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	err := r.get(ctx, inventoryPath(repo)+"/git/ref/heads/"+strings.Join(parts, "/"), nil, &out)
	return out.Object.SHA, err
}

// TreeEntry is one entry of a commit's tree.
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// tree is every entry of the tree at commit, in one request; truncated is a
// tree past what GitHub answers in one, of which it lists only a part.
func (r installationReader) tree(ctx context.Context, repo, commit string) ([]TreeEntry, bool, error) {
	var out struct {
		Tree      []TreeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	path := inventoryPath(repo) + "/git/trees/" + url.PathEscape(commit)
	if err := r.get(ctx, path, url.Values{"recursive": {"1"}}, &out); err != nil {
		return nil, false, err
	}
	return out.Tree, out.Truncated, nil
}

// blob is one object's bytes, at most limit of them; oversize is one past
// limit, whose content is not decoded.
func (r installationReader) blob(ctx context.Context, repo, oid string, limit int64) ([]byte, bool, error) {
	var out struct {
		Size     int64  `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := r.get(ctx, inventoryPath(repo)+"/git/blobs/"+url.PathEscape(oid), nil, &out); err != nil {
		return nil, false, err
	}
	if out.Size > limit {
		return nil, true, nil
	}
	if out.Encoding != "base64" {
		return nil, false, fmt.Errorf("github: a blob of %s is encoded as %q, not base64", repo, out.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return nil, false, fmt.Errorf("github: decode a blob of %s: %w", repo, err)
	}
	if int64(len(raw)) > limit {
		return nil, true, nil
	}
	return raw, false, nil
}
