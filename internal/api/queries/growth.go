package queries

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `GET /growth/changes` (ADR-0171): the Growth page's recent agent changes,
// read off the access log's growth records, newest first: every step of a call
// that was not a read, for one product (`product=`) or all, paged by `cursor=`
// and `limit=`. An administrator's, as the company-wide trail is
// ([authz.ActionAuditRead]): the records name no credential and no person's
// data, and a published text is its digest.

// GrowthReads is the access log's growth records, as this read takes them.
type GrowthReads interface {
	Changes(ctx context.Context, q accessdomain.GrowthQuery, fresh statelog.Freshness) (accessdomain.GrowthPage, error)
}

// growthChanges answers the read.
func (s Sources) growthChanges(ctx context.Context, p Params) (any, error) {
	asking, err := caller(ctx, "growth_changes")
	if err != nil {
		return nil, err
	}
	d := authz.Decide(ctx, asking, authz.ActionAuditRead, authz.Object{Kind: authz.KindCompany}, s.Chart, s.clock())
	switch {
	case d.Unknown():
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, d.Err)
	case !d.Allowed:
		return nil, NewRefusal("growth_changes", d)
	}
	page, err := s.Growth.Changes(ctx, accessdomain.GrowthQuery{Product: p.String("product"),
		Cursor: p.String("cursor"), Limit: Clamp(p.Int("limit", 0), accessdomain.DefaultPageSize,
			accessdomain.MaxPageSize)}, statelog.Freshness{Level: statelog.ReadConsistentPrefix})
	switch {
	case errors.Is(err, accessdomain.ErrInvalid):
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
	case err != nil:
		return nil, fmt.Errorf("%w: the access log could not be read: %w", ErrUnavailable, err)
	}
	rows := make([]map[string]any, 0, len(page.Rows))
	for _, g := range page.Rows {
		rows = append(rows, growthRow(g))
	}
	return map[string]any{"rows": rows, "next": page.Next, "complete": page.Complete}, nil
}

// growthRow is one growth record as the page shows it: money as decimal
// strings of micros, so an int64 stays exact in a browser.
func growthRow(g accessdomain.GrowthRow) map[string]any {
	changes := make([]map[string]any, 0, len(g.Changes))
	for _, c := range g.Changes {
		row := map[string]any{"field": c.Field, "label": c.Label, "money": c.Money, "text": c.Text}
		if c.Text {
			row["before_sha256"], row["before_bytes"] = c.BeforeSHA256, c.BeforeBytes
			row["after_sha256"], row["after_bytes"] = c.AfterSHA256, c.AfterBytes
		} else {
			row["before"], row["after"] = c.Before, c.After
		}
		changes = append(changes, row)
	}
	var money any
	if m := g.Money; m != nil {
		money = map[string]any{"currency": m.Currency,
			"delta_micros": strconv.FormatInt(m.DeltaMicros, 10), "daily_micros": strconv.FormatInt(m.DailyMicros, 10),
			"daily_after_micros":    strconv.FormatInt(m.DailyAfterMicros, 10),
			"per_change_cap_micros": strconv.FormatInt(m.PerChangeCapMicros, 10),
			"daily_cap_micros":      strconv.FormatInt(m.DailyCapMicros, 10)}
	}
	return map[string]any{"seat": g.Handle, "turn": g.Turn, "act": g.Act(), "held": g.HeldCall != "",
		"product": g.Product, "connection": g.Connection, "account": g.Account, "target": g.Target,
		"tool": g.Tool, "class": g.Class, "step": g.Step, "answered_by": g.AnsweredBy, "answerer": g.Answerer,
		"via":       g.Via,
		"operation": g.Operation, "before": g.Before, "changes": changes, "money": money,
		"failure": g.Failure, "at": g.At.UTC()}
}
