package codetest

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The package's own tests hold its setup helpers to the outcomes statelog
// answers (pending_test.go).
var (
	NewCase   = newCase
	PushEach  = pushEach
	BulkPacks = bulkPacks
)

// Gesture is gesture with the store's own publication handed in.
func (d *DataNode) Gesture(t testing.TB, node string,
	op func(*code.Store, context.Context, code.Actor, string, string) (statelog.Result, error),
	name string) statelog.Position {

	t.Helper()
	return d.gesture(t, node, op, name)
}
