package metaads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
)

// A write is classed against its target as read just before (package doc,
// "Classes"): its changes, each field's value before and after, and the
// states its operation's rules name, read from the request or the target. A
// state the engine could not read does not hold, the stricter reading.

// before is a target as it reads now: the fields the write's rules need and
// their digest, which an approval binds.
type before struct {
	attrs  map[string]any
	digest string
}

// beforeOf reads t's target: for an update of a node, the fields its
// operation's rules name that it changes, and its status; for every other
// request nothing, since what it creates is new and its path is bound.
func (p *Product) beforeOf(ctx context.Context, m made, t target) (before, error) {
	empty := before{attrs: map[string]any{}, digest: hexSum([]byte("{}"))}
	if len(t.row.reads) == 0 || t.node == "" {
		return empty, nil
	}
	fields := []string{"id"}
	for _, f := range t.row.reads {
		if _, set := t.params[f]; set || f == "status" || f == "execution_spec" || f == "is_published" {
			fields = append(fields, f)
		}
	}
	if spendBearing(t) {
		for _, f := range []string{"daily_budget", "lifetime_budget", "budget_remaining", "campaign_id", "stop_time",
			"end_time", "bid_amount", "bid_strategy"} {
			if slices.Contains(t.row.reads, f) || f == "budget_remaining" || f == "campaign_id" {
				if !slices.Contains(fields, f) && fieldOf(t.row.op.Resource, f) {
					fields = append(fields, f)
				}
			}
		}
	}
	attrs, err := p.readObject(ctx, m.conn, t.node, fields)
	if err != nil {
		return before{}, err
	}
	delete(attrs, "id")
	data, _ := json.Marshal(attrs)
	return before{attrs: attrs, digest: hexSum(data)}, nil
}

// spendFields are the fields the spend guard reads of a campaign or an ad set.
var spendFields = map[string][]string{
	"Campaign": {"status", "daily_budget", "lifetime_budget", "budget_remaining", "stop_time", "bid_strategy"},
	"AdSet":    {"status", "daily_budget", "lifetime_budget", "budget_remaining", "campaign_id", "end_time", "bid_amount", "bid_strategy"},
}

// fieldOf reports whether node type resource answers field, among those the
// spend guard reads.
func fieldOf(resource, field string) bool { return slices.Contains(spendFields[resource], field) }

// spendBearing reports whether t updates a campaign or an ad set, whose
// budget and status the guard weighs.
func spendBearing(t target) bool {
	return t.row.op.ID == opCampaignUpdate || t.row.op.ID == opAdSetUpdate
}

// changesOf are the fields t sets, each with its value before, times read as
// Unix seconds so a later start and an earlier end compare as numbers.
func changesOf(t target, b before) []growth.Change {
	names := make([]string, 0, len(t.params))
	for k := range t.params {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]growth.Change, 0, len(names))
	for _, n := range names {
		after := plain(t.params[n])
		bv := b.attrs[n]
		if tb, ok := timeOf(bv); ok {
			if ta, ok := timeOf(after); ok {
				bv, after = json.Number(fmt.Sprint(tb.Unix())), json.Number(fmt.Sprint(ta.Unix()))
			}
		}
		out = append(out, growth.Change{Field: n, Before: bv, After: after})
	}
	return out
}

// plain is a parameter's value as a rule reads it: JSON text decoded, so a
// list sent as text reads as the list.
func plain(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{") {
		var out any
		if decode([]byte(t), &out) == nil {
			return out
		}
	}
	return v
}

