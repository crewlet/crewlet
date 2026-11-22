package googleplay

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// metricSetOps are the vitals metric sets an agent queries, each by the
// method that queries it.
var metricSetOps = map[string]string{
	"anrRate":                     "playdeveloperreporting.vitals.anrrate.query",
	"crashRate":                   "playdeveloperreporting.vitals.crashrate.query",
	"errorCount":                  "playdeveloperreporting.vitals.errors.counts.query",
	"excessiveWakeupRate":         "playdeveloperreporting.vitals.excessivewakeuprate.query",
	"stuckBackgroundWakelockRate": "playdeveloperreporting.vitals.stuckbackgroundwakelockrate.query",
	"slowStartRate":               "playdeveloperreporting.vitals.slowstartrate.query",
	"slowRenderingRate":           "playdeveloperreporting.vitals.slowrenderingrate.query",
	"lmkRate":                     "playdeveloperreporting.vitals.lmkrate.query",
	"anonRssAndSwapMemoryUsage":   "playdeveloperreporting.vitals.anonrssandswapmemoryusage.query",
	"bitmapMemoryUsage":           "playdeveloperreporting.vitals.bitmapmemoryusage.query",
}

// metricSets are the metric sets' names, sorted.
func metricSets() []string {
	out := make([]string, 0, len(metricSetOps))
	for k := range metricSetOps {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// vitalsQueries are the methods the vitals tool may call.
func vitalsQueries([]growth.Operation) []string {
	out := make([]string, 0, len(metricSetOps))
	for _, set := range metricSets() {
		out = append(out, metricSetOps[set])
	}
	return out
}

// hourly are the metric sets Google aggregates by the hour too.
var hourly = []string{"anrRate", "crashRate", "errorCount"}

// Vitals is the vitals tool's answer: Google's rows as it answers them, with
// the zone their days are in.
type Vitals struct {
	App           string `json:"app"`
	MetricSet     string `json:"metric_set"`
	TimeZone      string `json:"time_zone"`
	Rows          string `json:"rows" untrusted:"fence"`
	NextPageToken string `json:"next_page_token,omitempty"`
}

// queryVitals queries one metric set of an app over a range of days.
func (p *Product) queryVitals(ctx context.Context, call growth.Call) (any, error) {
	app, _ := call.Args["app"].(string)
	set, _ := call.Args["metric_set"].(string)
	id, ok := metricSetOps[set]
	if !ok {
		return nil, refusal(growth.RefusedInvalid, "metric_set is one of "+words.Or(metricSets())+
			". No vendor was reached.")
	}
	period, _ := call.Args["aggregation"].(string)
	if period == "" {
		period = "DAILY"
	}
	zone := "America/Los_Angeles"
	if period == "HOURLY" {
		if !slices.Contains(hourly, set) {
			return nil, refusal(growth.RefusedInvalid, "HOURLY is offered for anrRate, crashRate and errorCount "+
				"alone. No vendor was reached.")
		}
		zone = "UTC"
	}
	now := p.deps.Now().UTC()
	start, err := dayArg(call.Args["start_date"], now.AddDate(0, 0, -28))
	if err != nil {
		return nil, err
	}
	end, err := dayArg(call.Args["end_date"], now.AddDate(0, 0, -1))
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"timelineSpec": map[string]any{"aggregationPeriod": period, "startTime": civil(start, zone),
			"endTime": civil(end, zone)},
		"metrics":    stringList(call.Args["metrics"]),
		"dimensions": stringList(call.Args["dimensions"]),
		"pageSize":   200,
	}
	if f, _ := call.Args["filter"].(string); f != "" {
		body["filter"] = f
	}
	if n, ok := call.Args["page_size"].(float64); ok && n >= 1 && n <= growth.ReportMaxRows {
		body["pageSize"] = int(n)
	}
	if t, _ := call.Args["page_token"].(string); t != "" {
		body["pageToken"] = t
	}
	op, _ := p.Catalog().Operation(id)
	req, err := newRequest(op, map[string]string{"appsId": app}, nil, body)
	if err != nil {
		return nil, err
	}
	if err = p.inScope(call.Handle, req); err != nil {
		return nil, err
	}
	raw, err := p.client.Send(ctx, call.Handle, req)
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Rows []any  `json:"rows"`
		Next string `json:"nextPageToken"`
	}
	if err = decode(raw, set+" for "+app, &doc); err != nil {
		return nil, err
	}
	out, err := p.client.AnswerOf(req, raw, func(v any) any {
		m, _ := v.(map[string]any)
		return m["rows"]
	})
	if err != nil {
		return nil, err
	}
	return Vitals{App: app, MetricSet: set, TimeZone: zone, Rows: out.Answer, NextPageToken: doc.Next}, nil
}

// dayArg is a YYYY-MM-DD argument, def where it is absent.
func dayArg(v any, def time.Time) (time.Time, error) {
	s, _ := v.(string)
	if s == "" {
		return def, nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%q is not a day as YYYY-MM-DD. No "+
			"vendor was reached.", s))
	}
	return t, nil
}

// civil is a day as the Reporting API takes it, in zone.
func civil(t time.Time, zone string) map[string]any {
	return map[string]any{"year": t.Year(), "month": int(t.Month()), "day": t.Day(),
		"timeZone": map[string]any{"id": zone}}
}
