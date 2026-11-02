package codetest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Rows is a repository's whole state as the code domain's rows hold it: its
// row, every live ref and every pack bundle — what every cache must hold once
// synced at Position.
type Rows struct {
	Repo     code.RepoRef
	Position statelog.Position
	// Refs is every live ref (ref → id).
	Refs  map[string]string
	Packs []code.PackRow
}

// Linearizable is the freshness a check reads rows at: through the barrier, so
// what a case judges is never an old copy.
var Linearizable = statelog.Freshness{Level: statelog.ReadLinearizable}

// Stale is the freshness of a read that must not wait for the applier.
var Stale = statelog.Freshness{Level: statelog.ReadStale}

// Reader is who a check reads the rows as: a person, an operator credential
// bound to a human seat, whom no chart and no visibility narrows (ADR-0081) —
// so what a case judges the caches by is never filtered by a grant it is not
// about.
var Reader = code.Principal{Kind: code.PrincipalOperator, OperatorID: "codetest-reader", Seat: "reader"}

// snapshotRows reads a whole snapshot — every page — through state.
func snapshotRows(ctx context.Context, repo code.RepoRef, fresh statelog.Freshness,
	state func(context.Context, code.StateQuery) (code.RepoSnapshot, error)) (Rows, error) {

	out := Rows{Repo: repo, Refs: map[string]string{}}
	q := code.StateQuery{RepoID: repo.ID, Fresh: fresh}
	for {
		page, err := state(ctx, q)
		if err != nil {
			return Rows{}, err
		}
		if q.Cursor == nil {
			out.Repo, out.Position = page.Repo, page.Position
		}
		out.Packs = append(out.Packs, page.Packs...)
		for _, r := range page.Refs {
			if r.OID != "" {
				out.Refs[r.Ref] = r.OID
			}
		}
		if page.Next == nil {
			return out, nil
		}
		q.Cursor = page.Next
	}
}

// converged is NothingButSyncWritesACache's check over a whole fleet: every
// repository the rows hold, on every node's cache, synced linearizable, holds
// exactly the rows' refs, a HEAD naming the default branch and the rows' packs
// — and nothing a sync never writes (a loose ref, a loose object).
func converged(ctx context.Context, f Fleet) error {
	names, err := f.Repos(ctx)
	if err != nil {
		return fmt.Errorf("the repositories: %w", err)
	}
	return convergedOn(ctx, f, names...)
}

// convergedOn is converged's check over the repositories called names alone.
func convergedOn(ctx context.Context, f Fleet, names ...string) error {
	var errs []error
	for _, name := range names {
		rows, err := f.Rows(ctx, name, Linearizable)
		if errors.Is(err, code.ErrRepoNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("the rows of %s: %w", name, err)
		}
		for _, n := range f.Nodes() {
			if err := cacheHolds(ctx, n, rows); err != nil {
				errs = append(errs, fmt.Errorf("%s's cache of %s: %w", n.ID(), name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// cacheHolds syncs one repository on one node linearizable — once its
// installer has placed or refused every published layer the rows name — and
// compares its directory with rows, its installed chain among them
// ([LayersHold]).
func cacheHolds(ctx context.Context, n Node, rows Rows) error {
	s, chain, err := settledCopy(ctx, n, rows.Repo.ID)
	if s.Dir == "" {
		return err
	}
	defer s.Release()
	if s.Position.Packed() < rows.Position.Packed() {
		return fmt.Errorf("synced at %s, behind the rows read at %s", s.Position, rows.Position)
	}
	errs := []error{err, LayersHold(ctx, s.Dir, chain)}
	head, err := os.ReadFile(filepath.Join(s.Dir, "HEAD"))
	if want := "ref: refs/heads/" + rows.Repo.DefaultBranch + "\n"; err != nil || string(head) != want {
		errs = append(errs, fmt.Errorf("HEAD is %q (%w), want %q", head, err, want))
	}
	refs, err := packedRefs(filepath.Join(s.Dir, "packed-refs"))
	switch {
	case err != nil:
		errs = append(errs, err)
	case !maps.Equal(refs, rows.Refs):
		errs = append(errs, fmt.Errorf("packed-refs holds %v, the rows %v", refs, rows.Refs))
	}
	if loose, lerr := filesUnder(filepath.Join(s.Dir, "refs")); lerr != nil || len(loose) > 0 {
		errs = append(errs, fmt.Errorf("loose refs %v (%w): a sync writes packed-refs alone", loose, lerr))
	}
	packs, err := packsIn(filepath.Join(s.Dir, "objects", "pack"))
	want := make([]string, 0, len(rows.Packs))
	for _, p := range rows.Packs {
		want = append(want, p.GitChecksum)
	}
	slices.Sort(want)
	if err != nil || !slices.Equal(packs, want) {
		errs = append(errs, fmt.Errorf("packs %v (%w), the rows %v", packs, err, want))
	}
	if loose, lerr := looseObjects(filepath.Join(s.Dir, "objects")); lerr != nil || len(loose) > 0 {
		errs = append(errs, fmt.Errorf("loose objects %v (%w): a sync installs packs alone", loose, lerr))
	}
	return errors.Join(errs...)
}

// packedRefs parses a packed-refs file: every ref line, never the header or
// a peeled line.
func packedRefs(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		oid, ref, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("packed-refs line %q is no ref", line)
		}
		refs[ref] = oid
	}
	return refs, nil
}

// filesUnder is every regular file below dir, relative to it.
func filesUnder(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

// packsIn is the checksum of every pack-<checksum>.pack in dir, sorted.
func packsIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "pack-") && strings.HasSuffix(name, ".pack") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(name, "pack-"), ".pack"))
		}
	}
	slices.Sort(out)
	return out, nil
}

// looseObjects is every file in a two-hex-digit directory of objects/.
func looseObjects(objects string) ([]string, error) {
	entries, err := os.ReadDir(objects)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != 2 || strings.Trim(e.Name(), "0123456789abcdef") != "" {
			continue
		}
		files, err := filesUnder(filepath.Join(objects, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			out = append(out, e.Name()+f)
		}
	}
	return out, nil
}
