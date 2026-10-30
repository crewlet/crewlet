// Package artifacts is the artifacts VOCABULARY: the values every surface that
// touches an artifact has to be able to say, and nothing that does anything
// with them.
//
// # Why a leaf, beside the domain rather than inside it
//
// An artifact is named by the authority table, the state log, the upload
// pipeline, the serving origin, the HTTP surface, the agent tools, the
// datastore and the dashboard's own contract. Several of those must not
// import a store client or a broker: internal/authz decides over values and a
// chart and nothing else, and internal/config validates a file with no I/O at
// all. So the words they share live here, VALUES ONLY, exactly as
// internal/iam holds the identity vocabulary for internal/iamdomain's durable
// half. The durable half of artifacts is internal/artifactsdomain, and the
// datastore's is internal/artifactdata.
//
// One definition per shared shape is the point (one implementation of a shared
// rule): the role ladder the authority table compares, the share an applier
// stores, the manifest a publish names, the authority a datastore write is
// stamped with and the claims a view ticket carries are each written once,
// here, and every package that speaks them imports this one rather than
// declaring a look-alike. What a presence room says is internal/presence's,
// because an artifact is no longer the only thing with a room.
//
// # What lives here, and what does not
//
// Identifiers and the host label, kinds and capabilities, the role ladder,
// audiences and shares, manifest entries and the script policy a linter
// derives for them, the plan quotas and every bound with its reason, the
// authority stamp, the upload intent, the view ticket's claims and the frame
// the dashboard embeds, and the datastore's op and schema shapes.
//
// Behaviour does not: the label codec and id derivation belong to the domain
// that mints ids, the ladder's ordering to the authority table that compares
// roles, the manifest grammar to internal/artifacts/manifest, the media table
// to internal/artifacts/mediatype, and every validation of a datastore value to
// internal/artifactdata. A small, named set of files in this package is added
// by those owners (role.go, id.go, label.go, audience.go); nothing else here
// changes without changing the contract every one of them was built against.
//
// THE KIND TABLE (kind.go) is the one place a kind's rules are decided: native
// or framed, its entry, its format, its store and its search, with every
// surface asking it rather than switching on a kind's name (ADR-0042).
//
// # Zero values
//
// Every enum here is a named string type with typed constants and a Valid
// method, and its zero value is invalid, so a field somebody forgot to fill in
// is refused rather than read as the first member. A name this build does not
// know arrives as a value, never a panic: a newer peer may write one during a
// rolling upgrade.
package artifacts
