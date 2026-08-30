# 504 — What "the dashboard passes its own suite" has to mean

Status: decided, implemented
Relates to: [502 — the dashboard wire protocol is frozen](502-dashboard-wire-protocol.md)

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

So the gate says the dashboard "passes its own JS suite unmodified". The
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

`tests/dashboard/js/dashboardRoot.mjs` is that point. It reads
`CREWLET_DASHBOARD_ROOT`, falling back to the in-repo tree — the tree this
checkout ships, and the one a contributor debugging one suite by hand wants. It
resolves a relative value against the suite directory rather than the process's
cwd, and it **checks** the result: a wrong root fails with "no dashboard at
...", not with `ERR_MODULE_NOT_FOUND` on whichever module a suite happened to
import first, which reads as a missing dashboard *file* rather than a missing
dashboard.

Every suite now imports from it. The assertions are untouched — what changed is
a location, which was never part of the frozen contract, and which had been
written down 35 times. This project collapses that shape everywhere else it
appears (`queue/topics.py`, `env_refs.py`, `env_file.py`) for the same reason:
a fact recorded in many places can only ever be moved in most of them.

## Three assertions, because they were three different things

`internal/api/dashboardjs_test.go`:

1. **`TestTheDashboardPassesItsOwnSuite`** — every `*.test.mjs`, discovered by
   glob rather than listed, run under `node` against `static/dashboard` with
   a 60-second cap. Exit code *and* the harness's trailing count are checked:
   the count is written last, after the final test has actually finished, so
   its absence means the process exited zero without running anything — which
   an exit code cannot tell apart from a clean run.
2. **`TestTheEmbeddedTreeMatchesTheSource`** — the vendored copy is byte-identical
   to the Python tree, in both directions. It went with the Python tree it
   compared against.
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
a list is a thing that goes stale.

## Skipping

Locally a missing `node` skips: the dashboard's tests are somebody else's
problem and the rest of the Go suite still runs.

In CI it is a **red build**. This is the dashboard's only coverage, so letting
it go quiet would retire 348 assertions behind a green tick — the failure mode
CONTRIBUTING names ("a skip is not a pass"). `nodeBinary` is what enforces
that: `t.Fatalf` whenever `CI` is set and node is off `PATH`. `ci.yml`'s `test`
job installs node if the image stops shipping one, which makes the red build
fixable by reading the log rather than by bisecting runner images — but that
step is a convenience, not the gate, and nothing asserts it is still there.

`TestTheWorkflowDeclaresItsNodeDependency` used to assert it. It was dropped:
it grepped the whole of `ci.yml` for `node --version`, a string that appears in
two jobs, so deleting the step from the `test` job — the only one that runs
this gate — left it green. It also went red on `node -v` and on an upgrade to
`actions/setup-node`, an edit that would make the invariant stronger. See
decisions/901, *What is deliberately not asserted*, for the general form.

The workflow also had to start **watching** the dashboard. During the rewrite
`go-ci` filtered on `go/**`, and neither of this gate's inputs was under it —
the suites sat in the Python tree and the tree they certify was held identical
to the Python one. A dashboard edit would have reached no job that runs its
tests against the Go server. A path filter that does not name a gate's inputs
retires the gate in silence, so both paths went into it. `ci.yml` has since
dropped path filtering altogether and runs on every change, which settles the
same question by removing it.

## Measurement

Twelve mutants were run when this gate was built, and all were caught: a
static-reference regex that matches nothing, a module-specifier regex that
matches nothing, modules served as `application/octet-stream`, the icons
dropped from the embed pattern, one byte of drift between the two trees, a
broken function in the served `store.js`, the runner not passing
`CREWLET_DASHBOARD_ROOT` (which silently tests the Python tree — caught only
because the served tree was broken in the same mutant), the runner ignoring the
exit code, a suite glob pointing at nothing, the workflow's node step removed,
and each of the two path filters removed.

Three of those twelve no longer apply: the drift mutant went with the Python
tree, and the two path-filter mutants went with `ci.yml`'s path filters. The
node-step mutant was caught by `TestTheWorkflowDeclaresItsNodeDependency`,
which has since been dropped — a later run showed it only caught the step
being deleted from *both* jobs, not from the `test` job alone. The nine
mutants that bear on the two surviving assertions still hold.
