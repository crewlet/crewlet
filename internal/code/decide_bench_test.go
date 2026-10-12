package code

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// BenchmarkDecideABatch is ONE ROUND of the push decide — the snapshot's two
// reads and every command against every rule — over a repository of ten
// thousand refs and [MaxProtectionsPerRepo] rules of the shape a team's
// convention writes (refs/heads/teamNN/release/**), each rule asked about each
// command because none matches:
//
//   - maximal: three pushes of [MaxCommandsPerPush] ref creations, the most a
//     record's rows hold — the round a burst of wide pushes pays;
//   - group: [MaxPushesPerRecord] pushes of one command each, the round a
//     burst of small pushes pays.
//
// A round runs on the repository's ONE committer goroutine and inside the
// publisher's snapshot — one read transaction, which holds a connection of
// the replicated estate's reader pool for its length — and again on every
// round a peer's record forces; doc.go's measured section records the
// figures.
func BenchmarkDecideABatch(b *testing.B) {
	const repo = "0190f3c4-0000-7000-8000-00000000be01"
	e := openApplyEstate(b)
	settings := settingsFor("")
	for i := range MaxProtectionsPerRepo {
		settings.Protections = append(settings.Protections, Protection{
			Pattern: fmt.Sprintf("refs/heads/team%02d/release/**", i), DirectPush: DirectNone,
		})
	}
	e.apply(rec(b, NameSubject("bench"), OpCreate,
		Scope{NamePath(NameToken("bench")), RepoPath(repo)},
		CreateRepo{RepoID: repo, Name: "bench", Settings: settings}))
	e.seed(repo, 0, 10_000, 0)

	proposal := func(cmds []Command) Proposal {
		return Proposal{
			PushID: uuid.Must(uuid.NewV7()).String(), RepoID: repo,
			Principal: Principal{Kind: PrincipalOperator, OperatorID: "op", Seat: "op"},
			Node:      "node-a", Commands: cmds, ReceivedAt: applyClock,
		}
	}
	var maximal, group []Proposal
	for p := range 3 {
		var cmds []Command
		for c := range MaxCommandsPerPush {
			cmds = append(cmds, Command{Ref: fmt.Sprintf("refs/heads/team%02d/work/%d/%04d", c%100, p, c),
				Old: ZeroOID, New: oid(fmt.Sprint(p, c))})
		}
		maximal = append(maximal, proposal(cmds))
	}
	for p := range MaxPushesPerRecord {
		group = append(group, proposal([]Command{{Ref: fmt.Sprintf("refs/heads/team%02d/work/one", p),
			Old: ZeroOID, New: oid(fmt.Sprint(p))}}))
	}
	for _, shape := range []struct {
		name  string
		batch []Proposal
	}{{"maximal", maximal}, {"group", group}} {
		b.Run(shape.name, func(b *testing.B) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if err := e.db.Read(ctx, func(tx *sql.Tx) error {
					entries, _, _, err := decideBatch(ctx, tx, repo, shape.batch, DefaultLimits(), applyClock)
					if err == nil && len(entries) != len(shape.batch) {
						err = fmt.Errorf("%d entries of %d pushes: the round refused what it measures",
							len(entries), len(shape.batch))
					}
					return err
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
