package boxhelper

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/browser"
)

// MinNodeMajor is the oldest Node major the box helper supports, and so the
// oldest a box may run the tree on: 22, the oldest release line Node's project
// still maintains, Node 20 having reached its end of life on 2026-04-30
// (nodejs/Release's schedule.json). Named as internal/browser's
// MinChromiumMajor is, and never a "node floor", which in this tree is git's
// floor for a Gitlet node. The floor's pinned release (scripts/node-pin.env)
// moves with it, which boxhelpertest's TestTheFloorNodeIsTheMajorItStandsFor
// holds, and the package doc says why that one Node version is pinned.
const MinNodeMajor = 22

// pw is the tree's two tarballs, byte for byte as the registry serves them.
//
//go:embed pw/*.tgz
var pw embed.FS

// Tarball is one npm tarball of the tree.
type Tarball struct {
	// Name is its file name under pw/: mcp-<release>.tgz for @playwright/mcp,
	// playwright-core-<release>.tgz for playwright-core.
	Name string
	// Bytes are the tarball as the registry serves it, shared by every caller,
	// which reads them and never writes into them.
	Bytes []byte
	// SHA256 is the hex SHA-256 of Bytes, the digest the pin file names.
	SHA256 string
}

// tree is the two tarballs, read and hashed once. The files are embedded from
// this repository, so one that is missing is a build whose tree is not the
// pin's, which TestTheEmbeddedTreeIsThePinnedRelease refuses: there is no
// operator input to blame and nothing to fall back to, so it panics.
var tree = sync.OnceValue(func() []Tarball {
	names := []string{
		"mcp-" + browser.PlaywrightMCPRelease + ".tgz",
		"playwright-core-" + browser.PlaywrightCoreRelease + ".tgz",
	}
	out := make([]Tarball, 0, len(names))
	for _, name := range names {
		body, err := pw.ReadFile("pw/" + name)
		if err != nil {
			panic(fmt.Sprintf("boxhelper: pw/%s is not embedded, so the tree is not internal/browser's release: "+
				"run `make playwright-mcp` (%v)", name, err))
		}
		sum := sha256.Sum256(body)
		out = append(out, Tarball{Name: name, Bytes: body, SHA256: hex.EncodeToString(sum[:])})
	}
	return out
})

// Tarballs is the tree, @playwright/mcp then playwright-core, each named for
// its release in internal/browser.
func Tarballs() []Tarball {
	return slices.Clone(tree())
}
