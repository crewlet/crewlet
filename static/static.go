// Package static embeds the dashboard.
//
// EMBEDDED rather than read from disk, because the product is one binary: a
// dashboard that lived beside the executable would make deployment a copy of
// two things, and a version skew between them a supported state. Embedding
// makes the assets and the server that answers their queries the same artifact
// by construction.
//
// The tree is a BUILD OUTPUT, written by Vite from dashboard/ and committed,
// because `go build ./...` and `go install ...@latest` have to work on a clean
// checkout with no Node on the machine, and an embed directive cannot run a
// bundler. CI rebuilds it and diffs the tree, so a committed bundle that does
// not match its source is a red build rather than a silent lie.
package static

import (
	"embed"
	"io/fs"
)

// files is the whole tree.
//
// `all:` so directories whose names begin with _ or . are included too: the
// default embed pattern silently skips them, and a stylesheet under one would
// be missing from the binary with nothing failing until a browser asked for it.
//
// EVERYTHING THE PRODUCT SERVES IS UNDER dashboard/. The mark used to sit
// beside it and need a pattern of its own, and embedding only `dashboard`
// left it 404ing from the binary while every module and stylesheet served
// perfectly: a page with no logo and a blank tab icon, which is the kind of
// break nothing fails on and nobody files. It is emitted into the build now,
// from the package that owns the drawing, along with the raster favicon a
// browser asks for unprompted.
//
// A NEW TOP-LEVEL ASSET WOULD NEED A NEW PATTERN HERE, and
// TestEveryStaticFileIsInTheBinary is what says so: it walks this directory on
// disk and fails on anything the embed did not take.
//
//go:embed all:dashboard
var files embed.FS

// FS is the embedded tree rooted at the dashboard directory's parent, so a
// path in it reads the same as the URL that asks for it: dashboard/js/app.js.
func FS() fs.FS { return files }
