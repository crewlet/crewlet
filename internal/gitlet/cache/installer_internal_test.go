package cache

import (
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A FETCH A MOVED PLACEMENT HOLDS REPLACES AN OLDER STAGING UNDER ITS ID, BUT
// NOT ITS TWIN: holdFetched keeps one staged layer a repository and layer id, so
// the very layer over the very base already staged (the maintainer's adoption,
// made during the download) stays and the fetched copy goes; an id staged over
// another base, which no plan of this chain wants, gives way to the fetch, as a
// newer adoption replaces an older one.
//
// Mutations: holdFetched's twin test dropped either way: always keep the staged
// one (the layer over the new base is lost) or always replace it (the adoption
// gives way to its own fetched copy).
func TestAHeldFetchReplacesAnOlderStagingButNotItsTwin(t *testing.T) {
	t.Parallel()
	c := &Cache{installer: newLayerInstaller(FetchConcurrency)}
	const repo, id = "0b6b3cb1-2f2c-4f5e-9d6e-0c7a3b8e1f21", "9dea2c32fb952566f619905a2fe170dd720362aa"
	over := func(base string) gitlet.LayerRow {
		return gitlet.LayerRow{Layer: gitlet.LayerRef{LayerID: id, BaseID: base}}
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
