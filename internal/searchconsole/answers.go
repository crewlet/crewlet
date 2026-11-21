package searchconsole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// pacific is Search Console's day: every date it takes and reports is
// Pacific time.
const pacific = "America/Los_Angeles"

// siteRow is one property the seat may reach, with its identity's
// permission on it.
type siteRow struct {
	Site       string `json:"site"`
	Permission string `json:"permission"`
}

// sitesAnswer is search_console_sites_list's answer.
type sitesAnswer struct {
	Sites []siteRow `json:"sites"`
	// Unreached are the seat's sites its identity is not added to.
	Unreached []string `json:"unreached,omitempty"`
}

// permissionOf is a permissionLevel as the catalog's role ID, whether Google
// writes the enum's name (SITE_OWNER) or its legacy JSON value (siteOwner).
func permissionOf(level string) string {
	var b strings.Builder
	for i, r := range level {
		if r >= 'A' && r <= 'Z' && i > 0 && !strings.Contains(level, "_") {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	id := strings.ToUpper(b.String())
	if slices.ContainsFunc(roles(), func(r growth.Role) bool { return r.ID == id }) {
		return id
	}
	return "unknown"
}

// listSites is every property the seat may reach, and the ones of its sites
// its identity is not added to yet.
func (p *Product) listSites(ctx context.Context, call growth.Call) (sitesAnswer, error) {
	if p.seat(call.Handle) == nil {
		return sitesAnswer{}, refusal(growth.RefusedForbidden, "this agent is off at Search Console.")
	}
	op, _ := p.Catalog().Operation("webmasters.sites.list")
	req, err := newRequest(nil, op, nil, nil, nil)
	if err != nil {
		return sitesAnswer{}, err
	}
	raw, err := p.client.Send(ctx, call.Handle, req)
	var list struct {
		Entries []struct {
			Site       string `json:"siteUrl"`
			Permission string `json:"permissionLevel"`
		} `json:"siteEntry"`
	}
	if err == nil {
		if derr := json.Unmarshal(raw, &list); derr != nil {
			err = &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome,
				Err: fmt.Errorf("searchconsole: decode the site list: %w", derr)}
		}
	}
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return sitesAnswer{}, err
	}
	out := sitesAnswer{Sites: []siteRow{}}
	sites := p.sites(call.Handle)
	for _, e := range list.Entries {
		if slices.Contains(sites, e.Site) {
			out.Sites = append(out.Sites, siteRow{Site: e.Site, Permission: permissionOf(e.Permission)})
		}
	}
	for _, s := range sites {
		if !slices.ContainsFunc(out.Sites, func(r siteRow) bool { return r.Site == s }) {
			out.Unreached = append(out.Unreached, s)
		}
	}
	return out, nil
}

// analyticsRow is one row of a performance report: its keys, which searchers
// typed and pages carry, and its figures.
type analyticsRow struct {
	Keys        string  `json:"keys" untrusted:"fence"`
	Clicks      float64 `json:"clicks"`
	Impressions float64 `json:"impressions"`
	CTR         float64 `json:"ctr"`
	Position    float64 `json:"position"`
}

// analyticsAnswer is search_console_search_analytics' answer.
type analyticsAnswer struct {
	Site       string         `json:"site"`
	StartDate  string         `json:"start_date"`
	EndDate    string         `json:"end_date"`
	Dimensions []string       `json:"dimensions"`
	Rows       []analyticsRow `json:"rows"`
	// Truncated is a full page: there may be more from start_row on.
	Truncated   bool   `json:"truncated"`
	TimeZone    string `json:"time_zone"`
	Aggregation string `json:"aggregation,omitempty"`
	// FirstIncompleteDate and FirstIncompleteHour are Google's first day
	// or hour whose figures may still change.
	FirstIncompleteDate string `json:"first_incomplete_date,omitempty"`
	FirstIncompleteHour string `json:"first_incomplete_hour,omitempty"`
	EmptyReason         string `json:"empty_reason,omitempty"`
}

// googleAnalytics is the part of a Search Analytics answer the answer reads.
type googleAnalytics struct {
	Rows []struct {
		Keys        []string `json:"keys"`
		Clicks      float64  `json:"clicks"`
		Impressions float64  `json:"impressions"`
		CTR         float64  `json:"ctr"`
		Position    float64  `json:"position"`
	} `json:"rows"`
	Aggregation string `json:"responseAggregationType"`
	Metadata    struct {
		FirstIncompleteDate string `json:"firstIncompleteDate"`
		FirstIncompleteHour string `json:"firstIncompleteHour"`
	} `json:"metadata"`
}

