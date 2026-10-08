package statelog

// How far back an operation ledger may have lost rows, and the one thing that
// loses them.
//
// A domain's ledger is what makes a retry safe: a retry of an operation whose
// row is present is answered with the first copy, and one whose row is absent
// is decided afresh. That reading of absence is sound only where the ledger
// never lost a row it once held, so the one way it can lose one records the
// instant before which it may have — the ledger's WATERMARK, in
// `statelog_ops_lost`, beside the ledger in the same file ([Rows.LostBefore],
// read by [Publisher.vouches]). Every write moves it only forward.
//
// THE SWEEP deletes rows applied more than [OpsRetention] ago and records its
// cutoff in the transaction that deletes them ([tables.purgeOps], through
// [tables.markLost]), and it is the watermark's only writer.
//
// AN ADOPTION LOSES NOTHING. The ledger TRAVELS inside a snapshot
// ([Domain.OpsTable]) and so does the watermark, so the adopter holds the
// donor's rows and inherits how far back the donor's own sweep had lost any —
// the file and the watermark that describes it are installed by one rename.
