// Package static embeds the dashboard.
//
// EMBEDDED rather than read from disk, because the product is one binary: a
// dashboard that lived beside the executable would make deployment a copy of
// two things, and a version skew between them a supported state. Embedding
// makes the assets and the server that answers their queries the same artifact
// by construction.
//
// The tree is the zero-build ES-module app itself — no bundler, no transpile
// step, no build output. What is embedded is what a browser receives, which is
// also what makes the dashboard's own test suite meaningful: it runs against
// these files, unchanged.
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
// THE ICON IS NOT UNDER dashboard/ AND MUST STILL BE NAMED. The shell asks
// for it at /static/crewlet-icon.svg (the tab icon and the sidebar brand) —
// one directory above the rest of the app, because it is the product's mark
// rather than the dashboard's asset. Embedding only `dashboard` left it
// 404ing from the binary while every module and stylesheet served perfectly,
// which renders as a page with no logo and a blank tab icon: the kind of
// break nothing fails on and nobody files. (The raster favicon.ico a browser
// asks for unprompted lives under dashboard/ and rides the `all:` pattern.)
//
// A NEW TOP-LEVEL ASSET NEEDS A NEW PATTERN HERE, and TestEveryStaticFileIsInTheBinary
// is what says so — it walks this directory on disk and fails on anything the
// embed did not take.
//
//go:embed all:dashboard
//go:embed crewlet-icon.svg
var files embed.FS

// FS is the embedded tree rooted at the dashboard directory's parent, so a
// path in it reads the same as the URL that asks for it: dashboard/js/app.js.
func FS() fs.FS { return files }
