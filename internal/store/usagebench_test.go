package store_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// BenchmarkUsageForDay is what deriving one company day costs a node, and what
// the reads of unit repositories add to it: the same busy day of every other
// usage type — forty seats' thousand turns, three phases each, their two
// thousand knowledge reads and two hundred auxiliary records — with no
// repository read, and with fifty thousand records of them, a company whose
// CI and seats read its unit repositories all day: a thousand principals over
// twenty repositories, both ways, spread across the day, each record one
// reader's reads of a flush (internal/code/readaudit), one to four of them.
//
// THE DELTA DECIDED A SHAPE (M3-decisions M3-O22). The publisher derives
// today and yesterday every fifteen seconds the day moves, so whatever the
// reads add is paid on every such tick, and a derived day may add 150 ms — 1 %
// of the tick. Grouped by fields read out of each row's payload the reads
// added 272 ms (334 ms against 62 ms), so the fields the statement groups and
// sums on are columns of their own (node/0059, and node/0060 each record's
// count of reads), on which the records add 116 ms (193 ms against 77 ms,
// beside another session's builds; 86 ms, 151 against 65, when each row was
// one read and counted). Every figure was taken with `-benchtime 5x -count 1`
// on two cores of an Intel Core Ultra 7 265U; the migrations and the
// derivation's doc (store.DB.usageRepoReads) state them.
func BenchmarkUsageForDay(b *testing.B) {
	day := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	w := usageDayOf(day, day.Add(24*time.Hour))
	for _, fetches := range []int{0, 50_000} {
		b.Run(fmt.Sprintf("code_fetched=%d", fetches), func(b *testing.B) {
			db := storetest.OpenNode(b, filepath.Join(b.TempDir(), "store.db"), store.Options{})
			b.Cleanup(func() { _ = db.Close() })
			seedUsageDay(b, db, day, fetches)
			b.ResetTimer()
			for b.Loop() {
				got, err := db.UsageForDay(b.Context(), w)
				if err != nil {
					b.Fatal(err)
				}
				if fetches > 0 && len(got.RepoReads) == 0 {
					b.Fatal("the day derived no repository reads")
				}
			}
		})
	}
}

