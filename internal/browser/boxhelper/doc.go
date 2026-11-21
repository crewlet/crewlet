// Package boxhelper holds what the engine decides about a seat's own browser in
// its box: the Playwright tree the box installs with npm, locked in tree/, and
// the oldest Node, with the npm it bundles, the box helper supports
// ([MinNodeMajor], [MinNPMMajor]).
//
// # The tree, installed by npm from a lock
//
// A box gets Playwright MCP the way the catalogue's npm servers reach one: npm
// in the box installs it, from npm's registry or the mirror the box's npm
// configuration names (NPM_CONFIG_REGISTRY). tree/package.json names
// @playwright/mcp alone, at an exact release: it pins playwright and
// playwright-core exactly, so a direct dependency on either would nest a second
// copy on a bump of one alone. tree/package-lock.json pins all three by the
// registry's address and SHA-512, so `npm ci` installs those bytes or nothing,
// whether from the registry, a mirror (npm sends the lock's registry.npmjs.org
// addresses to the registry configured) or a cache. The manifest declares no
// engines, for the reason .github/dependabot.yml gives of the dashboard's.
//
// Nothing binary is committed or embedded, `go build` needs neither Node nor
// the network, and a bump is two text files. The cost: a box needs npm, and the
// registry or a mirror on its first install, or an npm cache an image filled
// from this lock, since npm cannot vouch for a node_modules an image ships. The
// three packages are Apache-2.0, and the engine carries none of their code,
// only the lock naming them; npm installs each with its own licence and notice,
// so a release's notices name none of them.
//
// # The lock is the release somebody read
//
// A bump of the tree is code every seat's browser runs, so it never lands
// unread. internal/browser names the releases somebody read
// (PlaywrightMCPRelease, PlaywrightCoreRelease) and the Chromium build that
// playwright-core expects (ExpectedChromiumRevision, ExpectedChromiumVersion),
// literals never read from the lock. TestTheLockIsTheReleaseSomebodyRead holds
// the manifest and the lock to the releases: four entries, each package at its
// release and the edges between them exact, every address the registry's own
// and every integrity a SHA-512, no package with a script, a platform, or an
// optional, development or peer flag, and no engines.node above [MinNodeMajor].
// TestTheLockedCoreExpectsTheChromiumSomebodyRead holds the expected build to
// the browsers.json of the tree the suite installs from the lock
// (boxhelpertest.Tree), the only place it is written.
//
// Dependabot proposes each release (.github/dependabot.yml), and its pull
// request stays red until a person reads the release, checks the registry's
// signatures on what it installs (`npm audit signatures`) and moves the
// constants, from a branch of their own (CONTRIBUTING.md, "A Playwright MCP
// bump is read before it lands"). Nor does it merge itself:
// .github/workflows/dependabot-merge.yml, which queues every other bump to
// merge once green, starts no run on one that changes only tree/. The
// signature check fetches the registry's keys when it runs rather than reading
// a pinned one, the price of a check npm maintains.
//
// # The floor Node
//
// A box brings its own Node and npm, and the helper supports Node
// [MinNodeMajor] or newer with the npm that major bundles, [MinNPMMajor] or
// newer. So boxhelpertest hands a case of the helper two Nodes, each with its
// own npm: the machine's, at or above both, and the floor's newest release,
// of both majors exactly, which scripts/node-pin.env pins by version and
// per-platform SHA-256 and `make node` (scripts/node.sh) fetches, keeping its
// node and its npm. Without the floor's run the suite would prove the newest
// Node alone, and a floor that only CI ran would let a developer's `make
// check` pass on code the floor rejects: the git and Chromium floors take the
// same posture. TestTheLockInstallsWithEachNodesOwnNpm installs the lock with
// each, offline, from the npm cache of the suite's tree.
//
// It is the one Node version the tree names (CONTRIBUTING.md, "Which node"),
// because a weekly check moves it: .github/workflows/pins.yml's leg
// (scripts/node-pin-check.sh) holds the pin to the newest patch of its major,
// verifying each release it proposes against the Node releasers' signing
// keys, and fails from 60 days before the major's end of life, naming the
// release to raise it to: the newest of the oldest major above it in
// long-term support whose own end of life is more than 60 days away.
package boxhelper
