package engine

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/store"
)

// keptReadings is what this node last kept of each sign-in's windows in its
// own file (`llm_sign_in_window`), so a heartbeat writes only what is newer.
type keptReadings struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// keepReadings writes every window a harness entry's sign-in heard since it
// was last kept: what [Engine.recallReadings] reads back after a restart.
func (e *Engine) keepReadings(ctx context.Context, c *Company) {
	if e.backends == nil || e.backends.Store == nil || c == nil || c.Models == nil {
		return
	}
	type row struct {
		signIn string
		limit  harness.Limit
	}
	var fresh []row
	k := &e.readings
	k.mu.Lock()
	if k.seen == nil {
		k.seen = map[string]time.Time{}
	}
	for _, p := range c.Models.All() {
		agent, ok := p.(*harness.Provider)
		if !ok {
			continue
		}
		signIn := string(agent.SignInID())
		for _, l := range agent.Plan().State(e.now()).Limits {
			id := signIn + "\x00" + l.Key()
			if l.Seen.After(k.seen[id]) {
				k.seen[id] = l.Seen
				fresh = append(fresh, row{signIn: signIn, limit: l})
			}
		}
	}
	k.mu.Unlock()
	if len(fresh) == 0 {
		return
	}
	err := e.backends.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, r := range fresh {
			l := r.limit
			var used sql.NullFloat64
			if l.Used != nil {
				used = sql.NullFloat64{Float64: *l.Used, Valid: true}
			}
			var resets sql.NullInt64
			if !l.ResetsAt.IsZero() {
				resets = sql.NullInt64{Int64: store.EncodeTime(l.ResetsAt), Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO llm_sign_in_window
				(sign_in, win, model, hours, state, used, resets_at, seen) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (sign_in, win, model) DO UPDATE SET hours = excluded.hours,
				state = excluded.state, used = excluded.used, resets_at = excluded.resets_at,
				seen = excluded.seen WHERE excluded.seen > llm_sign_in_window.seen`,
				r.signIn, string(l.Window), l.Model, l.Hours, string(l.State), used, resets,
				store.EncodeTime(l.Seen)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.WarnContext(ctx, "llm_sign_in_windows_unkept", "error", err.Error(),
			"detail", "a restart before the next heartbeat starts these sign-ins with no reading")
	}
}

// recallReadings reads back what this node kept of c's sign-ins' windows into
// their plans: a window's kept reading replaces none newer
// ([harness.Plan.Observe]), so recalling at every apply is safe.
func (e *Engine) recallReadings(ctx context.Context, c *Company) {
	if e.backends == nil || e.backends.Store == nil || c == nil || c.Models == nil {
		return
	}
	plans := map[string]*harness.Plan{}
	for _, p := range c.Models.All() {
		if agent, ok := p.(*harness.Provider); ok {
			plans[string(agent.SignInID())] = agent.Plan()
		}
	}
	if len(plans) == 0 {
		return
	}
	err := e.backends.Store.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT sign_in, win, model, hours, state, used, resets_at, seen FROM llm_sign_in_window`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				signIn, window, model, state string
				hours                        int
				used                         sql.NullFloat64
				resets                       sql.NullInt64
				seen                         int64
			)
			if err := rows.Scan(&signIn, &window, &model, &hours, &state, &used, &resets, &seen); err != nil {
				return err
			}
			plan := plans[signIn]
			if plan == nil || !llm.LimitWindow(window).Valid() || !llm.LimitState(state).Valid() {
				continue
			}
			r := llm.LimitReading{Window: llm.LimitWindow(window), Hours: hours, Model: model,
				State: llm.LimitState(state)}
			if used.Valid {
				u := used.Float64
				r.Used = &u
			}
			if resets.Valid {
				r.ResetsAt = store.DecodeTime(resets.Int64)
			}
			plan.Observe([]llm.LimitReading{r}, store.DecodeTime(seen))
		}
		return rows.Err()
	})
	if err != nil {
		log.WarnContext(ctx, "llm_sign_in_windows_unread", "error", err.Error(),
			"detail", "these sign-ins start with no reading until their next call reports one")
	}
}