// timeOf reads a Meta time: RFC 3339, Meta's own offset form, or Unix seconds.
func timeOf(v any) (time.Time, bool) {
	s := strings.TrimSpace(fmt.Sprint(v))
	if v == nil || s == "" || s == "0" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-0700", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// pausingTypes are the automated rule actions that only pause or tell.
var pausingTypes = []string{"PAUSE", "NOTIFICATION", "PING_ENDPOINT"}

// dryRun reports whether t is Meta's dry run, which changes nothing.
func dryRun(t target) bool {
	opts := plain(t.params["execution_options"])
	list, _ := opts.([]any)
	if s, ok := opts.(string); ok {
		list = []any{s}
	}
	return slices.ContainsFunc(list, func(v any) bool { return v == "validate_only" }) &&
		slices.ContainsFunc(t.row.op.Fields, func(f growth.FieldRule) bool { return f.Field == "execution_options" })
}

// classOf is t's class against its target as read: a dry run is read, every
// other write its operation's rules over its changes in the states that hold.
func (p *Product) classOf(ctx context.Context, m made, t target, b before) growth.Class {
	if dryRun(t) {
		return growth.ClassRead
	}
	states := map[string]bool{}
	for _, s := range wantedStates(t.row.op) {
		states[s] = p.holds(ctx, m, t, b, s)
	}
	return growth.ClassifyIn(t.row.op, changesOf(t, b), func(s string) bool { return states[s] })
}

// wantedStates are every state op's rules name.
func wantedStates(op growth.Operation) []string {
	var out []string
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, s := range op.States {
		add(s.When)
	}
	for _, f := range op.Fields {
		for _, w := range f.While {
			add(w.When)
		}
	}
	return out
}

// holds reports whether t is in state, read from its request or its target.
func (p *Product) holds(ctx context.Context, m made, t target, b before, state string) bool {
	text := func(k string) string {
		if t.params[k] == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(plain(t.params[k])))
	}
	switch state {
	case statePaused:
		if specs, ok := plain(t.params["ad_specs"]).([]any); ok {
			for _, s := range specs {
				spec, _ := s.(map[string]any)
				if spec["status"] != "PAUSED" {
					return false
				}
			}
			return len(specs) > 0
		}
		if _, ok := t.params["status_option"]; ok {
			return text("status_option") == "PAUSED"
		}
		return text("status") == "PAUSED"
	case stateInactive:
		return b.attrs["status"] == "PAUSED"
	case stateUnused:
		return p.unused(ctx, m, t.node)
	case statePausesOnly:
		spec := plain(t.params["execution_spec"])
		if spec == nil {
			spec = b.attrs["execution_spec"]
		}
		es, _ := spec.(map[string]any)
		kind, _ := es["execution_type"].(string)
		return slices.Contains(pausingTypes, kind)
	case stateDraft:
		if t.row.op.Method == http.MethodPost && t.node != "" && len(t.row.reads) > 0 {
			return b.attrs["is_published"] == false
		}
		return t.params["is_published"] == false || text("is_published") == "false"
	case stateNew:
		return text("flow_id") == ""
	case stateTestEvents:
		return text("test_event_code") != ""
	case stateUnpublished:
		return t.params["published"] == false || text("published") == "false"
	}
	return false
}

// pausedEffective are the effective statuses of an ad that serves nothing,
// and will not serve without a change somebody makes.
var pausedEffective = []string{"PAUSED", "CAMPAIGN_PAUSED", "ADSET_PAUSED", "ARCHIVED", "DELETED"}

// maxAudiencePages bounds the ads read to decide an audience is unused: past
// five pages of 100 it counts as used.
const maxAudiencePages = 5

// unused reports whether no ad that could serve uses the custom audience and
// no lookalike is built on it; one the engine cannot read counts as used.
func (p *Product) unused(ctx context.Context, m made, audience string) bool {
	if audience == "" {
		return false
	}
	attrs, err := p.readObject(ctx, m.conn, audience, []string{"lookalike_audience_ids"})
	if err != nil {
		return false
	}
	if ids, _ := attrs["lookalike_audience_ids"].([]any); len(ids) > 0 {
		return false
	}
	tok, err := p.tokenFor(ctx, m.conn, nil, growth.ClassRead, []string{"ads_read"})
	if err != nil {
		return false
	}
	used := false
	err = pages(ctx, m.conn.client, tok.valueNow(), "/"+url.PathEscape(audience)+"/ads",
		url.Values{"fields": {"effective_status"}, "limit": {"100"}}, objectOf(m.account), true, maxAudiencePages,
		func(raw json.RawMessage) error {
			var ad struct {
				Status string `json:"effective_status"`
			}
			if json.Unmarshal(raw, &ad) != nil || !slices.Contains(pausedEffective, ad.Status) {
				used = true
			}
			return nil
		})
	return err == nil && !used
}
