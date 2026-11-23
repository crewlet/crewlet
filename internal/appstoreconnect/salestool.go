package appstoreconnect

import (
	"context"
	"errors"
	"net/url"
	"slices"
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
	// A READ NO SEAT KEY MAY MAKE goes through the company's report key, unless
	// the seat's own identity was refused, which stops every call of its.
	if _, err := p.signer(ctx, call.Handle); err != nil {
		return nil, err
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
	bySKU, err := p.skusOf(ctx, t, seat.Apps)
	if err != nil {
		return nil, said(err)
	}
	for _, r := range rows {
		if app, ok := attributed(r, seat.Apps, bySKU); ok {
			out.Rows = append(out.Rows, app)
		}
	}
	if len(out.Rows) == 0 {
		out.EmptyReason = growth.EmptyNoData
	}
	p.note(ctx, growth.StepMade, recordOf(call, made{op: op, class: growth.ClassRead, team: t, path: "/v1/salesReports",
		query: q, slot: slot, signer: growth.SignerCompany}))
	return out, nil
}

// skusOf maps the SKU of each of apps the team reaches to its app.
func (p *Product) skusOf(ctx context.Context, t *team, apps []string) (map[string]string, error) {
	infos, err := p.appsOf(ctx, t)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, a := range infos {
		if a.SKU != "" && slices.Contains(apps, a.ID) {
			out[a.SKU] = a.ID
		}
	}
	return out, nil
}

// attributed is r as one of apps' rows, false for another app's. An in-app
// purchase's or subscription's row names its own ID where an app's names the
// app's, and its app by the app's SKU (Apple's sales report reference).
func attributed(r SalesRow, apps []string, bySKU map[string]string) (SalesRow, bool) {
	if r.parent == "" {
		return r, slices.Contains(apps, r.App)
	}
	app, ok := bySKU[r.parent]
	if !ok {
		return SalesRow{}, false
	}
	r.Item, r.App = r.App, app
	return r, true
}
