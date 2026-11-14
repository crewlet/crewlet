package github

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
)

// GitHub as the atlas reads its repositories' files (ADR-0136): the
// repositories the agents' installations cover, each one's default branch,
// its tree and its blobs, under a token minted for the pass carrying
// Read-only of Metadata and Contents and revoked when the pass ends. Which
// file is read is not decided here: this lists a tree whole and serves the
// object it is asked for (internal/atlas/codescan).

// modeSymlink is git's mode of a symbolic link, whose blob is a path and
// never a file's bytes.
const modeSymlink = "120000"

// OpenCode implements [collect.CodeHost].
func (atlasSource) OpenCode(_ context.Context, in collect.Input) (collect.CodeSession, error) {
	access, err := openAtlas(in)
	if err != nil {
		return nil, err
	}
	return &codeSession{atlasAccess: access, readers: map[string]installationReader{}}, nil
}

// The source is a forge.
var _ collect.CodeHost = atlasSource{}

// codeSession is one pass's access to the organization's repositories.
type codeSession struct {
	atlasAccess
	// installs is every installation the pass reads as, in seat order.
	installs []*atlasInstall
	// readers is the installation each listed repository is read as, by the
	// repository's full name.
	readers map[string]installationReader
}

func (s *codeSession) Calls() int { return s.calls() }

// Close withdraws every token the pass minted.
func (s *codeSession) Close(ctx context.Context) { revokeAll(ctx, s.installs) }

// Repositories lists every repository an installation holding Contents
// covers that is not archived, read as the first such installation in the
// order of the agents' handles. The listing is whole unless an agent's
// installation could not be read as, or holds no Contents: what only that
// one covers is not known.
func (s *codeSession) Repositories(ctx context.Context) ([]collect.CodeRepository, bool, error) {
	whole := true
	for _, seat := range s.seats {
		in, err := openInstallation(ctx, s.http, s.apiBase, s.org, seat,
			func(held map[string]string) (map[string]string, bool) {
				// CONTENTS, READ-ONLY, whatever the agent's own tokens may
				// write: an installation without it serves no file.
				return map[string]string{PermissionMetadata: string(LevelRead), PermissionContents: string(LevelRead)},
					reads(held, PermissionContents)
			})
		if err != nil {
			return nil, false, err
		}
		if in == nil {
			whole = false
			continue
		}
		s.installs = append(s.installs, in)
		if err := in.list(ctx); err != nil {
			return nil, false, err
		}
	}
	var out []collect.CodeRepository
	for _, in := range s.installs {
		for _, name := range slices.Sorted(maps.Keys(in.repos)) {
			row := in.repos[name]
			if _, taken := s.readers[row.FullName]; taken || row.Archived || row.DefaultBranch == "" {
				continue
			}
			s.readers[row.FullName] = in.reader
			// A NAME THAT IS NO KEY is listed with none, for the pass to count.
			key, _ := atlas.ForgeRepositoryKey(s.webBase, row.FullName)
			repo := collect.CodeRepository{Key: key, ID: row.FullName, Branch: row.DefaultBranch}
			// A PUSH TO ANY BRANCH MOVES IT, and so does a new default
			// branch: while neither has, the default branch is where it was.
			// A listing that does not say when it was pushed gives no stamp,
			// and the repository is asked at every pass.
			if row.PushedAt != "" {
				repo.Stamp = row.PushedAt + " " + row.DefaultBranch
			}
			out = append(out, repo)
		}
	}
	return out, whole, nil
}

// codeFault reads a GitHub fault of one repository's code: a refusal (the
// installation lost the repository or its Contents) is the repository's own,
// and everything else, GitHub's rate limit above all, ends the pass.
func codeFault(err error) error {
	if refused(err) {
		return fmt.Errorf("%w: %w", collect.ErrCodeRefused, err)
	}
	return err
}

func (s *codeSession) reader(repo collect.CodeRepository) (installationReader, error) {
	r, ok := s.readers[repo.ID]
	if !ok {
		return installationReader{}, fmt.Errorf("github: %q is no repository this pass listed", repo.ID)
	}
	return r, nil
}

// Tip is the commit the repository's default branch is at, "" for a
// repository with no commit, which GitHub answers as a conflict.
func (s *codeSession) Tip(ctx context.Context, repo collect.CodeRepository) (string, error) {
	r, err := s.reader(repo)
	if err != nil {
		return "", err
	}
	head, err := r.branchHead(ctx, repo.ID, repo.Branch)
	if Status(err) == http.StatusConflict {
		return "", nil
	}
	return head, codeFault(err)
}

// Files lists every regular file of the tree at tip.
func (s *codeSession) Files(ctx context.Context, repo collect.CodeRepository, tip string) ([]collect.CodeFile, error) {
	r, err := s.reader(repo)
	if err != nil {
		return nil, err
	}
	entries, truncated, err := r.tree(ctx, repo.ID, tip)
	switch {
	case err != nil:
		return nil, codeFault(err)
	case truncated:
		return nil, collect.ErrCodeTooLarge
	}
	var out []collect.CodeFile
	for _, e := range entries {
		if e.Type == "blob" && e.Mode != modeSymlink {
			out = append(out, collect.CodeFile{Path: e.Path, OID: e.SHA, Size: e.Size})
		}
	}
	return out, nil
}

// Blob reads one object of the repository.
func (s *codeSession) Blob(ctx context.Context, repo collect.CodeRepository, oid string, limit int64) ([]byte, bool, error) {
	r, err := s.reader(repo)
	if err != nil {
		return nil, false, err
	}
	raw, oversize, err := r.blob(ctx, repo.ID, oid, limit)
	return raw, oversize, codeFault(err)
}
