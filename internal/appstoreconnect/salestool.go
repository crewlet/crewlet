package appstoreconnect

import (
	"context"
	"errors"
	"fmt"
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
	key, err := p.keyOf(ctx, call.Handle)
	if err != nil {
		return nil, err
	}
	op, _ := Catalog().Operation(opSales)
	if !growth.Permits(op, []string{key.role}) {
		fix := setUpIdentity
		if growth.LevelOf(seat.Level) != growth.LevelRead {
			// A SALES KEY FITS THE READ LEVEL ALONE (roleFit).
			fix = "Only an agent at the read level holds one; a person sets one up for such an agent."
		}
		return nil, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("this agent's key has the %s role; sales "+
			"need a Sales key. %s No call was made.", roleName(key.role), fix))
	}
	if key.detail == "" {
		return nil, growth.Refuse(growth.RefusedUnavailable, "sales need the team's vendor number, which this "+
			"agent's key was handed in without. A person hands the key in again with it, on the integration's "+
			"Agents tab. No call was made.")
	}
	t := key.team
	q := url.Values{"filter[frequency]": {"DAILY"}, "filter[reportType]": {"SALES"}, "filter[reportSubType]": {"SUMMARY"},
		"filter[vendorNumber]": {key.detail}, "filter[reportDate]": {date}}
	resp, err := key.client.Do(ctx, ownSlot, Request{Method: "GET", Path: "/v1/salesReports", Query: q, Read: true,
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
	bySKU, err := p.skusOf(ctx, t, key.client, seat.Apps)
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
		query: q, key: key}))
	return out, nil
}

// skusOf maps the SKU of each of apps the team holds to its app.
func (p *Product) skusOf(ctx context.Context, t *team, c *Client, apps []string) (map[string]string, error) {
	infos, err := p.appsOf(ctx, t, c)
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
