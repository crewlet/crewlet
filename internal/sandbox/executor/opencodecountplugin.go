package executor

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// openCodeCountPlugin is opencode_count.js, which records a capped OpenCode
// run's model requests off its server's own events for its turn cap
// ([openCodeCountRecord]). Written as its directory's `index` module
// ([Paths.OpenCodeCountPlugin], [openCodeSteerEntry]).
//
//go:embed opencode_count.js
var openCodeCountPlugin []byte

// countedRecords implements [recordsCounted]: the count plugin's record
// beside the stream. The stream shows the main session's steps alone, never
// a subagent's (measured on 2.3.2: 4 of 7 with a subagent's 3) nor a
// compaction's summary requests; the plugin's record shows each request the
// server answered, every session's, one retried after a 429 or a 500 once
// (measured: 7 for 7, 17 for 17 with two subagents and a 429, 6 for 6 beside
// a 429 and a 500, 3 for 3 with a refused compaction's two attempts), about
// 0.1 s after the answer. So the count is their larger, live and at
// collection. A request the server never answered is in neither record.
func (OpenCode) countedRecords() []recordCounted { return []recordCounted{openCodeCountRecord{}} }

// openCodeCountRecord is the count plugin's record, a line a request.
type openCodeCountRecord struct{}

// liveRecord implements [recordCounted]: the run's own file, cleared before
// the run starts: no pointer to read. ABOUT 60 BYTES A REQUEST, so at most
// some 60 KB at the cap's ceiling ([config.RoundCapCeiling]).
//
//nolint:unparam // the interface's: the runners whose scripts find their record read a pointer that can fail.
func (openCodeCountRecord) liveRecord(_ context.Context, _ sandbox.Sandbox, paths Paths) (string, int, bool, error) {
	return paths.OpenCodeCountRecord(), 0, true, nil
}

// recordTally implements [recordCounted].
func (openCodeCountRecord) recordTally(context.Context, sandbox.Sandbox, Paths) (recordTally, error) {
	return &openCodeCountTally{}, nil
}

// collectsFromRecord implements [recordCollected].
func (openCodeCountRecord) collectsFromRecord() {}

// unarmed implements [armedRecord]: a capped run whose plugin never wrote its
// start line had nothing recording its requests.
func (openCodeCountRecord) unarmed(turnCap int) string {
	return fmt.Sprintf("this run had a turn cap of %d model requests, and the plugin that records them for it "+
		"never loaded, so its requests were not counted", turnCap)
}

// openCodeCountTally is the count plugin's record read: its request lines,
// and whether its start line stood.
type openCodeCountTally struct {
	n, unread int
	started   bool
}

func (o *openCodeCountTally) Line(line []byte) {
	var entry struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &entry) != nil {
		o.unread++
		return
	}
	switch entry.Type {
	case "start":
		o.started = true
	case "request":
		o.n++
	}
}

// Skipped is a line the plugin never writes: its lines are a few dozen bytes.
func (o *openCodeCountTally) Skipped(LongLine) { o.unread++ }
func (o *openCodeCountTally) requests() int    { return o.n }
func (o *openCodeCountTally) undecided() int   { return o.unread }
func (o *openCodeCountTally) armed() bool      { return o.started }
