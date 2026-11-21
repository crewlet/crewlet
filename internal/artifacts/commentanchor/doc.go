// Package commentanchor is where a comment on an artifact points, and how
// [Place] finds that content again in each version's [Index] (ADR-0154);
// [Accepts] says which anchors each artifact kind takes.
//
// A thread stores its anchor and the version it was made on, never a place per
// version: [Place] finds the anchor's content in a version's index at read, so
// a publish re-anchors every thread with no write, and a thread whose content
// is gone is kept as outdated, never dropped. The index holds no id and no
// clock, so it is cached by version. The accepts table has a row for every
// artifact kind and no default, so a new kind states its anchors or fails a test.
package commentanchor
