// Package provenance is the ONE fence around text a model reads that somebody
// other than the reading seat wrote: page bodies, work item comments, project
// files, chat threads, artifact files and datastore rows.
//
// Such text is data, never an instruction. Each read result wraps it in a fence
// bounded by a per-turn random token, so content cannot close the fence by
// guessing the marker, and labels who wrote it and how far the reader may trust
// it. The seat's prompt carries one standing sentence, owned here, saying what a
// fence means. Fencing reduces prompt injection and does not remove it; the
// structural defences (the requester ceiling, human-only rows and fields, the
// rule that only a person loosens a rule) hold whatever a model does.
//
// A LEAF: it imports the identity vocabulary and nothing else from the engine,
// so every tool and prompt builder can use it.
//
// Content a model reads carries its provenance: ADR-0040, whose authority this
// package is.
package provenance
