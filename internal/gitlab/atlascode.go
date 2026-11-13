package gitlab

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/integration"
)

// GitLab as the atlas reads its repositories' files (ADR-0136): the group's
// projects that hold code, each one's default branch, its tree and its blobs,
// under the credential the atlas's own read of the group uses and with GET
// alone. Which file is read is not decided here: this lists a tree whole and
// serves the object it is asked for (internal/atlas/codescan).

// modeSymlink is git's mode of a symbolic link, whose blob is a path and
// never a file's bytes.
const modeSymlink = "120000"

// OpenCode implements [collect.CodeHost].
func (atlasSource) OpenCode(_ context.Context, in collect.Input) (collect.CodeSession, error) {
	access, err := openAtlas(in)
	if err != nil {
		return nil, err
	}
	return &codeSession{atlasAccess: access}, nil
}

// codeSession is one pass's access to the group's repositories.
type codeSession struct{ atlasAccess }

func (s *codeSession) Calls() int { return s.calls() }

// Close ends the session. It minted nothing, so there is nothing to withdraw.
func (s *codeSession) Close(context.Context) {}

// Repositories lists every project of the group, and each the settings name
// beside it, that holds code. The listing is whole unless GitLab does not
// show the group or a named project, or the group is past what a read walks.
func (s *codeSession) Repositories(ctx context.Context) ([]collect.CodeRepository, bool, error) {
	group, found, err := s.client.GroupByPath(ctx, s.group)
	if err != nil {
		return nil, false, fmt.Errorf("gitlab: read the group: %w", integration.Reject(err, Status(err)))
	}
	if !found {
		return nil, false, nil
	}
	projects, err := s.client.InventoryProjects(ctx, group.ID)
	switch {
	case errors.Is(err, ErrInventoryTooLarge):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("gitlab: list the group's projects: %w", err)
	}
	whole := true
	listed := map[string]bool{}
	for _, p := range projects {
		listed[strings.ToLower(p.Path)] = true
	}
	for _, path := range s.extraPaths() {
		if listed[strings.ToLower(path)] {
			continue
		}
		listed[strings.ToLower(path)] = true
		p, err := s.client.InventoryProject(ctx, path)
		switch {
		case err == nil:
			projects = append(projects, p)
		case refused(err):
			whole = false
		default:
			return nil, false, fmt.Errorf("gitlab: read a project the settings name: %w", err)
		}
	}
	var out []collect.CodeRepository
	for _, p := range projects {
		if !p.HoldsCode() {
			continue
		}
		// A PATH THAT IS NO KEY is listed with none, for the pass to count.
		key, _ := atlas.ForgeRepositoryKey(s.web, p.Path)
		out = append(out, collect.CodeRepository{Key: key, ID: strconv.FormatInt(p.ID, 10), Branch: p.DefaultBranch})
	}
	return out, whole, nil
}

// codeFault reads a GitLab fault of one repository's code: a refusal (the
// credential's owner is below Reporter there, or the project is gone) is the
// repository's own, and everything else ends the pass.
func codeFault(err error) error {
	if refused(err) {
		return fmt.Errorf("%w: %w", collect.ErrCodeRefused, err)
	}
	return err
}

func projectID(repo collect.CodeRepository) (int64, error) {
	id, err := strconv.ParseInt(repo.ID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("gitlab: %q is no project's id", repo.ID)
	}
	return id, nil
}

// Tip is the commit the project's default branch is at.
func (s *codeSession) Tip(ctx context.Context, repo collect.CodeRepository) (string, error) {
	id, err := projectID(repo)
	if err != nil {
		return "", err
	}
	head, err := s.client.BranchHead(ctx, id, repo.Branch)
	return head, codeFault(err)
}

// Files lists every regular file of the tree at tip.
func (s *codeSession) Files(ctx context.Context, repo collect.CodeRepository, tip string) ([]collect.CodeFile, error) {
	id, err := projectID(repo)
	if err != nil {
		return nil, err
	}
	entries, err := s.client.Tree(ctx, id, tip)
	switch {
	case errors.Is(err, ErrTreeTooLarge):
		return nil, collect.ErrCodeTooLarge
	case err != nil:
		return nil, codeFault(err)
	}
	var out []collect.CodeFile
	for _, e := range entries {
		if e.Type == "blob" && e.Mode != modeSymlink {
			out = append(out, collect.CodeFile{Path: e.Path, OID: e.ID, Size: -1})
		}
	}
	return out, nil
}

// Blob reads one object of the project's repository.
func (s *codeSession) Blob(ctx context.Context, repo collect.CodeRepository, oid string, limit int64) ([]byte, bool, error) {
	id, err := projectID(repo)
	if err != nil {
		return nil, false, err
	}
	raw, oversize, err := s.client.Blob(ctx, id, oid, limit)
	return raw, oversize, codeFault(err)
}

// The source is a forge.
var _ collect.CodeHost = atlasSource{}
