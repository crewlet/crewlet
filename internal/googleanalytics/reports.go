package googleanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// propertyRow is one property the seat may reach.
type propertyRow struct {
	Account     string `json:"account"`
	AccountName string `json:"account_name" untrusted:"name"`
	Property    string `json:"property"`
	Name        string `json:"property_name" untrusted:"name"`
}

// propertiesAnswer is google_analytics_list_properties' answer.
type propertiesAnswer struct {
	Properties []propertyRow `json:"properties"`
	// Unreached are the seat's properties its identity is not added to.
	Unreached []string `json:"unreached,omitempty"`
}

// listProperties is every property the seat may reach, and the ones of its
// scope its identity is not added to yet.
func (p *Product) listProperties(ctx context.Context, call growth.Call) (propertiesAnswer, error) {
	b := p.seat(call.Handle)
	if b == nil {
		return propertiesAnswer{}, refusal(growth.RefusedForbidden, "this agent is off at Google Analytics.")
	}
	all, err := p.summaries(ctx, call.Handle)
	if op, ok := p.Catalog().Operation(summariesOp); ok {
		req, _ := newRequest(op, nil, nil, nil)
		p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	}
	if err != nil {
		return propertiesAnswer{}, err
	}
	var out propertiesAnswer
	reached := map[string]bool{}
	for _, s := range all {
		for _, prop := range s.Properties {
			id := strings.TrimPrefix(prop.Property, "properties/")
			if !slices.Contains(b.Properties, id) {
				continue
			}
			reached[id] = true
			out.Properties = append(out.Properties, propertyRow{Account: strings.TrimPrefix(s.Account, "accounts/"),
				AccountName: s.DisplayName, Property: id, Name: prop.DisplayName})
		}
	}
	for _, id := range b.Properties {
		if !reached[id] {
			out.Unreached = append(out.Unreached, id)
		}
	}
	return out, nil
}

// reportRow is one row of a report: its dimension values, which the public
// and the site's tags write, and its metric values.
type reportRow struct {
	Dimensions string   `json:"dimensions" untrusted:"fence"`
	Metrics    []string `json:"metrics"`
}

// reportAnswer is a report's answer, with Google's quality flags passed on.
type reportAnswer struct {
	Property   string      `json:"property"`
	Dimensions []string    `json:"dimensions"`
	Metrics    []string    `json:"metrics"`
	Rows       []reportRow `json:"rows"`
	RowCount   int64       `json:"row_count"`
	Truncated  bool        `json:"truncated"`
	Currency   string      `json:"currency,omitempty"`
	TimeZone   string      `json:"time_zone,omitempty"`
	// Thresholded, Sampled and OtherRow are Google's flags that figures are
	// hidden, estimated or folded into "(other)".
	Thresholded bool   `json:"thresholded,omitempty"`
	Sampled     bool   `json:"sampled,omitempty"`
	OtherRow    bool   `json:"other_row,omitempty"`
	EmptyReason string `json:"empty_reason,omitempty"`
	// Quota is what Google says is left of the property's tokens.
	Quota json.RawMessage `json:"quota,omitempty"`
}

// googleReport is the part of a Data API report the answer reads.
type googleReport struct {
	DimensionHeaders []struct {
		Name string `json:"name"`
	} `json:"dimensionHeaders"`
	MetricHeaders []struct {
		Name string `json:"name"`
	} `json:"metricHeaders"`
	Rows []struct {
		DimensionValues []struct {
			Value string `json:"value"`
		} `json:"dimensionValues"`
		MetricValues []struct {
			Value string `json:"value"`
		} `json:"metricValues"`
	} `json:"rows"`
	RowCount int64 `json:"rowCount"`
	Metadata struct {
		CurrencyCode          string `json:"currencyCode"`
		TimeZone              string `json:"timeZone"`
		SubjectToThresholding bool   `json:"subjectToThresholding"`
		DataLossFromOtherRow  bool   `json:"dataLossFromOtherRow"`
		EmptyReason           string `json:"emptyReason"`
		SamplingMetadatas     []any  `json:"samplingMetadatas"`
	} `json:"metadata"`
	PropertyQuota json.RawMessage `json:"propertyQuota"`
}