// seedUsageDay writes one busy day of usage records into db, and fetches
// records of reads of unit repositories beside them, through the production
// writer's custody path — written and settled as kept, in batches, so every
// row counts.
func seedUsageDay(b *testing.B, db *store.DB, day time.Time, fetches int) {
	b.Helper()
	const (
		seats, turnsPerSeat, phasesPerTurn = 40, 25, 3
		readsPerTurn, auxiliary            = 2, 200
		repos, principals                  = 20, 1000
		batch                              = 2000
	)
	var recs []store.EventRecord
	encode := func(v any) []byte {
		raw, err := json.Marshal(v)
		if err != nil {
			b.Fatal(err)
		}
		return raw
	}
	at := func(i, of int) time.Time { return day.Add(time.Duration(i) * (24 * time.Hour) / time.Duration(of+1)) }
	n := 0
	for s := range seats {
		agent, handle := fmt.Sprintf("seat-%02d", s), fmt.Sprintf("dev%02d", s)
		for turn := range turnsPerSeat {
			id := fmt.Sprintf("t-%02d-%02d", s, turn)
			when := at(s*turnsPerSeat+turn, seats*turnsPerSeat)
			for p := range phasesPerTurn {
				payload := encode(map[string]any{"agent_id": agent, "role": "Dev", "turn_id": id,
					"phase": []string{"execute", "review", "execute"}[p], "model": "m-1",
					"provider_key": "primary", "input_tokens": 4000, "output_tokens": 400,
					"total_tokens": 4400})
				recs = append(recs, store.EventRecord{ID: fmt.Sprintf("p-%d", n),
					Type: "agent_phase_completed", Time: when.Add(time.Duration(p) * time.Second),
					Category: "system", Tags: store.ExtractTags(payload), Payload: payload})
				n++
			}
			end := encode(types.TurnCompleted{Agent: agent, AgentHandle: handle, RoleName: "Dev", TurnID: id,
				DurationMS: 60_000, Turn: &types.TurnTally{DurationMS: 60_000, Reviews: 1}})
			summary := encode(types.AgentTurnCompleted{Agent: agent, RoleName: "Dev", TurnID: id,
				TotalTokens: 13_200})
			recs = append(recs,
				store.EventRecord{ID: fmt.Sprintf("e-%d", n), Type: "turn_completed",
					Time: when.Add(4 * time.Second), Category: "learning", Tags: store.ExtractTags(end),
					Payload: end},
				store.EventRecord{ID: fmt.Sprintf("s-%d", n), Type: "agent_turn_completed",
					Time: when.Add(5 * time.Second), Category: "system", Tags: store.ExtractTags(summary),
					Payload: summary})
			for r := range readsPerTurn {
				read := encode(map[string]any{"agent_id": agent, "agent_handle": handle, "role": "Dev",
					"turn_id": id, "via": "search", "backend": "native", "query": "deploy",
					"pages": []map[string]any{{"id": fmt.Sprintf("page-%d", (s+r)%60), "rank": 1}}})
				recs = append(recs, store.EventRecord{ID: fmt.Sprintf("r-%d-%d", n, r),
					Type: "knowledge_read", Time: when.Add(2 * time.Second), Category: "learning",
					Tags: store.ExtractTags(read), Payload: read})
			}
			n++
		}
	}
	for a := range auxiliary {
		aux := encode(types.AuxiliarySpend{Agent: fmt.Sprintf("seat-%02d", a%seats), RoleName: "Dev",
			Stage: types.AuxStageReflection, Purpose: types.AuxPersistDecider, Model: "haiku",
			ProviderKey: "cheap", Calls: 2, InputTokens: 900, OutputTokens: 30, TotalTokens: 930})
		recs = append(recs, store.EventRecord{ID: fmt.Sprintf("a-%d", a), Type: "auxiliary_spend",
			Time: at(a, auxiliary), Category: "system", Tags: store.ExtractTags(aux), Payload: aux})
	}
	for f := range fetches {
		// A RECORD IS A READER'S READS OF A FLUSH (internal/code/readaudit):
		// one to four of them here, which the derivation sums, at the same
		// cost a record whatever it counts.
		reads := int64(1 + f%4)
		read := types.CodeFetched{RepoID: fmt.Sprintf("repo-%02d", f%repos),
			RepoName: fmt.Sprintf("unit-%02d", f%repos), PrincipalKind: types.CodeReaderOperator,
			Principal: fmt.Sprintf("pat:%04d", f%principals), Seat: fmt.Sprintf("person%03d", f%principals%300),
			Via: types.FetchedViaGit, Reads: reads, Bytes: reads * 48_000}
		if f%3 == 0 {
			read.PrincipalKind, read.Principal, read.Seat = types.CodeReaderAgent, fmt.Sprintf("dev%02d", f%seats),
				fmt.Sprintf("dev%02d", f%seats)
			read.Via, read.Kind, read.Bytes = types.FetchedViaTool, "blob", reads*9_000
		}
		payload := encode(read)
		recs = append(recs, store.EventRecord{ID: fmt.Sprintf("f-%d", f), Type: read.EventType(),
			Actor: read.Actor(), Time: at(f, fetches), Category: "system", Tags: store.ExtractTags(payload),
			Payload: payload})
	}
	log := db.Events()
	for start, i := 0, 0; start < len(recs); start, i = start+batch, i+1 {
		id := fmt.Sprintf("seed-%d", i)
		if err := log.WriteCustody(b.Context(), store.CustodyBatch{ID: id, Origin: "bench",
			Records: recs[start:min(start+batch, len(recs))]}, day); err != nil {
			b.Fatal(err)
		}
		if err := log.SettleCustody(b.Context(), id, true); err != nil {
			b.Fatal(err)
		}
	}
}
