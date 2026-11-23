package coordtest

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the atlas code scans' standings ------------------------------------ //

func (h *fleetHarness) putAtlasCodeScan(repo, value string) {
	h.t.Helper()
	h.putAtlasCodeScanOf(coord.AtlasCodeGitlet, repo, value)
}

func (h *fleetHarness) putAtlasCodeScanOf(host, repo, value string) {
	h.t.Helper()
	if err := h.f.PutAtlasCodeScan(h.ctx, host, repo, []byte(value)); err != nil {
		h.t.Fatalf("PutAtlasCodeScan(%q, %q): %v", host, repo, err)
	}
}

func (h *fleetHarness) atlasCodeScans(host string) map[string][]byte {
	h.t.Helper()
	got, err := h.f.AtlasCodeScans(h.ctx, host)
	if err != nil {
		h.t.Fatalf("AtlasCodeScans(%q): %v", host, err)
	}
	return got
}

func (h *fleetHarness) atlasCodeScan(host, repo string) (string, bool) {
	h.t.Helper()
	value, found, err := h.f.AtlasCodeScan(h.ctx, host, repo)
	if err != nil {
		h.t.Fatalf("AtlasCodeScan(%q, %q): %v", host, repo, err)
	}
	return string(value), found
}

func (h *fleetHarness) atlasCodeScanRepos() []string {
	h.t.Helper()
	repos := slices.Collect(maps.Keys(h.atlasCodeScans(coord.AtlasCodeGitlet)))
	slices.Sort(repos)
	return repos
}

var atlasCodeScanCases = []fleetCase{
	{name: "a recorded standing reads back byte for byte, listed and by its key", fn: func(h *fleetHarness) {
		// The value is OPAQUE to this store, and the key is a UUID, which
		// the backend must carry as it is.
		const repo, standing = "0192f3a0-7c1e-7b3a-9d2e-5f6a7b8c9d0e", `{"name":"shop","tip":"ab12"}`
		h.putAtlasCodeScan(repo, standing)
		if got := string(h.atlasCodeScans(coord.AtlasCodeGitlet)[repo]); got != standing {
			h.t.Fatalf("listed %q, want %q", got, standing)
		}
		if got, found := h.atlasCodeScan(coord.AtlasCodeGitlet, repo); !found || got != standing {
			h.t.Fatalf("read back %q (%v), want %q", got, found, standing)
		}
	}},

	{name: "a repository with no standing is absent rather than empty", fn: func(h *fleetHarness) {
		// The scan reads a missing key as "never scanned, so due now".
		h.putAtlasCodeScan("repo-b", `{}`)
		if _, present := h.atlasCodeScans(coord.AtlasCodeGitlet)["repo-a"]; present {
			h.t.Fatal("a repository nothing recorded came back present")
		}
		if _, found := h.atlasCodeScan(coord.AtlasCodeGitlet, "repo-a"); found {
			h.t.Fatal("a repository nothing recorded was read by its key")
		}
	}},

	{name: "the last write wins", fn: func(h *fleetHarness) {
		h.putAtlasCodeScan("repo-a", `{"tip":"1"}`)
		h.putAtlasCodeScan("repo-a", `{"tip":"2"}`)
		if got, _ := h.atlasCodeScan(coord.AtlasCodeGitlet, "repo-a"); got != `{"tip":"2"}` {
			h.t.Fatalf("read back %q, want the second write", got)
		}
	}},

	{name: "a deleted standing is gone, the rest survive, and deleting it again is fine", fn: func(h *fleetHarness) {
		h.putAtlasCodeScan("repo-a", `{}`)
		h.putAtlasCodeScan("repo-b", `{}`)
		for range 2 {
			if err := h.f.DeleteAtlasCodeScan(h.ctx, coord.AtlasCodeGitlet, "repo-a"); err != nil {
				h.t.Fatalf("DeleteAtlasCodeScan: %v", err)
			}
		}
		if got := h.atlasCodeScanRepos(); !slices.Equal(got, []string{"repo-b"}) {
			h.t.Fatalf("listed %v, want repo-b alone", got)
		}
	}},

	{name: "each host lists its own repositories, never its own standing or another host's", fn: func(h *fleetHarness) {
		// A forge's pass reads its repositories alone, and Gitlet's scan
		// never takes a forge's for a repository Gitlet stopped listing.
		// Mutation: list a host's keys by their class alone, and Gitlet
		// lists github's repository.
		const voice = "github.com:acme/shop"
		h.putAtlasCodeScan("repo-a", `{"tip":"g"}`)
		h.putAtlasCodeScanOf("github", voice, `{"tip":"f"}`)
		h.putAtlasCodeScanOf("github", "", `{"repositories":1}`)
		h.putAtlasCodeScanOf("gitlab", voice, `{"tip":"l"}`)
		if got := h.atlasCodeScanRepos(); !slices.Equal(got, []string{"repo-a"}) {
			h.t.Fatalf("Gitlet's repositories listed %v, want repo-a alone", got)
		}
		forge := h.atlasCodeScans("github")
		if len(forge) != 1 || string(forge[voice]) != `{"tip":"f"}` {
			h.t.Fatalf("github's repositories listed %q, want its one", forge)
		}
		if got, found := h.atlasCodeScan("github", ""); !found || got != `{"repositories":1}` {
			h.t.Fatalf("github's own standing read back %q (%v)", got, found)
		}
		if err := h.f.DeleteAtlasCodeScan(h.ctx, "github", ""); err != nil {
			h.t.Fatal(err)
		}
		if _, found := h.atlasCodeScan("github", ""); found {
			h.t.Fatal("github's own standing survived its deletion")
		}
		if got, found := h.atlasCodeScan("github", voice); !found || got != `{"tip":"f"}` {
			h.t.Fatalf("deleting github's own standing took its repository's: %q (%v)", got, found)
		}
	}},

	{name: "a standing the caller mutates afterwards does not change the store", fn: func(h *fleetHarness) {
		value := []byte(`{"tip":"1"}`)
		if err := h.f.PutAtlasCodeScan(h.ctx, coord.AtlasCodeGitlet, "repo-a", value); err != nil {
			h.t.Fatalf("PutAtlasCodeScan: %v", err)
		}
		copy(value, []byte(`{"tip":"9"}`))
		if got, _ := h.atlasCodeScan(coord.AtlasCodeGitlet, "repo-a"); got != `{"tip":"1"}` {
			h.t.Fatalf("read back %q after the caller reused its buffer", got)
		}
	}},

	{name: "a standing is kept apart from the atlas sources' standings", fn: func(h *fleetHarness) {
		// The collect duty forgets every key of its bucket that is no source
		// of its build, which is what a repository's id is to it.
		h.putAtlasCodeScan("cloudflare", `{"tip":"1"}`)
		h.putAtlasSource("cloudflare", `{"calls":1}`)
		if err := h.f.DeleteAtlasSourceStatus(h.ctx, "cloudflare"); err != nil {
			h.t.Fatal(err)
		}
		if got, _ := h.atlasCodeScan(coord.AtlasCodeGitlet, "cloudflare"); got != `{"tip":"1"}` {
			h.t.Fatalf("forgetting the source's standing took the scan's with it: %q", got)
		}
	}},
}
