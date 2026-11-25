package boxhelper

// MinNodeMajor is the oldest Node major the box helper supports, with the npm
// that major bundles ([MinNPMMajor]), and so the oldest a seat's box may run
// it on: 22, the oldest line Node's project still maintains (Node 20's end of
// life was 2026-04-30, nodejs/Release's schedule.json). Every locked package
// asks for less (its engines.node), which TestTheLockIsTheReleaseSomebodyRead
// holds. Named as internal/browser's MinChromiumMajor is, never a "node
// floor", which here is git's floor for a Gitlet node. The floor's pinned
// release (scripts/node-pin.env) moves with it, which boxhelpertest's
// TestTheFloorNodeIsTheMajorItStandsFor holds; CONTRIBUTING.md says why that
// one Node version is pinned ("Which node").
const MinNodeMajor = 22

// MinNPMMajor is the oldest npm major the box helper supports: 10, the npm
// every release of Node MinNodeMajor bundles, and the one npm the suite proves
// the helper on (the floor build's 10.9.9). So it refuses npm 9, which a
// distribution may pair with a supported Node (Ubuntu 26.04 packages npm 9.2.0
// beside Node 22): an npm 9 that accepts the same command proves nothing of
// what it installs, and proving it would take a third pinned npm. It moves
// with MinNodeMajor, to the npm the new floor bundles, which boxhelpertest's
// FloorNPM holds, and scripts/playwright-tree.sh's min_npm with it, which
// TestTheTreeRecipeRefusesTheNPMTheHelperRefuses holds.
const MinNPMMajor = 10