// day is a date as Search Console takes it.
var day = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`) })

// reportDays are a report's first and last day: the call's dates, or its
// named range (the last 28 days by default) cut in Pacific time at now.
func reportDays(args map[string]any, now time.Time) (string, string, error) {
	start, end := argString(args, "start_date"), argString(args, "end_date")
	if start != "" || end != "" {
		if !day().MatchString(start) || !day().MatchString(end) {
			return "", "", refusal(growth.RefusedInvalid, "give start_date and end_date both, as YYYY-MM-DD, "+
				"or a range. No vendor was reached.")
		}
		return start, end, nil
	}
	r := growth.Range(argString(args, "range"))
	if r == "" {
		r = growth.RangeLast28Days
	}
	if !r.Valid() {
		return "", "", refusal(growth.RefusedInvalid, fmt.Sprintf("%q is not a range; give %s. No vendor was "+
			"reached.", r, words.Or(rangeNames())))
	}
	zone, err := time.LoadLocation(pacific)
	if err != nil {
		return "", "", fmt.Errorf("searchconsole: load Search Console's zone: %w", err)
	}
	days, err := r.Resolve(now, zone)
	if err != nil {
		return "", "", err
	}
	return days.Start, days.End, nil
}

// searchAnalytics is search_console_search_analytics.
func (p *Product) searchAnalytics(ctx context.Context, call growth.Call) (analyticsAnswer, error) {
	start, end, err := reportDays(call.Args, time.Now())
	if err != nil {
		return analyticsAnswer{}, err
	}
	limit := growth.RowLimit(argInt(call.Args, "limit"))
	dims := stringArgs(call.Args["dimensions"])
	body := map[string]any{"startDate": start, "endDate": end, "dimensions": dims, "rowLimit": limit,
		"startRow": argInt(call.Args, "start_row")}
	for arg, field := range map[string]string{"type": "type", "data_state": "dataState",
		"aggregation_type": "aggregationType", "dimension_filter_groups": "dimensionFilterGroups"} {
		if v, ok := call.Args[arg]; ok {
			body[field] = v
		}
	}
	op, _ := p.Catalog().Operation("webmasters.searchanalytics.query")
	site := argString(call.Args, "site")
	req, err := newRequest(p.sites(call.Handle), op, map[string]string{"siteUrl": site}, nil, body)
	if err != nil {
		return analyticsAnswer{}, err
	}
	if err = p.inScope(call.Handle, req); err != nil {
		return analyticsAnswer{}, err
	}
	raw, err := p.client.Send(ctx, call.Handle, req)
	var r googleAnalytics
	if err == nil {
		if derr := json.Unmarshal(raw, &r); derr != nil {
			err = &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome,
				Err: fmt.Errorf("searchconsole: decode the performance report: %w", derr)}
		}
	}
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return analyticsAnswer{}, err
	}
	out := analyticsAnswer{Site: site, StartDate: start, EndDate: end, Dimensions: dims, Rows: []analyticsRow{},
		TimeZone: pacific, Aggregation: enumOrEmpty(r.Aggregation),
		FirstIncompleteDate: matching(day(), r.Metadata.FirstIncompleteDate),
		FirstIncompleteHour: matching(hour(), r.Metadata.FirstIncompleteHour), Truncated: len(r.Rows) >= limit}
	for _, row := range r.Rows {
		keys, _ := json.Marshal(row.Keys)
		out.Rows = append(out.Rows, analyticsRow{Keys: string(keys), Clicks: row.Clicks,
			Impressions: row.Impressions, CTR: row.CTR, Position: row.Position})
	}
	if len(out.Rows) == 0 {
		out.EmptyReason = string(growth.EmptyNoData)
	}
	return out, nil
}

// inspectionAnswer is search_console_inspect_url's answer: Google's verdict, and its
// whole result, which quotes the page's own URLs, names and issues.
type inspectionAnswer struct {
	Site    string `json:"site"`
	URL     string `json:"url"`
	Verdict string `json:"verdict,omitempty"`
	Result  string `json:"result" untrusted:"fence"`
}

// inspect is search_console_inspect_url.
func (p *Product) inspect(ctx context.Context, call growth.Call) (inspectionAnswer, error) {
	site, page := argString(call.Args, "site"), argString(call.Args, "url")
	body := map[string]any{"siteUrl": site, "inspectionUrl": page}
	if lang := argString(call.Args, "language"); lang != "" {
		body["languageCode"] = lang
	}
	op, _ := p.Catalog().Operation("searchconsole.urlInspection.index.inspect")
	req, err := newRequest(p.sites(call.Handle), op, nil, nil, body)
	if err != nil {
		return inspectionAnswer{}, err
	}
	if err = p.inScope(call.Handle, req); err != nil {
		return inspectionAnswer{}, err
	}
	answer, err := p.client.SendAndRead(ctx, call.Handle, req, nil)
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return inspectionAnswer{}, err
	}
	var r struct {
		Result struct {
			Index struct {
				Verdict string `json:"verdict"`
			} `json:"indexStatusResult"`
		} `json:"inspectionResult"`
	}
	_ = json.Unmarshal([]byte(answer.Answer), &r)
	return inspectionAnswer{Site: site, URL: page, Verdict: enumOrEmpty(r.Result.Index.Verdict),
		Result: answer.Answer}, nil
}

// tokenAnswer is search_console_verification_token's answer: Google's token, shown
// whole since the agent places it, and where it goes.
type tokenAnswer struct {
	Site   string `json:"site"`
	Method string `json:"method"`
	Token  string `json:"token"`
	Place  string `json:"place"`
}

// places says where each method's token goes.
var places = map[string]string{
	"META":        "Add the token, a meta tag, to the head of the site's home page.",
	"FILE":        "Serve a file of this name, holding the line google-site-verification: and its name, at the site's root.",
	"ANALYTICS":   "The site's home page must carry the Google Analytics tag of a property this identity can edit; the token is not placed.",
	"TAG_MANAGER": "The site's home page must carry the Google Tag Manager container this identity publishes to; the token is not placed.",
	"DNS_TXT":     "Add the token as a TXT record on the domain.",
	"DNS_CNAME":   "Add the token's name and target as a CNAME record on the domain.",
	"DNS":         "Add the token as a TXT record on the domain.",
}

// verificationToken is search_console_verification_token.
func (p *Product) verificationToken(ctx context.Context, call growth.Call) (tokenAnswer, error) {
	site, method := argString(call.Args, "site"), argString(call.Args, "method")
	req, err := p.verifyRequest("siteVerification.webResource.getToken", call.Handle, site, method)
	if err != nil {
		return tokenAnswer{}, err
	}
	if err = p.inScope(call.Handle, req); err != nil {
		return tokenAnswer{}, err
	}
	raw, err := p.client.Send(ctx, call.Handle, req)
	var r struct {
		Token string `json:"token"`
	}
	if err == nil {
		derr := json.Unmarshal(raw, &r)
		if derr == nil && r.Token == "" {
			derr = errors.New("it holds no token")
		}
		if derr != nil {
			err = &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome,
				Err: fmt.Errorf("searchconsole: read Google's token answer: %w", derr)}
		}
	}
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return tokenAnswer{}, err
	}
	return tokenAnswer{Site: site, Method: method, Token: r.Token, Place: places[method] +
		" Then ask to verify with search_console_verify_site."}, nil
}

// verifyRequest is a getToken or insert request for site by method, both
// checked against each other first.
func (p *Product) verifyRequest(id, handle, site, method string) (googlegrowth.Request, error) {
	kind, identifier, _ := resourceOf(site)
	if !methodFits(kind, method) {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%s cannot prove %s: a "+
			"domain is verified by DNS_TXT, DNS_CNAME or DNS, a URL prefix by META, FILE, ANALYTICS or "+
			"TAG_MANAGER. No vendor was reached.", method, site))
	}
	op, _ := p.Catalog().Operation(id)
	body := map[string]any{"site": map[string]any{"type": kind, "identifier": identifier}}
	var query map[string]string
	if id == "siteVerification.webResource.getToken" {
		body["verificationMethod"] = method
	} else {
		query = map[string]string{"verificationMethod": method}
	}
	return newRequest(p.sites(handle), op, nil, query, body)
}

// enum is a value Google answers from a closed set (PASS, byProperty):
// letters and underscores, nothing a sentence could hide in.
var enum = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z_]{1,64}$`) })

// hour is an hour as Search Console reports it: an ISO 8601 time in Pacific time.
var hour = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?(Z|[+-]\d{2}:\d{2})?$`)
})

// matching is s where pattern matches it, "" otherwise.
func matching(pattern *regexp.Regexp, s string) string {
	if pattern.MatchString(s) {
		return s
	}
	return ""
}

// enumOrEmpty is an enum Google answers with, "" for anything else.
func enumOrEmpty(s string) string {
	if enum().MatchString(s) {
		return s
	}
	return ""
}

// argString is a string argument, trimmed, "" where absent.
func argString(args map[string]any, name string) string {
	s, _ := args[name].(string)
	return strings.TrimSpace(s)
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

// stringArgs is an argument list of strings, without blanks.
func stringArgs(v any) []string {
	list, _ := v.([]any)
	out := []string{}
	for _, e := range list {
		if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
