// Package helpersource names the source a note helper build was made from, so
// the engine refuses a helper built from other source than its own: the
// helper's flags, configs and bridge routes are a contract the two hold from
// one commit's source, and nothing else ties a helper found on disk
// (internal/sandbox/executor's noteHelperSearch) to the engine that writes it
// into a box.
//
// [Digest] is a committed constant both sides compile in, so every build of
// the engine carries it, `go build` and `go install` included, which an
// -ldflags -X stamp would not. The helper prints it in its usage, which keeps
// it in the binary, and the engine finds it by scanning the helper file's
// bytes for [Prefix]: a darwin engine cannot run a linux helper to ask.
//
// It is a SHA-256 over the non-test .go files of every package of this module
// the helper links (`go list -deps`, this package left out, since it holds
// the result), by import path and file name: derived, so a new import is
// covered with no list to keep. Module versions are left out: a bump of
// x/net, compress or yaml.v3 changes no flag, config or route between the two,
// and would otherwise need a regenerated constant on every Dependabot pull
// request. internal/notehelper/helperbuild computes it, and its suite fails
// while [Digest] is stale; `go run ./scripts/notehelper -digest` rewrites
// digest.go.
package helpersource

// Prefix begins every [Digest].
const Prefix = "crewlet-notehelper-source/"
