package cache

import (
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// A FETCH A MOVED PLACEMENT HOLDS REPLACES AN OLDER STAGING UNDER ITS ID, BUT
// NOT ITS TWIN: the installer keeps one staged layer a repository and layer id
// (holdFetched), so where the very layer over the very base is staged already —
// the maintainer's adoption, which came while the fetch downloaded — that one
// stays and the fetched copy goes; and where the id names a layer over another
// base, an older staging no plan of this chain wants, the fetch the plan made
// takes its place, as a newer adoption replaces an older one.
//
// Mutations: keep whatever is staged under the id (holdFetched's twin test
// dropped, the fetch always the one to go) — the layer over the new base is
// lost; replace whatever is staged (the twin test dropped the other way) — the
// adoption gives way to its own fetched copy.
func TestAHeldFetchReplacesAnOlderStagingButNotItsTwin(t *testing.T) {
	t.Parallel()
	c := &Cache{installer: newLayerInstaller(FetchConcurrency)}
	const repo, id = "0b6b3cb1-2f2c-4f5e-9d6e-0c7a3b8e1f21", "9dea2c32fb952566f619905a2fe170dd720362aa"
	over := func(base string) code.LayerRow {
		return code.LayerRow{Layer: code.LayerRef{LayerID: id, BaseID: base}}
	}
	adopted := &stagedLayer{row: over(""), adopted: true}
	c.installer.staged[repo] = map[string]*stagedLayer{id: adopted}

	c.holdFetched(repo, []*stagedLayer{{row: over("")}})
	if got := c.installer.staged[repo][id]; got != adopted {
		t.Errorf("the very layer the maintainer adopted gave way to a fetched copy of it: %+v", got)
	}
	rebased := &stagedLayer{row: over("364ec34726d84da2502be913a9759f942b368658"),
		base: []string{"b1a1cc180a765dadf178593501dded35ba0c854a"}}
	c.holdFetched(repo, []*stagedLayer{rebased})
	if got := c.installer.staged[repo][id]; got != rebased {
		t.Errorf("the fetch over the base its plan wants is not what is staged under its id: %+v", got)
	}
}
