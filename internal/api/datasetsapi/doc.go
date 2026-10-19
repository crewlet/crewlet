// Package datasetsapi answers a dashboard artifact's datasets AS THEIR VIEWER
// (`GET /artifacts/{id}/versions/{n}/sources/{dataset}`), and lists the engine
// source catalogue (`GET /artifacts/sources`).
//
// # The rule (adr/0043)
//
// No row leaves the engine that its viewer could not read directly. A board is
// one spec for every reader, and every figure on it is each reader's own:
//
//   - The board is admitted as any read of a version's content is
//     ([artifactsapi.Service.AdmitContent]): a share decides who may open it,
//     an administrator's override is recorded before anything is read, and an
//     artifact in the trash answers nothing.
//   - An `engine` dataset is answered by the question the dashboard's own
//     screens ask (the tracker's `work_items` and `work_workload`, spend's
//     `tokens` and `token_series`) through the question registry, ON THE
//     REQUEST'S OWN CONTEXT, so the principal deciding it is the viewer the
//     guard resolved and the posture, the grants and every per-record rule
//     are the registry's, unchanged. The agents' status is the `GET /agents`
//     overview, decided and shaped by the socket's own audience table. The
//     author of the board is never asked about: sharing a board widens
//     nobody's access to the data behind it.
//   - A `store` dataset reads the artifact's own datastore under the
//     collection's read rule for the reader the admission names, as every
//     datastore read does.
//   - A `file` dataset is the version's own, fixed with it, which the
//     admission already decided.
//   - A `calc` dataset is computed here from its inputs, each read by the rules
//     above as the same viewer; an input the viewer may not read is the
//     calculation's refusal, never a partial answer.
//
// A source the viewer may not read is answered as a REFUSAL of that dataset,
// naming the rule and the grants that would admit them, so the board draws and
// one figure says why it does not. It is not an HTTP failure: the board was
// admitted, and the answer is that one source is closed to this reader.
//
// # A projection, never a second read path
//
// A binding names a question, the parameters the source's arguments become,
// and the fields of its answer that become columns. The answer is the very
// value the registry hands the REST route, encoded as that route would encode
// it, so a column can only ever hold a field the viewer could have read from
// that route. A binding may name only a question whose posture is decided on
// the principal's grants alone ([queries.Everybody] or [queries.Requires]),
// which [New] refuses otherwise: that is what lets an answer be cached per
// principal (below) without a relation the cache key does not carry.
//
// # Cost and freshness
//
// An engine answer is kept for [artifacts.DatasetCacheTTL] under a key derived
// from everything about the principal a bound question can read (its identity,
// its seat, its effective grants, its position and its stage) and the source's
// arguments, so a board open in three tabs costs one domain read, and no
// answer is ever handed to a second principal. Every read that reaches a
// domain or the datastore is charged to the internal/costlimit buckets: one
// viewer on one artifact, and every viewer on it together, per node.
package datasetsapi
