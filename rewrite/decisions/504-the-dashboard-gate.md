# 504 — Gate G5: what "the dashboard passes its own suite" has to mean

Status: decided, implemented
Relates to: [502 — the dashboard wire protocol is frozen](502-dashboard-wire-protocol.md), D14 (repo layout)

## The problem

The dashboard is not being rewritten. It is ~5k lines of zero-build ES modules
that talk to the server over one WebSocket, and 502 froze the protocol between
them: the client ships unchanged and wins any disagreement about what a frame
contains.

That makes its JavaScript suite — 23 files, 348 assertions, running under bare
`node` with a vendored DOM — the compatibility reference for the client's half
of the contract. It is the only place on either side of the rewrite where the
shape the browser actually parses is written down and checked. The Python side
of the dashboard has essentially no coverage because there is essentially no
Python in it.

So Phase 5's gate says the dashboard "passes its own JS suite unmodified". The
question is what that sentence is allowed to mean, because there are several
readings and only one of them certifies anything.

## What was already there

Every suite located the dashboard by spelling a relative path to it:

```js
const base = new URL("../../../src/crewlet/static/dashboard/js/", import.meta.url);
```

Thirty-five occurrences across 23 files. Each names the *Python* tree, which
is the tree the Python server serves.

The Go binary serves a different tree. `go:embed` cannot reach outside its own
module, so `go/static/dashboard/` is a **copy** of `src/crewlet/static/dashboard/`,
and the Go server hands out the copy.

## The reading that certifies nothing

Run the suites where they are.

They pass. The two trees are byte-identical today, so the run is green and the
gate is "met" — and it has tested a set of files the Go binary never serves.
The first fix that lands in one copy and not the other passes just as green:
the suite reads the copy the fix went into, and the binary serves the other.

"Identical today" is the property a gate exists to *keep* true. It is not one a
gate may assume on its way to declaring itself satisfied.

## Decision

**The tree under test is a parameter with exactly one resolution point, and the
Go runner points it at the tree the binary embeds.**

`tests/test_dashboard/js/dashboardRoot.mjs` is that point. It reads
`CREWLET_DASHBOARD_ROOT`, falling back to the in-repo Python tree — which is
the living specification until Phase 9 (D14), and the tree a contributor
debugging one suite by hand wants. It resolves a relative value against the
suite directory rather than the process's cwd, and it **checks** the result: a
wrong root fails with "no dashboard at ...", not with `ERR_MODULE_NOT_FOUND` on
whichever module a suite happened to import first, which reads as a missing
dashboard *file* rather than a missing dashboard.

Every suite now imports from it. The assertions are untouched — what changed is
a location, which was never part of the frozen contract, and which had been
written down 35 times. This project collapses that shape everywhere else it
appears (`queue/topics.py`, `env_refs.py`, `env_file.py`) for the same reason:
a fact recorded in many places can only ever be moved in most of them.

## Three assertions, because they are three different things

`internal/api/dashboardjs_test.go`:

1. **`TestTheDashboardPassesItsOwnSuite`** — every `*.test.mjs`, discovered by
   glob rather than listed, run under `node` against `go/static/dashboard` with
   a 60-second cap. Exit code *and* the harness's trailing count are checked:
   the count is written last, after the final test has actually finished, so
   its absence means the process exited zero without running anything — which
   an exit code cannot tell apart from a clean run.
2. **`TestTheEmbeddedTreeMatchesTheSource`** — the vendored copy is byte-identical
   to the Python tree, in both directions. Deleted in Phase 9 with the tree it
   compares against.
3. **`TestTheShellLoadsFromTheBinary`** — fetch `/dashboard` from the server,
   then everything it names, then everything *those* name, all over HTTP.

(1) without (2) tests files the server never serves. (2) without (3) proves the
bytes are present and nothing about the routes that hand them out: an ES module
served as anything but a JavaScript type is refused by the module loader, and
the page fails with a MIME error rather than a missing file, which sends a
reader looking for the wrong problem.

### (3) found a bug on its first run

`//go:embed all:dashboard` took the dashboard directory. The shell asks for two
files that are not in it — `/static/crewlet-icon.svg` (the favicon) and
`/static/crewlet-icon.png` (the sidebar brand) — because they are the product's
marks rather than the dashboard's assets, and they live one directory up.

Both 404'd from the Go binary. Every module and every stylesheet served
perfectly; the page just had no logo and a blank tab icon. Nothing failed, and
the JS suite could not have caught it: it reads files off disk, where they were
present all along.

The fix names them in the embed pattern. The *guard* is
`TestEveryStaticFileIsInTheBinary`, which walks the directory on disk and fails
on anything the pattern did not take — because an embed pattern is a list, and
a list is a thing that goes stale. That one survives Phase 9.

## Skipping

Locally a missing `node` skips: the dashboard's tests are somebody else's
problem and the rest of the Go suite still runs.

In CI it is a **red build**. This is the dashboard's only coverage, so letting
it go quiet would retire 348 assertions behind a green tick — the failure mode
CONTRIBUTING names ("a skip is not a pass"). `go-ci`'s `test` job installs node
if the image stops shipping one, and `TestTheGoWorkflowDeclaresItsNodeDependency`
asserts the step is still there: the fatal turns a missing binary red, and the
step makes the red build fixable by reading the log rather than by bisecting
runner images.

The workflow also had to start **watching** the dashboard. `go-ci` filtered on
`go/**`, and neither of this gate's inputs is under it — the suites are in the
Python tree and the tree they certify is held identical to the Python one. A
dashboard edit would have reached no job that runs its tests against the Go
server. The same test asserts those two paths are in the filter.

## Measurement

Twelve mutants, all caught: a static-reference regex that matches nothing, a
module-specifier regex that matches nothing, modules served as
`application/octet-stream`, the icons dropped from the embed pattern, one byte
of drift between the two trees, a broken function in the served `store.js`, the
runner not passing `CREWLET_DASHBOARD_ROOT` (which silently tests the Python
tree — caught only because the served tree was broken in the same mutant), the
runner ignoring the exit code, a suite glob pointing at nothing, the workflow's
node step removed, and each of the two path filters removed.

The one invalid mutant is worth recording: deleting `"node --version"` from the
*assertion list* survived, as it must — a test cannot catch the removal of its
own assertion. It was re-run against the workflow instead, where it is caught.
