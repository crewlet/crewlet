// Package static embeds the dashboard, and what the artifact origin serves
// beside a page: the page runtime and the design system's artifact kit.
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
// against these files.
//
// THE PROTOCOL MODULE (`protocol/protocol.js`) is the source's second build
// target, which internal/e2e replays a real company's socket frames through:
// committed, rebuilt and diffed with the dashboard, and NOT embedded. No page
// loads it and the engine serves it to nobody, so in the embedded tree it was
// 124 KiB of a test's module in every engine binary and image.
//
// THE ARTIFACT TREE (`artifacts/`) is the dashboard source's third build
// target (`dashboard/vite.runtime.config.ts`): `runtime/<contract>/` installs
// `window.crewlet` in an artifact page, and `kit/<version>/` is the
// `@crewlethq/artifact-kit` package's own built files, copied unchanged.
// internal/artifactserve serves both from the api.artifacts listener, and
// internal/artifactupload reads which runtime contracts the tree holds when a
// publish pins one. Committed, rebuilt and diffed exactly as the dashboard is.
package static

import (
	"embed"
	"io/fs"
)

// files is the whole tree.
//
// `all:` so directories beginning with _ or . are included: the default embed
// pattern silently skips them, and nothing fails until a browser asks.
// THE BRAND MARK IS UNDER dashboard/ TOO, the only copy: the build emits it
// from `@crewlethq/icons` (`dashboard/vite.config.ts`, `brandAssets`).
//
// A NEW TOP-LEVEL ASSET NEEDS A NEW PATTERN HERE, and
// TestEveryStaticFileIsInTheBinary fails on anything on disk the embed did not
// take. protocol/ is the one excused tree (unembeddedTrees), held the other
// way: a pattern taking it fails too.
//
//go:embed all:dashboard all:artifacts
var files embed.FS

// FS is the embedded tree rooted at the dashboard directory's parent, so a
// path in it reads the same as the URL that asks for it: dashboard/js/app.js,
// and artifacts/runtime/1.0.0/runtime.js beside it.
func FS() fs.FS { return files }
