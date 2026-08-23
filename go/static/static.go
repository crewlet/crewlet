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
//go:embed all:dashboard
var files embed.FS

// FS is the embedded tree rooted at the dashboard directory's parent, so a
// path in it reads the same as the URL that asks for it: dashboard/js/app.js.
func FS() fs.FS { return files }
