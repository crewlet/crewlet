package mirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Wait waits for every attempt this worker started: a case's way of reading
// the rows an attempt left once it has ended.
func (m *Mirrors) Wait() { m.wg.Wait() }

// Steps is what [steps] lands of outs and cmds: each record's outputs and
// commands, in its order.
func Steps(outs []string, cmds []code.Command) (packs [][]string, refs [][]code.Command) {
	for _, st := range steps(outs, cmds) {
		packs, refs = append(packs, st.outs), append(refs, st.cmds)
	}
	return packs, refs
}

// CutOfWhatTheCopyHolds is what [Mirrors.cut] answers of a fetch that brought
// only objects repoID's copy holds already — the restart after a chunk of
// packs alone, whose fetch asks again for everything those packs hold, since
// no ref of the copy reaches them: a scratch over the copy synced at fresh,
// holding a link of every live pack of it, cut as an attempt cuts.
func (m *Mirrors) CutOfWhatTheCopyHolds(ctx context.Context, repoID string, fresh statelog.Freshness) ([]string,
	error) {

	s, err := m.o.Cache.Sync(ctx, repoID, fresh)
	if err != nil {
		return nil, err
	}
	defer s.Release()
	sc, err := m.o.Cache.NewScratch(ctx, s, statelog.NewOpID(m.now(), ""))
	if err != nil {
		return nil, err
	}
	defer sc.Remove()
	held := m.o.Cache.Packs(s)
	if len(held) == 0 {
		return nil, fmt.Errorf("the copy of %s holds no pack to fetch again", repoID)
	}
	for _, p := range held {
		for _, ext := range []string{".pack", ".idx"} {
			name := "pack-" + p.GitChecksum + ext
			if err := os.Link(filepath.Join(sc.Alternate, "objects", "pack", name),
				filepath.Join(sc.Dir, "objects", "pack", name)); err != nil {
				return nil, err
			}
		}
	}
	fetched, err := sc.Packs()
	if err != nil {
		return nil, err
	}
	var bytes int64
	for _, p := range fetched {
		bytes += fileSize(p.Path)
	}
	return m.cut(ctx, s, sc, fetched, bytes, m.o.Limits())
}
