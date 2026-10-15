// Package artifactsdomain is the artifacts STATE-LOG DOMAIN: every fact about an
// artifact except its datastore and its bytes. Artifacts, versions and their
// manifests, the objects each artifact names, assets, shares, pins and
// recently-opened marks, the change record and the company's usage counters are
// records on one strictly ordered stream (CREWLET_ARTIFACTS_LOG), applied by
// every data node into its replicated estate. internal/artifacts is the
// vocabulary it speaks; internal/artifactdata is the datastore beside it, on a
// log of its own.
//
// # Subjects
//
// The subject is the arbitration unit. An artifact's own subject orders every
// record that moves an object reference: a publish, a rollback, a version's
// retirement and a purge, so the broker orders every carry against every
// removal. A republish-in-place address is a SOURCE subject, create-only, so two
// first publishes of one source contend there. An asset rides a subject of its
// own beneath its artifact (never carried, so it needs no artifact ordering),
// and a reader's pins and opened marks ride the reader's. The `comment` kind is
// reserved for a later build and has no op here.
//
// # The carry rule, and why the collector needs no lock
//
// A version names every file by object (key, digest, size). A file the artifact
// already holds is CARRIED: the new entry names the existing object and no byte
// moves. The applier writes a carried entry only when its own transaction finds
// the object named by THIS artifact with that digest and size, and an uploaded
// entry only when no other artifact or asset names its key; otherwise the
// record is refused on every data node and its writer told the paths. So once no row
// names a key, no later record can name it (ADR-0027 as extended for carrying),
// and a carry never crosses an artifact, which keeps a hash lookup from telling a
// writer whether an artifact they cannot read holds a file.
//
// # Retention rides the record
//
// A publish names the exact unlabelled versions it retires, decided in the
// writer's snapshot so the artifact keeps at most a hundred unlabelled versions
// plus every labelled one, and a label's removal retires in the same record what
// it pushes past that bound. Every data node deletes the same rows at the same
// position; no node deletes a durable row on its own authority.
//
// # Quotas are stamped
//
// The node that decides a quota-consuming record (a create, a publish, a
// rollback, an asset) stamps the Tier A limits in force into it, refuses early
// from its own snapshot, and the applier enforces the stamp against the
// replicated counters, so two writes racing past a quota apply identically on
// every data node: the second is refused everywhere.
//
// # A refusal is a row
//
// A record the broker accepted that the rows refuse (a head that moved, a
// carried object gone, a quota crossed) applies as a no-op that writes its
// history row with the outcome `refused` and the reason, on every data node alike,
// and the writer reads its own verdict back by operation id. An error would stall
// every data node's log on a record that is merely late. For the same reason the
// applier applies no rule a later build may change: the media table, the path
// grammar and every bound in internal/artifacts/limits.go are the writer's.
//
// # What a batch moved
//
// After each committed batch the applier hands the engine what it moved (a new
// version, a narrowing's access epoch, a trash, a purge), built from the rows it
// wrote, and the engine fans it out to the serving origin's authorization cache
// and the socket hub ([Moved]). A batch that retained a record this build cannot
// read moves everything ([MoveReset]). No record wakes a seat in this release, so
// the domain declares no feed.
//
// # Attribution is a seat handle
//
// Every row names who acted by seat handle (or a machine login) and the actor's
// kind, never by display name, so a person's name can be pseudonymised without
// rewriting history. Every instant is the broker's.
package artifactsdomain
