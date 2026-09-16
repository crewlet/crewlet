// Package static embeds the dashboard.
//
// EMBEDDED rather than read from disk, because the product is one binary: a
// dashboard that lived beside the executable would make deployment a copy of
// two things, and a version skew between them a supported state. Embedding
// makes the assets and the server that answers their queries the same artifact
// by construction.
//
// The tree IS BUILD OUTPUT — Vite's, from the React + TypeScript source in
// dashboard/ — and it is committed, which is what lets `go build ./...` and
// `go install …@latest` work on a clean checkout with no node on the machine:
// an embed directive cannot run a bundler. So nothing here is hand-edited and
// nothing here is the thing under test; `make dashboard` regenerates it,
// `make dashboard-check` fails when it has drifted from its source, and the
// dashboard's own suites run against dashboard/src under Vitest rather than
// against these files. The one exception is dashboard/protocol.js, the second
// build target, which internal/e2e replays a real company's socket frames
// through.
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
// THE PRODUCT'S MARK RIDES THE DASHBOARD TREE, and did not always. It used to
// sit one directory above the rest of the app and needed a pattern of its own
// here, because embedding only `dashboard` left it 404ing from the binary
// while every module and stylesheet served perfectly: a page with no logo and
// a blank tab icon, the kind of break nothing fails on and nobody files.
//
// It is now the design system's asset rather than a file this repository
// keeps. The dashboard build copies it out of @crewlethq/icons into its own
// output (see the copy list in dashboard/vite.config.ts), so it is served from
// /static/dashboard/crewlet-icon.svg, which is where the shell, the GitHub App
// return page and the dashboard's own document all ask for it, and it rides
// the `all:` pattern with everything else.
//
// A NEW TOP-LEVEL ASSET WOULD STILL NEED A NEW PATTERN HERE, and
// TestEveryStaticFileIsInTheBinary is what says so: it walks this directory on
// disk and fails on anything the embed did not take.
//
//go:embed all:dashboard
var files embed.FS

// FS is the embedded tree rooted at the dashboard directory's parent, so a
// path in it reads the same as the URL that asks for it: dashboard/js/app.js.
func FS() fs.FS { return files }
