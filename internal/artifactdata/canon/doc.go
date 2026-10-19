// Package canon is the ONE canonical encoding of a datastore document and the
// store state digest built from it.
//
// # Why one encoder, and why not encoding/json
//
// Every data node must hold byte-identical rows (the domain claims identity),
// so a body is written by exactly one function: object keys sorted bytewise, no
// insignificant whitespace, every number carried as the decimal text it arrived
// as, and every string escaped by one fixed rule written here. encoding/json is
// not that function. It decodes a number into a float64 unless told otherwise,
// which rewrites a large integer (the ADR-0006 hazard), it silently keeps the
// last of two duplicate keys, and its escaping of `<`, `>` and `&` is a
// property of a library a later toolchain may change. A body this package
// encodes is the same bytes on every node and every build that runs it.
//
// A DUPLICATE KEY IS REFUSED rather than resolved: a document whose meaning
// depends on which of two values a parser keeps is two documents, and the
// datastore stores one.
//
// # The state digest
//
// A store's digest is the SUM, modulo two to the 256th, of one SHA-256 per live
// document (over its collection, id, canonical body and version) and one per
// collection (over its name and canonical specification). A sum rather than a
// hash of the concatenation, because a write must update it from what it
// changed alone: subtract the old document's term, add the new one's. Two
// nodes holding the same rows hold the same digest whatever order they wrote
// them in, and a checkpoint taken later can be verified against the digest a
// history row recorded at its position.
//
// It is an integrity check against bugs and divergence, not an adversarial
// MAC: what the rows hold is already authenticated by the signed records that
// wrote them. Pure, and a leaf over the standard library.
package canon
