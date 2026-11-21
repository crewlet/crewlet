package statelog

// How far back an operation ledger may have lost rows (package doc, "An
// operation id carries the instant it was minted").
//
// Absence of a ledger row is sound only where the ledger never lost one, so
// each sweep records its cutoff, in the transaction that deletes the rows, as
// a watermark that only moves forward: the table's in `statelog_ops_lost`
// ([tables.purgeOps], [tables.markLost]) and, for a kind with a SHORTER
// horizon ([OpsHorizon.Kinds]), the kind's in `statelog_ops_lost_kind`
// ([tables.purgeOpsOfKind], [tables.markLostKind]). The sweeps are their only
// writers; [Rows.LostBefore] answers the later of the two to
// [Publisher.vouches]. AN ADOPTION LOSES NOTHING: the ledger and both
// watermarks travel in one snapshot ([Domain.OpsTable]).
