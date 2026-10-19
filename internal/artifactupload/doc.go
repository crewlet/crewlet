// Package artifactupload is how bytes become a version: upload intents and their
// reservations, the two transfer legs, the verified finalize, and THE publish
// pipeline every surface calls (the dashboard, the CLI, the agent tools and a
// page saving itself).
//
// # Bytes are reserved before they move
//
// An intent ([Service.Intend]) decides everything a publish can decide without
// the bytes: paths, media types, per-file and per-publish limits, which files
// the artifact already holds (those are carried and move nothing), and whether
// the bytes still to move fit the principal's pending budget, the company's
// (four times that), and the company's quota counted over what its artifacts
// name and every other hold outstanding. Only then does it mint one key per new
// file on the engine's own clock and record the hold in the fleet's
// reservation ledger, one coordination record written by compare-and-set, so
// two intents racing for the last of a budget cannot both win. A hold lasts
// until the object collector could have deleted the bytes it answers for, so
// the bytes the store holds that nothing names never exceed the holds, where
// they used to be bounded only by the collector's day. The objects the pipeline
// writes itself (a linted page, an edit's result, a copied CDN file, a tool's
// inline content) are reserved the same way before they are stored.
//
// # Two transports, one key
//
// On an S3 bucket whose boot probe proved it, a client other than an agent's
// tool is handed a presigned put bound to the file's exact length, type, digest
// and if-none-match; every other file is put through the engine
// ([Service.Transfer]), claimed on its upload's record first so no second put of
// its key ever runs, and checked against its declaration as it completes. An
// agent's sandbox is never handed a URL: a presigned put is a credential an
// injected agent could aim at any bytes.
//
// # Finalize reads back before it decides
//
// Whether a file arrived through the engine or straight to the bucket, a
// finalize ([Service.Create], [Service.Publish], [Service.AddAsset]) reads every
// new object back through the object store's verifying reader before the record
// is decided, and holds it to its type: UTF-8 for text, a format's leading bytes
// for a binary file, so a polyglot cannot pass for a benign one. The row's
// digest is therefore the digest of the stored bytes, established by the
// engine, on every transport. The engine mints every key; a client never names
// one.
//
// # One pipeline
//
// So a rule about what may be published is enforced on every path: edits
// against the base version's source, the policy linter (lint), copies of
// allowlisted CDN files into the version (vendor), credential shapes refused in
// what an agent publishes, search text, runtime contract resolution, and the
// domain's writer last. An agent's turn is held to its guardrails (twenty
// publishes, ten of them on one artifact, 128 MiB written) before any byte
// moves.
//
// # A refused publish leaves nothing behind
//
// Every object a refused publish wrote is deleted and its hold released, and
// the caller's upload is open again, its own bytes held for another try until
// its window closes. A publish that lands deletes what its upload sent and the
// version does not name, where no put of it can land later. An upload abandoned
// in its window is released early, its streamed bytes deleted, once the window
// closes. An outcome nobody could establish leaves everything as it is: a
// retry under the same operation key continues the same reservation, reuses the
// objects its first attempt stored, and is answered with what the first one
// landed, so a retried publish never makes a second version.
//
// Bytes are reserved before they move, and a carried object is named again
// only by a record that finds it named: ADR-0036, whose authority this package
// is.
package artifactupload
