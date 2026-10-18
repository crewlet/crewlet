// Package artifactsdomain is the artifacts STATE-LOG DOMAIN: every fact about an
// artifact except its datastore and its bytes. Artifacts, versions and their
// manifests, the objects each artifact names, assets, shares, pins and
// recently-opened marks, the change record and the company's usage counters are
// records on one strictly ordered stream, applied by every data node into its
// replicated estate. internal/artifacts is the vocabulary it speaks;
// internal/artifactdata is the datastore beside it, on a log of its own.
//
// THE ARTIFACT SUBJECT ORDERS EVERY RECORD THAT MOVES AN OBJECT REFERENCE. A
// publish, a rollback, a version prune and a purge all arbitrate on the artifact
// itself, so the broker orders every carry against every removal, and the
// applier writes a carried reference only when its own transaction finds the
// object still named by this artifact. Once no row names a key, no later record
// can name it again, which is what keeps the object collector sound without a
// lock. Assets ride subjects of their own and are never carried.
//
// QUOTAS ARE STAMPED. The node that decides a quota-consuming record stamps the
// Tier A limits in force into it, refuses early from its own snapshot, and the
// applier enforces the stamp against the replicated counters, so two publishes
// racing past a quota apply identically on every node: the second is skipped
// everywhere and its writer reads the skip back.
//
// ATTRIBUTION IS A SEAT HANDLE. Every row names who acted by seat or machine
// login and never by display name, so a person's name can be pseudonymised
// without rewriting history.
package artifactsdomain
