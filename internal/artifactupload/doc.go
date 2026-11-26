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
// inline content, a copy of the adopted brand's logo or face) are reserved the
// same way before they are stored.
//
// # Adopting a brand kit
//
// A kit's logos are stored sanitised (brandformat.Sanitize), so the bytes a
// page serves are never the bytes a stranger uploaded. An adoption copies every
// file the kit serves, sanitised again, beside the stylesheet generated once,
// all under one reservation, and refuses an unreadable text colour before any
// byte moves, so a refusal costs nothing to undo.
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
// An object is deleted only by the attempt that owns it, and only where no
// write of unknown outcome was offered it. An attempt owns the generated
// objects it stored itself and, once its version lands, the files its upload
// streamed through the engine. The record counts, on each file, the
// writes of unknown outcome it was offered to, since a version or asset one
// landed may name it: a retry taking up a claim counts the run before it, and
// an asset add, having no claim, counts itself before its write and takes its
// own count back once a refusal shows nothing landed. Whatever a run does
// not own stays reserved until its hold lapses, deleted by nothing here; only
// the collector judges it, and it deletes only what no version names. The
// record answers this rather than the domain: the operation an attempt wrote
// under derives from what it said, which an attempt that ended unanswered, or
// a node that died, leaves nobody to ask by.
//
// So a refused publish deletes the generated objects it owns and releases
// their hold, drops any other generated file from the record while keeping
// its bytes reserved there (record.Kept), and opens the caller's upload again,
// its own bytes held for another try until its window closes. A publish that
// lands deletes what it owns and the version does not name. An upload
// abandoned in its window is released early once the window closes, its
// streamed bytes deleted, unless it holds bytes a write of unknown outcome may
// name; a direct upload never is. An outcome nobody could establish leaves
// everything as it is: a retry under the same operation key continues the
// same reservation, reuses the objects its first attempt stored, and is
// answered with what the first one landed, so a retried publish never makes a
// second version.
//
// A full object store is a refusal like any other ([StoreFull], reason
// store_full, 507 on the surfaces that map it), never a failure held for a
// retry: the same publish meets the same full bucket until an operator makes
// room. It still counts as a bad publish_success event, because the refusal is
// the deployment's rather than the client's, so a full bucket burns the
// objective.
//
// Bytes are reserved before they move, and a carried object is named again
// only by a record that finds it named: ADR-0036, whose authority this package
// is.
package artifactupload
