package speech

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Platforms are the platforms a recognizer library is pinned for.
func Platforms() []string {
	out := make([]string, 0, len(natives))
	for _, p := range natives {
		out = append(out, p.platform)
	}
	sort.Strings(out)
	return out
}

// Prepare fetches and verifies the recognizer into dir as `dictation.recognizer_dir`
// and [BesideBinary] lay it out: platform's library and the default model, each
// in a directory of its name. Files already there that verify are kept.
// progress hears bytes read of everything still to fetch.
func Prepare(ctx context.Context, dir, platform string, progress func(done, total int64)) error {
	p, ok := nativeFor(platform)
	if !ok {
		return fmt.Errorf("speech: no recognizer library is pinned for %s; the platforms are %s",
			platform, strings.Join(Platforms(), ", "))
	}
	m := DefaultModel
	lib := filepath.Join(dir, p.Name())
	total := m.Download()
	var done int64
	if p.Verify(lib) != nil {
		total += p.archive.Size
		if err := p.Fetch(ctx, lib, func(n int64) { progress(n, total) }); err != nil {
			return err
		}
		done = p.archive.Size
	}
	return m.Fetch(ctx, filepath.Join(dir, m.Name), func(n, _ int64) { progress(done+n, total) })
}