// runReport is google_analytics_run_report.
func (p *Product) runReport(ctx context.Context, call growth.Call) (reportAnswer, error) {
	start, end := argString(call.Args, "start_date", "28daysAgo"), argString(call.Args, "end_date", "yesterday")
	body := map[string]any{
		"dateRanges":          []any{map[string]any{"startDate": start, "endDate": end}},
		"dimensions":          names(call.Args["dimensions"]),
		"metrics":             names(call.Args["metrics"]),
		"limit":               growth.RowLimit(argInt(call.Args, "limit")),
		"offset":              argInt(call.Args, "offset"),
		"returnPropertyQuota": true,
	}
	for arg, field := range map[string]string{"dimension_filter": "dimensionFilter", "metric_filter": "metricFilter",
		"order_bys": "orderBys"} {
		if v, ok := call.Args[arg]; ok {
			body[field] = v
		}
	}
	return p.report(ctx, call, "analyticsdata.properties.runReport", body)
}

// realtime is google_analytics_run_realtime_report.
func (p *Product) realtime(ctx context.Context, call growth.Call) (reportAnswer, error) {
	body := map[string]any{
		"dimensions": names(call.Args["dimensions"]), "metrics": names(call.Args["metrics"]),
		"limit": growth.RowLimit(argInt(call.Args, "limit")), "returnPropertyQuota": true,
	}
	return p.report(ctx, call, "analyticsdata.properties.runRealtimeReport", body)
}

// report runs a Data API report and reads its answer.
func (p *Product) report(ctx context.Context, call growth.Call, id string, body map[string]any) (reportAnswer, error) {
	op, _ := p.Catalog().Operation(id)
	property, _ := call.Args["property"].(string)
	req, err := newRequest(op, map[string]string{"propertiesId": property}, nil, body)
	if err != nil {
		return reportAnswer{}, err
	}
	if err = p.inScope(ctx, call.Handle, req); err != nil {
		return reportAnswer{}, err
	}
	raw, err := p.client.Send(ctx, call.Handle, req)
	var r googleReport
	if err == nil {
		if derr := json.Unmarshal(raw, &r); derr != nil {
			err = &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome, Err: fmt.Errorf("googleanalytics: decode %s: %w", id, derr)}
		}
	}
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return reportAnswer{}, err
	}
	out := reportAnswer{Property: property, RowCount: r.RowCount, Currency: r.Metadata.CurrencyCode,
		TimeZone: r.Metadata.TimeZone, Thresholded: r.Metadata.SubjectToThresholding,
		Sampled: len(r.Metadata.SamplingMetadatas) > 0, OtherRow: r.Metadata.DataLossFromOtherRow,
		EmptyReason: r.Metadata.EmptyReason, Quota: r.PropertyQuota, Rows: []reportRow{}}
	for _, h := range r.DimensionHeaders {
		out.Dimensions = append(out.Dimensions, h.Name)
	}
	for _, h := range r.MetricHeaders {
		out.Metrics = append(out.Metrics, h.Name)
	}
	for _, row := range r.Rows {
		var dims []string
		for _, d := range row.DimensionValues {
			dims = append(dims, d.Value)
		}
		var mets []string
		for _, m := range row.MetricValues {
			mets = append(mets, m.Value)
		}
		encoded, _ := json.Marshal(dims)
		out.Rows = append(out.Rows, reportRow{Dimensions: string(encoded), Metrics: mets})
	}
	out.Truncated = out.RowCount > int64(argInt(call.Args, "offset")+len(out.Rows))
	if len(out.Rows) == 0 && out.EmptyReason == "" {
		out.EmptyReason = string(growth.EmptyNoData)
	}
	return out, nil
}

// names is a list of names as the Data API's objects.
func names(v any) []any {
	list, _ := v.([]any)
	out := make([]any, 0, len(list))
	for _, n := range list {
		if s, ok := n.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, map[string]any{"name": s})
		}
	}
	return out
}

// argString is a string argument, def where absent.
func argString(args map[string]any, name, def string) string {
	if s, ok := args[name].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return def
}

// argInt is an integer argument, 0 where absent.
func argInt(args map[string]any, name string) int {
	switch v := args[name].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}
