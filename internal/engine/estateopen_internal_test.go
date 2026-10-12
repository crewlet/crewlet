package engine

import "github.com/crewlet/crewlet/internal/store"

// replicatedOpen reports whether the replicated estate is open on a node's
// store — what a join's install closes and a restore reopens.
func replicatedOpen(db *store.DB) bool {
	_, err := db.ReplicatedDB()
	return err == nil
}
