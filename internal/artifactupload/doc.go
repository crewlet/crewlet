// Package artifactupload is how bytes become a version: upload intents and their
// reservations, the two transfer legs, the verified finalize, and THE publish
// pipeline every surface calls (the dashboard, the CLI, the agent tools and a
// page saving itself).
//
// BYTES ARE RESERVED BEFORE THEY MOVE. An intent decides everything a publish
// can decide without the bytes (the caller's authority, paths, media types,
// per-file and per-publish limits, which files the artifact already holds, and
// whether named usage plus outstanding reservations plus the new bytes fits the
// quota), mints one key per new file at the engine's own clock, and records a
// reservation that outlives its objects until the collector could have deleted
// them. So bytes nothing names never exceed the reservation budget, where they
// used to be bounded only by the collector's day.
//
// FINALIZE READS BACK BEFORE IT DECIDES. Whether a file arrived through the
// engine or straight to an S3 bucket on a presigned URL bound to its exact
// length, type and digest, finalize reads every new object back through the
// object store's verifying reader before the record is decided. The row's
// digest is therefore the digest of the stored bytes, established by the engine,
// on every transport. The engine mints every key; a client never names one.
//
// ONE PIPELINE, so a rule about what may be published is enforced on every
// path: edits, the policy linter, vendoring of allowlisted CDN files into the
// artifact's own version, search text extraction and runtime contract
// resolution all run here, and the domain's writer decides last.
//
// Bytes are reserved before they move, and a carried object is named again
// only by a record that finds it named: ADR-0036, whose authority this package
// is.
package artifactupload
