package code

import (
	"context"
	"database/sql"
)

// A PUSH THAT LANDS ON A PROTECTED BRANCH (M5-D17): every record that moves a
// protected branch leaves its LANDING ROW — a merge's ([Merge]), a queue's
// ([Land]) and a push's — so a branch's tip checks judge, and its green row
// follows, every way it moved. A push moves a protected branch only as a
// person's own push under direct_push persons, their recorded override
// (ADR-0066): no protection takes a seat's direct push.
//
// The push's DECIDE stamps its landings ([PushEntry.Landings]) from the rules
// its snapshot holds — the push half it already asks every command about, and
// the branch's gate only for a round with a landing ([pushGates]) — so the
// apply writes the rows without matching a rule itself: kind `push`, naming
// the pusher, green where the branch's gate requires no check, the branch's
// green row then moving with it as a direct merge's does.

// pushGates is one push round's reading of its repository's merge gates — the
// checks each branch a push lands on requires — read on the round's first
// landing and kept for the rest of the round.
type pushGates struct {
	repoID string
	rules  []Protection
	read   bool
}

// landings is the landings cmds — an entry's commands as its decide found them
// — make under rules, the round's push rules: one for each ok command moving a
// branch a rule protects onto a commit, in command order, green where the
// branch's gate requires no check.
func (g *pushGates) landings(ctx context.Context, tx *sql.Tx, rules Protections, cmds []Command) ([]PushLanding,
	error) {

	var out []PushLanding
	for i, c := range cmds {
		if c.Result != ResultOK || !lands(c) || !rules.protects(c.Ref) {
			continue
		}
		if !g.read {
			read, err := rulesOfRepos(ctx, tx, []readinessSubject{{repo: RepoRef{ID: g.repoID}}})
			if err != nil {
				return nil, err
			}
			g.rules, g.read = read[g.repoID], true
		}
		gate, err := GateRulesOf(g.rules, c.Ref, "")
		if err != nil {
			return nil, err
		}
		out = append(out, PushLanding{Command: i, Green: len(gate.Checks) == 0})
	}
	return out, nil
}

// writePushLandings writes the landing rows of pushes, a record's entries in
// its order — each landing's ordinal its place among the record's landings on
// its branch — and moves each green one's branch's green row onto it, the
// last in the record's order winning. It answers the rows it wrote.
func writePushLandings(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	pushes []PushEntry) (int, error) {

	var out []landing
	ordinals := map[string]int{}
	for _, e := range pushes {
		by, _ := e.Principal.party()
		if by == "" {
			by = e.Principal.Actor()
		}
		for _, l := range e.Landings {
			if l.Command < 0 || l.Command >= len(e.Commands) || e.Commands[l.Command].Result != ResultOK {
				// A RECORD NO CORRECT LOG CARRIES — its decide stamps a
				// landing on an ok command alone — lands the push and writes
				// no such landing, logged: a refusal would stop the log on
				// every node.
				applyLog.WarnContext(ctx, "code_landing_not_applied", "repo", repoID, "push", e.PushID,
					"command", l.Command, "position", at.position.String())
				continue
			}
			c := e.Commands[l.Command]
			ordinals[c.Ref]++
			out = append(out, landing{target: c.Ref, ordinal: ordinals[c.Ref], kind: LandedPush, base: c.Old,
				commit: c.New, by: by, green: l.Green})
		}
	}
	if len(out) == 0 {
		return 0, nil
	}
	if err := writeLandings(ctx, tx, at, repoID, out); err != nil {
		return 0, err
	}
	rows := len(out)
	for _, l := range out {
		if !l.green {
			continue
		}
		if err := advanceGreen(ctx, tx, at, repoID, l.target, l.commit); err != nil {
			return 0, err
		}
		rows++
	}
	return rows, nil
}
