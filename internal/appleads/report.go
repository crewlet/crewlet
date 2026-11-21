package appleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
)

// The promoted objects a report reads.
const (
	promotedApp   = "app"
	promotedBrand = "brand"
)

// reportLevels are the report tool's levels, in the order it lists them.
var reportLevels = []string{levelCampaign, levelAdGroup, levelKeyword, levelSearchTerm, levelAd}

func rangeNames() []string {
	out := make([]string, 0, len(growth.Ranges()))
	for _, r := range growth.Ranges() {
		out = append(out, string(r))
	}
	return out
}

// restateDays are the most recent days whose figures are marked estimated:
// Apple attributes an install to the day of its tap or view as it learns of
// it, so the last three days' installs may still rise.
const restateDays = 3

var dayShape = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`) })

// reportColumn is one column and where its cells come from in a row.
type reportColumn struct {
	growth.Column
	path string
}

// entityColumns are each level's own columns, read from a row's metadata.
var entityColumns = map[string][]reportColumn{
	levelCampaign: {{growth.Column{Name: "campaign_id", Type: growth.ColumnText}, "id"},
		{growth.Column{Name: "campaign", Type: growth.ColumnText, Untrusted: true}, "name"},
		{growth.Column{Name: "status", Type: growth.ColumnText}, "status"}},
	levelAdGroup: {{growth.Column{Name: "campaign_id", Type: growth.ColumnText}, "campaignId"},
		{growth.Column{Name: "ad_group_id", Type: growth.ColumnText}, "id"},
		{growth.Column{Name: "ad_group", Type: growth.ColumnText, Untrusted: true}, "name"},
		{growth.Column{Name: "status", Type: growth.ColumnText}, "status"}},
	levelKeyword: {{growth.Column{Name: "ad_group_id", Type: growth.ColumnText}, "adGroupId"},
		{growth.Column{Name: "keyword_id", Type: growth.ColumnText}, "id"},
		{growth.Column{Name: "keyword", Type: growth.ColumnText, Untrusted: true}, "text"},
		{growth.Column{Name: "match_type", Type: growth.ColumnText}, "matchType"},
		{growth.Column{Name: "status", Type: growth.ColumnText}, "status"}},
	levelSearchTerm: {{growth.Column{Name: "ad_group_id", Type: growth.ColumnText}, "adGroupId"},
		{growth.Column{Name: "search_term", Type: growth.ColumnText, Untrusted: true}, "searchTermText"},
		{growth.Column{Name: "source", Type: growth.ColumnText}, "searchTermSource"}},
	levelAd: {{growth.Column{Name: "ad_group_id", Type: growth.ColumnText}, "adGroupId"},
		{growth.Column{Name: "ad_id", Type: growth.ColumnText}, "id"},
		{growth.Column{Name: "ad", Type: growth.ColumnText, Untrusted: true}, "name"},
		{growth.Column{Name: "status", Type: growth.ColumnText}, "status"}},
}

// metricColumns are the figures every report carries, read from a row's
// totalMetrics; installs are App Store reports' alone.
func metricColumns(promoted, currency string) []reportColumn {
	out := []reportColumn{{growth.Column{Name: "impressions", Type: growth.ColumnNumber}, "impressions"},
		{growth.Column{Name: "taps", Type: growth.ColumnNumber}, "taps"}}
	if promoted == promotedApp {
		out = append(out, reportColumn{growth.Column{Name: "installs", Type: growth.ColumnNumber}, "totalInstalls"})
	}
	return append(out,
		reportColumn{growth.Column{Name: "spend", Type: growth.ColumnMoney, Currency: currency}, "localSpend.amount"},
		reportColumn{growth.Column{Name: "avg_cpt", Type: growth.ColumnMoney, Currency: currency}, "cpt.amount"},
		reportColumn{growth.Column{Name: "ttr", Type: growth.ColumnNumber}, "ttr"})
}

// ReportAnswer is the report tool's answer: the report and what it covers.
type ReportAnswer struct {
	Account  string `json:"account"`
	Level    string `json:"level"`
	Promoted string `json:"promoted"`
	Start    string `json:"start"`
	End      string `json:"end"`
	growth.Report
}

// report reads one report of one ad account, in its own zone or UTC.
func (p *Product) report(ctx context.Context, call growth.Call) (any, error) {
	args := call.Args
	level, _ := args["level"].(string)
	promoted, _ := args["promoted"].(string)
	if promoted == "" {
		promoted = promotedApp
	}
	opID, ok := reportOps[promoted][level]
	if !ok {
		return nil, invalid("`level` is campaign, ad_group, keyword, search_term or ad, and `promoted` app or brand.")
	}
	campaign, _ := args["campaign"].(string)
	switch {
	case level != levelCampaign && !digits().MatchString(campaign):
		return nil, invalid("a %s report names its campaign's ID in `campaign`.", level)
	case campaign != "" && !digits().MatchString(campaign):
		return nil, invalid("%q is not a campaign ID: its digits.", campaign)
	}
	op, _ := Catalog().Operation(opID)
	readArgs := map[string]any{"operation": opID, "account": args["account"], "body": map[string]any{}}
	m, err := p.concrete(ctx, call.Handle, ToolRead, readArgs)
	if err != nil {
		return nil, err
	}
	info, err := p.accountOf(ctx, m.conn, m.account)
	if err != nil {
		return nil, said(err)
	}
	zoneName, apple := info.TimeZone, "ORTZ"
	switch tz, _ := args["time_zone"].(string); tz {
	case "", "account":
	case "utc":
		if level == levelSearchTerm {
			return nil, invalid("Apple reports search terms in the ad account's own zone alone: leave `time_zone` out.")
		}
		zoneName, apple = "UTC", "UTC"
	default:
		return nil, invalid("`time_zone` is account (the default) or utc.")
	}
	zone, err := time.LoadLocation(zoneName)
	if err != nil {
		return nil, fmt.Errorf("appleads: ad account %s's time zone %q does not load: %w", m.account, zoneName, err)
	}
	days, err := daysOf(args, p.deps.Now(), zone)
	if err != nil {
		return nil, err
	}
	limit := growth.RowLimit(intArg(args["limit"]))
	body := map[string]any{"timeRange": map[string]any{"start": days.Start, "end": days.End, "timeZone": apple},
		"pagination": map[string]any{"offset": 0, "pageSize": limit}}
	if campaign != "" {
		id, _ := strconv.ParseInt(campaign, 10, 64)
		body["filters"] = []any{map[string]any{"field": "campaignId", "operator": "EQUALS", "value": id}}
	}
	resp, err := m.conn.client.Do(ctx, Request{Method: http.MethodPost, Path: op.Path, Body: body, Account: m.account,
		Read: true})
	rec := recordOf(call, m, growth.ClassRead)
	if err != nil {
		rec.Failure = failureOf(err)
		p.note(ctx, growth.StepFailed, rec)
		return nil, said(err)
	}
	scoped, err := m.scope(resp.Body)
	if err != nil {
		return nil, said(err)
	}
	p.note(ctx, growth.StepMade, rec)
	report, err := reportOf(scoped, level, promoted, info.Currency, limit)
	if err != nil {
		return nil, err
	}
	today := p.deps.Now().In(zone)
	report.TimeZone = zoneName
	report.DataThrough = today.AddDate(0, 0, -1).Format(time.DateOnly)
	report.Estimated = days.End >= today.AddDate(0, 0, -restateDays).Format(time.DateOnly)
	if len(report.Rows) == 0 {
		report.EmptyReason = growth.EmptyNoData
		if days.Start >= today.Format(time.DateOnly) {
			report.EmptyReason = growth.EmptyNotReady
		}
	}
	return ReportAnswer{Account: m.account, Level: level, Promoted: promoted, Start: days.Start, End: days.End,
		Report: report}, nil
}

// daysOf are a report call's days: a named range cut in zone, or its own
// start and end.
func daysOf(args map[string]any, now time.Time, zone *time.Location) (growth.Days, error) {
	named, _ := args["range"].(string)
	start, _ := args["start"].(string)
	end, _ := args["end"].(string)
	switch {
	case named != "" && (start != "" || end != ""):
		return growth.Days{}, invalid("give `range` or `start` and `end`, not both.")
	case named != "":
		if !growth.Range(named).Valid() {
			return growth.Days{}, invalid("`range` is %s.", rangeNames())
		}
		return growth.Range(named).Resolve(now, zone)
	case start == "" && end == "":
		return growth.RangeLast7Days.Resolve(now, zone)
	case !dayShape().MatchString(start) || !dayShape().MatchString(end) || end < start:
		return growth.Days{}, invalid("`start` and `end` are days, YYYY-MM-DD, the end no earlier than the start.")
	}
	return growth.Days{Start: start, End: end, Zone: zone}, nil
}

func intArg(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	}
	return 0
}

// reportOf reads Apple's report rows into a growth report: ids and figures
// exact, names marked as somebody's words.
func reportOf(body []byte, level, promoted, currency string, limit int) (growth.Report, error) {
	var doc struct {
		Result struct {
			Rows []struct {
				Metadata     map[string]any `json:"metadata"`
				TotalMetrics map[string]any `json:"totalMetrics"`
			} `json:"rows"`
		} `json:"result"`
		Pagination struct {
			TotalCount *int64 `json:"totalCount"`
		} `json:"pagination"`
	}
	if err := decode(body, &doc); err != nil {
		return growth.Report{}, fmt.Errorf("appleads: read the report: %w", err)
	}
	entity, metrics := entityColumns[level], metricColumns(promoted, currency)
	r := growth.Report{Rows: [][]any{}, TotalRows: doc.Pagination.TotalCount}
	for _, c := range append(append([]reportColumn{}, entity...), metrics...) {
		r.Columns = append(r.Columns, c.Column)
	}
	for _, row := range doc.Result.Rows {
		if len(r.Rows) >= limit {
			break
		}
		cells := make([]any, 0, len(r.Columns))
		for _, c := range entity {
			cells = append(cells, cell(c.Type, lookup(row.Metadata, c.path)))
		}
		for _, c := range metrics {
			cells = append(cells, cell(c.Type, lookup(row.TotalMetrics, c.path)))
		}
		r.Rows = append(r.Rows, cells)
	}
	r.Truncated = len(doc.Result.Rows) > len(r.Rows) ||
		(r.TotalRows != nil && *r.TotalRows > int64(len(r.Rows)))
	return r, nil
}

// cell is one value as a report carries it: a number as Apple's digits, an
// ID, a name or an amount as text.
func cell(typ growth.ColumnType, v any) any {
	if v == nil {
		return nil
	}
	if n, ok := v.(json.Number); ok && typ == growth.ColumnNumber {
		return n
	}
	return valueText(v)
}
