// Package boxhelper holds what the engine decides about the helper that runs a
// seat's own browser in its box: the oldest Node, and the npm it bundles, the
// helper supports ([MinNodeMajor], [MinNPMMajor]).
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
// same posture.
//
// It is the one Node version the tree names (CONTRIBUTING.md, "Which node"),
// because a weekly check moves it: .github/workflows/pins.yml's leg
// (scripts/node-pin-check.sh) holds the pin to the newest patch of its major,
// verifying each release it proposes against the Node releasers' signing
// keys, and fails from 60 days before the major's end of life, naming the
// release to raise it to: the newest of the oldest major above it in
// long-term support whose own end of life is more than 60 days away.
package boxhelper
