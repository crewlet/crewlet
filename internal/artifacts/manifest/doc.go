// Package manifest is the grammar of an artifact version's file list: which
// paths may be published, which are reserved, how a whole publish and a patch
// publish turn a base version and a request into the next manifest, which
// entries are carried from the artifact rather than uploaded, and the one
// canonical encoding a record and a row both hold.
//
// PURE. It reads no store and holds no clock, so the publish pipeline, the
// domain's decide and the serving path all reach one answer about one path.
// A path follows the engine's one file-path rule (internal/worklet's
// NormalizeFilePath): no empty, dot or dot-dot segment, no backslash, no
// control character. The reserved prefixes `_crewlet/` and `_assets/` belong to
// the serving origin, and `preflight.js` at the root is a reserved, size-bounded
// script the engine never executes.
//
// CONTENT ADDRESSING LIVES HERE AND NOT IN THE OBJECT KEY. A publish names each
// file by its SHA-256 and size; a hash the artifact already holds is carried
// (the new entry names the existing object) instead of uploaded again. Keys stay
// random, one per upload, which is what lets the object collector delete
// without a lock. A carry never crosses an artifact: a company-wide hash lookup
// would tell a writer whether any artifact, including one they cannot read,
// holds a file with a given digest.
package manifest
