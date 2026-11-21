package appstoreconnect

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/period"
)

// salesZone is the zone Apple cuts a sales day in.
const salesZone = "America/Los_Angeles"

// Sales is the sales tool's answer: one Pacific day's summary, for the seat's
// apps, money per proceeds currency.
type Sales struct {
	Date     string     `json:"date"`
	TimeZone string     `json:"time_zone"`
	Rows     []SalesRow `json:"rows"`
	// EmptyReason says why Rows is empty.
	EmptyReason growth.EmptyReason `json:"empty_reason,omitempty"`
	Note        string             `json:"note,omitempty"`
}

// sales reads one day's sales summary for the seat's apps.
func (p *Product) sales(ctx context.Context, call growth.Call) (any, error) {
	seat, err := p.seat(call.Handle)
	if err != nil {
		return nil, err
	}
	zone, err := period.LoadZone(salesZone)
	if err != nil {
		return nil, err
	}
	date, _ := call.Args["date"].(string)
	if date == "" || date == "yesterday" {
		days, _ := growth.RangeYesterday.Resolve(p.deps.Now(), zone)
		date = days.Start
	}
	if _, perr := time.Parse(time.DateOnly, date); perr != nil {
		return nil, invalid("`date` is a day as YYYY-MM-DD, or yesterday.")
	}
	name, _ := call.Args["connection"].(string)
	t, err := p.teamNamed(name)
	if err != nil {
		return nil, err
	}
	if t.vendor == "" {
		return nil, growth.Refuse(growth.RefusedUnavailable, "add the team's vendor number to read sales. No call was made.")
	}
	op, _ := Catalog().Operation(opSales)
	slot, ok := keyFor(op, t.slots, false)
	if !ok {
		return nil, growth.Refuse(growth.RefusedForbidden, "sales need a Finance, Sales or Access to Reports key. No call was made.")
	}
	q := url.Values{"filter[frequency]": {"DAILY"}, "filter[reportType]": {"SALES"}, "filter[reportSubType]": {"SUMMARY"},
		"filter[vendorNumber]": {t.vendor}, "filter[reportDate]": {date}}
	resp, err := t.client.Do(ctx, slot, Request{Method: "GET", Path: "/v1/salesReports", Query: q, Read: true,
		Accept: "application/a-gzip"})
	out := Sales{Date: date, TimeZone: salesZone}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == 404 {
		out.EmptyReason, out.Note = growth.EmptyNotReady, "Apple publishes a day's sales by about 8 a.m. Pacific time "+
			"the next day; a day with no sales has no report."
		return out, nil
	}
	if err != nil {
		return nil, said(err)
	}
	rows, err := readSales(resp.Body)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, a := range seat.Apps {
		allowed[a] = true
	}
	for _, r := range rows {
		if allowed[r.App] {
			out.Rows = append(out.Rows, r)
		}
	}
	if len(out.Rows) == 0 {
		out.EmptyReason = growth.EmptyNoData
	}
	p.note(ctx, growth.StepMade, recordOf(call, made{op: op, class: growth.ClassRead, team: t, path: "/v1/salesReports",
		query: q}))
	return out, nil
}
