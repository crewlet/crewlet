// Package dashspec is the format of a `dashboard` artifact: the spec a
// version's `dashboard.json` holds, what it may say, and the deterministic
// calculations it may declare.
//
// # A closed format, drawn natively (D61)
//
// A dashboard carries no author code. Its spec names datasets and widgets
// from closed sets, the dashboard draws every widget with its own primitives,
// and every figure names the dataset it was drawn from. A spec outside the
// format is REFUSED at publish with a finding that names the field and the
// rule ([Invalid]), never dropped on read, so an agent learns what to fix
// from the refusal rather than from a board that silently draws less.
//
// # Four sources (D62)
//
//   - `store`: a collection of the artifact's own datastore, live over the
//     socket, read under the collection's read rule for each viewer.
//   - `engine`: a named read from a closed [Catalogue] of the engine's own
//     domains (the tracker, spend, agents), each answered AS THE VIEWER by the
//     domain's existing read path (ADR-0043), so two viewers of one board see
//     what each may read and nothing more.
//   - `file`: a CSV or JSON file of the version, fixed with it.
//   - `calc`: rows derived from other datasets by a few declared [Step]s
//     (filter, group, compute, join, sort, limit), never author JavaScript,
//     and deterministic: one input and one spec give one output on every
//     node and in every run, so a calculation is a formula a viewer can read
//     in the Sources rail rather than a black box.
//
// # Pure
//
// The package reads no store, holds no clock and asks no domain. The publish
// pipeline validates with it, the API runs calculations with it over rows it
// read as the viewer, and the agent tool lists the catalogue from it; each
// reaches one answer about one spec. What a source is BOUND to (which
// question answers `tracker.items`, and how its answer becomes rows) is the
// API's, since only the API holds the viewer.
//
// # What a refresh is (D63)
//
// A store dataset is live. An engine dataset refetches when its domain
// announces a change on a push the dashboard already receives, else on the
// interval it declares (at least [artifacts.MinRefreshSeconds]), else when
// the viewer asks. A file dataset never changes within a version. A
// calculated dataset refreshes whenever an input does. [RefreshOf] is the one
// place that decides it; a dataset an agent keeps on a schedule is a later
// [Refresh] field and a later [RefreshMode], not a second decision.
package dashspec
