package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// estateZeroOpen reports whether layout 0's one partition is open on a node's
// store — what a join's install closes and a restore reopens.
func estateZeroOpen(db *store.DB) bool {
	_, err := db.PartitionDB(statelog.EstatePartition.String())
	return err == nil
}

// closeEstateZero closes layout 0's one partition on a node's store, as a
// join's install does between its close and its rename.
func closeEstateZero(db *store.DB) error {
	return db.ClosePartition(statelog.EstatePartition.String())
}

// estateZeroPath is where layout 0's one partition lives for a node's store,
// open or not.
func estateZeroPath(db *store.DB) string {
	file, err := LayoutZero().File(statelog.EstatePartition)
	if err != nil {
		panic(err)
	}
	return db.PartitionPath(file)
}

// openEstateZero opens layout 0's one partition on a node's store, as the
// runtime's start does, and as a join's install does again after its rename.
func openEstateZero(ctx context.Context, db *store.DB) error {
	file, err := LayoutZero().File(statelog.EstatePartition)
	if err != nil {
		return err
	}
	_, err = db.OpenPartition(ctx, file)
	return err
}
