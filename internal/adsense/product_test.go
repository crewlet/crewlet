package adsense_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/adsense"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
)

// A CALL NAMING ANOTHER ACCOUNT NEVER REACHES GOOGLE: one grant serves every
// seat, so the engine alone holds a seat to its accounts, for a listing, a
// change, a path's ad client, a body's resource name and the Platform API's
// platform alike.
//
// Mutation: make inScope or namesItsOwn return nil, and Google is called.
func TestACallNamingAnotherAccountNeverReachesGoogle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "administer")
	ctx := context.Background()
	if _, err := h.p.Run(ctx, h.call("adsense_sites_list", map[string]any{"account": theirs})); !refusedAs(err, growth.RefusedForbidden) {
		t.Errorf("a listing on another account answered %v, want forbidden", err)
	}
	for name, tc := range map[string]struct {
		tool string
		args map[string]any
	}{
		"a channel on another account": {"adsense_custom_channel_upsert", map[string]any{"account": theirs,
			"ad_client": "ca-" + theirs, "display_name": "Spring"}},
		"another account's ad client on the seat's account": {"adsense_ad_unit_upsert", map[string]any{
			"account": mine, "ad_client": "ca-" + theirs, "display_name": "Header", "size": "728x90"}},
		"a body naming another account's unit": {"adsense_operate", generic("adsense.accounts.adclients.adunits.create",
			map[string]any{"accountsId": mine, "adclientsId": client}, nil, map[string]any{"displayName": "x",
				"name": "accounts/" + theirs + "/adclients/ca-" + theirs + "/adunits/7"})},
		"another platform's sub-account": {"adsense_admin", generic("adsenseplatform.platforms.accounts.close",
			map[string]any{"platformsId": theirs, "accountsId": child}, nil, nil)},
		"another account's platform": {"adsense_operate", generic("adsenseplatform.accounts.platforms.groups.patch",
			map[string]any{"accountsId": theirs, "platformsId": "p1", "groupsId": "g1"},
			map[string]any{"updateMask": "description"}, map[string]any{"description": "Partners"})},
	} {
		if _, err := h.p.Prepare(ctx, h.call(tc.tool, tc.args)); !refusedAs(err, growth.RefusedForbidden) {
			t.Errorf("%s answered %v, want forbidden", name, err)
		}
	}
	if got := h.calls(); len(got) != 0 {
		t.Errorf("Google was called for a refused call: %v", got)
	}
	// A platform's own sub-account is the platform's to act on.
	if _, err := h.p.Prepare(ctx, h.call("adsense_publish", generic(
		"adsenseplatform.platforms.accounts.sites.requestReview",
		map[string]any{"platformsId": mine, "accountsId": child, "sitesId": "s1"}, nil, nil))); err != nil {
		t.Errorf("a review of the seat's platform's sub-account's site was refused: %v", err)
	}
}

// A SEAT THAT IS OFF REACHES NOTHING: its block turned off, every call is
// refused before Google, its own account's too.
//
// Mutation: drop inScope's seat check, and Google is called.
func TestASeatThatIsOffReachesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "operate")
	off := false
	h.company.Roles[0].Integrations.AdSense.Enabled = &off
	if _, err := h.p.Run(context.Background(), h.call("adsense_sites_list", map[string]any{"account": mine})); !refusedAs(err, growth.RefusedForbidden) {
		t.Errorf("a listing by a seat that is off answered %v, want forbidden", err)
	}
	if _, err := h.p.Run(context.Background(), h.call("adsense_read", generic("adsense.accounts.list",
		map[string]any{}, nil, nil))); !refusedAs(err, growth.RefusedForbidden) {
		t.Errorf("the account listing by a seat that is off answered %v, want forbidden", err)
	}
	if _, err := h.p.Prepare(context.Background(), h.call("adsense_operate", generic(adsenseUnitPatch, unitPath(),
		map[string]any{"updateMask": "displayName"}, map[string]any{"displayName": "x"}))); !refusedAs(err, growth.RefusedForbidden) {
		t.Errorf("a change by a seat that is off answered %v, want forbidden", err)
	}
	if got := h.calls(); len(got) != 0 {
		t.Errorf("Google was called: %v", got)
	}
}

// THE ACCOUNT LISTING IS CUT TO THE SEAT'S ACCOUNTS: Google lists what the
// signing person reaches, and an agent sees only its own.
func TestTheAccountListingIsCutToTheSeatsAccounts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "read")
	out, err := h.p.Run(context.Background(), h.call("adsense_read", map[string]any{"operation": "adsense.accounts.list"}))
	if err != nil {
		t.Fatal(err)
	}
	answer := out.(googlegrowth.Answer).Answer
	if !strings.Contains(answer, mine) || strings.Contains(answer, theirs) {
		t.Errorf("the listing answered %s, want %s alone", answer, mine)
	}
}

// EVERY CALL GOES AS THE COMPANY'S GRANT, RENEWED ONCE ON A 401: no seat
// identity is minted, and a token Google refuses is renewed once and the call
// made again with the new one.
func TestEveryCallGoesAsTheGrantRenewedOnceOnRefusal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "read")
	h.vend.unauthFirst = true
	if _, err := h.p.Run(context.Background(), h.call("adsense_sites_list", map[string]any{"account": mine})); err != nil {
		t.Fatalf("a call after one refused token failed: %v", err)
	}
	if h.grant.renewed != 1 || !slices.Equal(h.vend.bearers, []string{"ya29.first", "ya29.renewed"}) {
		t.Errorf("renewed %d times, Google was sent %v; want the first token then the renewed one",
			h.grant.renewed, h.vend.bearers)
	}
}

// A CHANGE IS CLASSED BY WHAT IT CHANGES ON ITS TARGET AS READ NOW: an ad
// unit's name or state is operate, a size or type that differs is public in
// any spelling of its mask, the same size is no change, a review request is
// public, and deleting a channel or moving a platform site between revenue
// groups is admin.
//
// Mutation: drop the size and type rule, or class a change under it by its
// own path, and a public change classes as operate.
func TestAChangeIsClassedByWhatItChangesOnItsTarget(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "administer")
	patch := func(mask string, body map[string]any) map[string]any {
		return generic("adsense.accounts.adclients.adunits.patch", unitPath(),
			map[string]any{"updateMask": mask}, body)
	}
	settings := func(s map[string]any) map[string]any { return map[string]any{"contentAdsSettings": s} }
	for _, tc := range []struct {
		name string
		args map[string]any
		want growth.Class
	}{
		{"a rename", patch("displayName", map[string]any{"displayName": "Footer"}), growth.ClassOperate},
		{"archived", patch("state", map[string]any{"state": "ARCHIVED"}), growth.ClassOperate},
		{"a new size", patch("contentAdsSettings.size", settings(map[string]any{"size": "300x250"})),
			growth.ClassPublic},
		{"a new size, snake_case", patch("content_ads_settings.size", settings(map[string]any{"size": "300x250"})),
			growth.ClassPublic},
		{"the same size", patch("contentAdsSettings.size", settings(map[string]any{"size": "728x90"})),
			growth.ClassOperate},
		{"the settings whole", patch("contentAdsSettings", settings(map[string]any{"size": "728x90",
			"type": "FEED"})), growth.ClassPublic},
		{"the settings cleared", patch("contentAdsSettings", map[string]any{}), growth.ClassPublic},
		{"a new unit", generic("adsense.accounts.adclients.adunits.create", map[string]any{"accountsId": mine,
			"adclientsId": client}, nil, map[string]any{"displayName": "x", "state": "ACTIVE",
			"contentAdsSettings": map[string]any{"size": "1x3", "type": "DISPLAY"}}), growth.ClassOperate},
		{"a review asked", generic("adsenseplatform.platforms.accounts.sites.requestReview",
			map[string]any{"platformsId": mine, "accountsId": child, "sitesId": "s1"}, nil, nil), growth.ClassPublic},
		{"a channel deleted", generic("adsense.accounts.adclients.customchannels.delete", channelPath(), nil, nil),
			growth.ClassAdmin},
	} {
		if got := classOf(t, h, toolFor(t, tc.args["operation"].(string)), tc.args); got != tc.want {
			t.Errorf("%s classes as %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := classOf(t, h, "adsense_ad_unit_upsert", map[string]any{"account": mine, "ad_client": client,
		"ad_unit": "7", "size": "300x250"}); got != growth.ClassPublic {
		t.Errorf("a size changed through the operate tool classes as %s, want public for its tool to refuse", got)
	}
}

// A PATCH OR A CALL THE ENGINE CANNOT READ AS GOOGLE WOULD NEVER REACHES
// GOOGLE: no mask (Google then replaces the whole unit), a path Google does
// not document, a body field spelled as its proto name, the mask passed as
// update_mask, a path parameter the method does not take, a resource name of
// another unit, and a site moved to a group of another platform; each could
// change a live unit's size under an operate class.
//
// Mutation: drop the mask, proto-name, query-key, stray-parameter or name
// refusal, and Google is called.
func TestACallTheEngineCannotReadNeverReachesGoogle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "administer")
	size := map[string]any{"contentAdsSettings": map[string]any{"size": "300x250"}}
	for name, args := range map[string]map[string]any{
		"no mask": generic(adsenseUnitPatch, unitPath(), nil, size),
		"a mask Google does not document": generic(adsenseUnitPatch, unitPath(),
			map[string]any{"updateMask": "reportingDimensionId"}, size),
		"every field": generic(adsenseUnitPatch, unitPath(), map[string]any{"updateMask": "*"}, size),
		"a body field by its proto name": generic(adsenseUnitPatch, unitPath(),
			map[string]any{"updateMask": "displayName"},
			map[string]any{"displayName": "x", "content_ads_settings": map[string]any{"size": "300x250"}}),
		"the mask given twice, its second value public": generic(adsenseUnitPatch, unitPath(),
			map[string]any{"updateMask": []any{"displayName", "contentAdsSettings.size"}},
			map[string]any{"displayName": "x", "contentAdsSettings": map[string]any{"size": "300x250"}}),
		"a platform sub-account that is no publisher ID": generic("adsenseplatform.platforms.accounts.close",
			map[string]any{"platformsId": mine, "accountsId": "c1"}, nil, nil),
		"the mask as update_mask": generic(adsenseUnitPatch, unitPath(),
			map[string]any{"updateMask": "displayName", "update_mask": "contentAdsSettings"}, size),
		"a stray path parameter": generic(adsenseUnitPatch, map[string]any{"accountsId": mine, "adclientsId": client,
			"adunitsId": "7", "customchannelsId": "9"}, map[string]any{"updateMask": "displayName"},
			map[string]any{"displayName": "x"}),
		"another unit's name": generic(adsenseUnitPatch, unitPath(), map[string]any{"updateMask": "displayName"},
			map[string]any{"displayName": "x", "name": "accounts/" + mine + "/adclients/" + client + "/adunits/8"}),
		"a site moved to another platform's group": generic(
			"adsenseplatform.accounts.platforms.childAccounts.sites.patch",
			map[string]any{"accountsId": mine, "platformsId": "p1", "childAccountsId": child, "sitesId": "s1"},
			map[string]any{"updateMask": "platformGroup"},
			map[string]any{"platformGroup": "accounts/" + mine + "/platforms/p2/groups/g2"}),
		"an account that is no publisher ID": generic("adsense.accounts.adclients.customchannels.delete",
			map[string]any{"accountsId": "accounts", "adclientsId": client, "customchannelsId": "9"}, nil, nil),
	} {
		tool := toolFor(t, args["operation"].(string))
		if _, err := h.p.Prepare(context.Background(), h.call(tool, args)); !refusedAs(err, growth.RefusedInvalid) ||
			strings.Contains(err.Error(), "does not make") {
			t.Errorf("%s answered %v, want invalid", name, err)
		}
	}
	if _, err := h.p.Run(context.Background(), h.call("adsense_read", generic("adsense.accounts.sites.list",
		map[string]any{"accountsId": mine}, map[string]any{"access_token": "x"}, nil))); !refusedAs(err, growth.RefusedInvalid) {
		t.Errorf("a listing with a query key it does not take answered %v, want invalid", err)
	}
	if got := h.calls(); len(got) != 0 {
		t.Errorf("Google was called: %v", got)
	}
}

const adsenseUnitPatch = "adsense.accounts.adclients.adunits.patch"

// A HELD CHANGE WHOSE TARGET MOVED SENDS NOTHING: a channel's deletion binds
// to the channel as a person saw it, and once the channel changes the
// approval is void, nothing is sent, and the access log says so.
//
// Mutation: make Apply skip BindingHolds, or read no target, and the delete
// is sent.
func TestAHeldChangeWhoseTargetMovedSendsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "administer")
	ctx := context.Background()
	call := h.call("adsense_admin", generic("adsense.accounts.adclients.customchannels.delete", channelPath(), nil, nil))
	hold, err := h.p.Prepare(ctx, call)
	if err != nil || hold.Class != growth.ClassAdmin {
		t.Fatalf("the deletion prepared as %v, %v; want admin", hold.Class, err)
	}
	card, err := h.p.Card(ctx, call, hold.Binding)
	if err != nil || card.Moved || len(card.Acts) != 1 || !card.Acts[0].Danger {
		t.Fatalf("the card reads %+v, %v; want one act asking to confirm, not moved", card, err)
	}
	h.vend.mu.Lock()
	h.vend.channel["displayName"] = "Spring, renamed"
	h.vend.mu.Unlock()
	call.HeldCall, call.Approver = "held-1", "founder"
	if _, err := h.p.Apply(ctx, call, hold.Binding); !errors.Is(err, growth.ErrMoved) {
		t.Fatalf("applying over a moved channel answered %v, want ErrMoved", err)
	}
	if h.writes() != 0 {
		t.Error("the delete was sent though its channel moved")
	}
	steps, _ := h.audit.take()
	if !slices.Equal(steps, []growth.Step{growth.StepAllowed, growth.StepVoid}) {
		t.Errorf("the access log holds %v, want the answer, then one void", steps)
	}
}

// AN OPERATE CHANGE IS MADE AT ONCE AND RECORDED, its name as words: a
// curated create sends a display unit, a curated patch the mask of what it
// names, and the access log keeps one made record whose name it digests.
func TestAnOperateChangeIsMadeOnceAndRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "operate")
	ctx := context.Background()
	call := h.call("adsense_ad_unit_upsert", map[string]any{"account": mine, "ad_client": client,
		"display_name": "Sidebar", "size": "300x250"})
	hold, err := h.p.Prepare(ctx, call)
	if err != nil || hold.Class != growth.ClassOperate {
		t.Fatalf("a new unit prepared as %v, %v", hold.Class, err)
	}
	if _, err := h.p.Apply(ctx, call, hold.Binding); err != nil {
		t.Fatal(err)
	}
	h.vend.mu.Lock()
	body := h.vend.lastBody
	h.vend.mu.Unlock()
	settings, _ := body["contentAdsSettings"].(map[string]any)
	if settings["type"] != "DISPLAY" || body["state"] != "ACTIVE" {
		t.Errorf("the new unit was sent as %v, want an active display unit", body)
	}
	steps, records := h.audit.take()
	if !slices.Equal(steps, []growth.Step{growth.StepMade}) || len(records) != 1 {
		t.Fatalf("the access log holds %v, want one made", steps)
	}
	for _, ch := range records[0].Changes {
		if ch.Field == "displayName" && !ch.Text {
			t.Errorf("the name is recorded as %+v, want it kept as words, by its digest", ch)
		}
	}
	rename := h.call("adsense_custom_channel_upsert", map[string]any{"account": mine, "ad_client": client,
		"custom_channel": "9", "active": false})
	hold, err = h.p.Prepare(ctx, rename)
	if err != nil || hold.Class != growth.ClassOperate {
		t.Fatalf("a channel turned off prepared as %v, %v", hold.Class, err)
	}
	if _, err := h.p.Apply(ctx, rename, hold.Binding); err != nil {
		t.Fatal(err)
	}
	if got := h.lastQuery().Get("updateMask"); got != "active" {
		t.Errorf("the patch's mask is %q, want active alone", got)
	}
}

// A PLATFORM EVENT'S PERSON NEVER REACHES THE QUESTION OR THE LOG: the email
// and billing address are a third party's personal data, so a person is
// asked, and the log records, their digest alone. The event binds to the
// sub-account it is about: once that account changes, nothing is sent.
//
// Mutation: flatten the event's body as it is, and the email shows; drop the
// event's getter, and it is sent to a sub-account that moved.
func TestAPlatformEventsPersonNeverReachesTheQuestionOrTheLog(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "administer")
	call := h.call("adsense_admin", generic("adsenseplatform.platforms.accounts.events.create",
		map[string]any{"platformsId": mine, "accountsId": child}, nil,
		map[string]any{"eventType": "LOG_IN_VIA_PLATFORM", "eventTime": "2026-11-21T10:00:00Z",
			"eventInfo": map[string]any{"email": "jane@example.com",
				"billingAddress": map[string]any{"city": "Springfield"}}}))
	hold, err := h.p.Prepare(context.Background(), call)
	if err != nil || hold.Class != growth.ClassAdmin {
		t.Fatalf("the event prepared as %v, %v; want admin", hold.Class, err)
	}
	asked, _ := json.Marshal(hold.Ask)
	for _, said := range []string{string(asked), hold.Question, hold.Context} {
		if strings.Contains(said, "jane@example.com") || strings.Contains(said, "Springfield") {
			t.Errorf("the hold carries the person's data: %s", said)
		}
	}
	if !strings.Contains(hold.Context, "eventInfo: (none) to sha256:") {
		t.Errorf("the question does not name the person's digest: %s", hold.Context)
	}
	h.vend.mu.Lock()
	h.vend.childState = "DISAPPROVED"
	h.vend.mu.Unlock()
	call.HeldCall, call.Approver = "held-1", "founder"
	if _, err := h.p.Apply(context.Background(), call, hold.Binding); !errors.Is(err, growth.ErrMoved) || h.writes() != 0 {
		t.Errorf("an event for a sub-account that changed answered %v with %d writes, want ErrMoved", err, h.writes())
	}
}

// A REPORT IS HELD TO ITS LISTS AND HONEST ABOUT ITS CUT: a dimension, a
// filter or a sort outside the lists is refused before Google; a report asks
// one row past its limit over the account's days, cuts and flags what is
// past it, passes Google's total, types money in its currency, fences page
// URLs and passes Google's warnings fenced.
//
// Mutation: drop a closed-list check, or the limit's extra row, and a case
// goes red.
func TestAReportIsHeldToItsListsAndHonestAboutItsCut(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "read")
	ctx := context.Background()
	report := func(extra map[string]any) map[string]any {
		args := map[string]any{"account": mine, "start_date": "2026-11-01", "end_date": "2026-11-03",
			"dimensions": []any{"DATE", "PAGE_URL"}, "metrics": []any{"ESTIMATED_EARNINGS", "IMPRESSIONS"}}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	for name, extra := range map[string]map[string]any{
		"an unknown dimension":        {"dimensions": []any{"DATE", "VISITOR_EMAIL"}},
		"an unknown metric":           {"metrics": []any{"REVENUE"}},
		"no metric":                   {"metrics": []any{}},
		"a filter on no dimension":    {"filters": []any{"COUNTRY_CODE==US,VISITOR==x"}},
		"a sort on no column":         {"order_by": []any{"-CLICKS"}},
		"a zone Google does not take": {"time_zone": "Europe/Paris"},
		"a start past three years":    {"start_date": "2020-01-01"},
	} {
		if _, err := h.p.Run(ctx, h.call("adsense_report", report(extra))); !refusedAs(err, growth.RefusedInvalid) {
			t.Errorf("%s answered %v, want invalid", name, err)
		}
	}
	if got := h.calls(); len(got) != 0 {
		t.Fatalf("Google was called for a refused report: %v", got)
	}
	out, err := h.p.Run(ctx, h.call("adsense_report", report(map[string]any{"limit": 2,
		"filters": []any{`COUNTRY_CODE==US,COUNTRY_NAME==Korea\, Republic of`}, "order_by": []any{"-IMPRESSIONS"}})))
	if err != nil {
		t.Fatal(err)
	}
	q := h.lastQuery()
	if q.Get("limit") != "3" || q.Get("dateRange") != "CUSTOM" || q.Get("startDate.day") != "1" ||
		!slices.Equal(q["dimensions"], []string{"DATE", "PAGE_URL"}) || q.Get("reportingTimeZone") != "" {
		t.Errorf("Google was asked %v, want limit 3 over the custom days in the account's zone", q)
	}
	var got struct {
		Report   growth.Report `json:"report"`
		Warnings []struct {
			Text string `json:"text"`
		} `json:"warnings"`
	}
	raw, _ := json.Marshal(out)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	r := got.Report
	if len(r.Rows) != 2 || !r.Truncated || r.TotalRows == nil || *r.TotalRows != 9 || r.TimeZone != "Europe/Berlin" {
		t.Errorf("the report holds %d rows, cut %v, total %v, zone %s; want 2, cut, 9, the account's zone",
			len(r.Rows), r.Truncated, r.TotalRows, r.TimeZone)
	}
	if len(r.Columns) != 4 || r.Columns[0].Type != growth.ColumnDate || !r.Columns[1].Untrusted ||
		r.Columns[2].Type != growth.ColumnMoney || r.Columns[2].Currency != "EUR" || !r.Estimated {
		t.Errorf("the columns are %+v, want a date, a fenced page URL and money in EUR, estimated", r.Columns)
	}
	if len(r.Rows) > 0 && (r.Rows[0][2] != "1.23" || r.Rows[0][3] != float64(40)) {
		t.Errorf("the first row is %v, want money as Google's decimal and a count", r.Rows[0])
	}
	if len(got.Warnings) != 1 || got.Warnings[0].Text != "Data is delayed." {
		t.Errorf("the warnings are %+v, want Google's", got.Warnings)
	}
	if _, err := h.p.Run(ctx, h.call("adsense_report", report(map[string]any{"time_zone": "America/Los_Angeles"}))); err != nil ||
		h.lastQuery().Get("reportingTimeZone") != "GOOGLE_TIME_ZONE" {
		t.Errorf("a report in Google's zone answered %v, asking %v", err, h.lastQuery())
	}
}

// EACH CALL HAS ONE OUTCOME ON THE ACCESS LOG, ITS OWN: a report records the
// report alone, never the account read its days were cut by, and a report
// whose account read fails leaves its one failure.
//
// Mutation: record the account read under the call, or drop the failed
// record, and the log holds two outcomes or none.
func TestEachCallHasOneOutcomeOnTheLogItsOwn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "read")
	ctx := context.Background()
	if _, err := h.p.Run(ctx, h.call("adsense_report", map[string]any{"account": mine, "range": "last_7_days",
		"metrics": []any{"CLICKS"}})); err != nil {
		t.Fatal(err)
	}
	_, records := h.audit.take()
	if len(records) != 1 || records[0].Failure != "" {
		t.Errorf("the access log holds %+v, want the report made, once", records)
	}
	down := newHarness(t, "read")
	down.vend.accountsDown = true
	if _, err := down.p.Run(ctx, down.call("adsense_report", map[string]any{"account": mine,
		"range": "yesterday", "metrics": []any{"CLICKS"}})); err == nil {
		t.Fatal("a report whose account read failed answered")
	}
	if _, records := down.audit.take(); len(records) != 1 || records[0].Failure == "" {
		t.Errorf("the access log holds %+v, want one failure", records)
	}
}

// A CSV REPORT IS READ AS TEXT: Google answers it as bytes of its own type,
// which reach the agent as that type and its text, fenced.
func TestACSVReportIsReadAsText(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "read")
	out, err := h.p.Run(context.Background(), h.call("adsense_read", generic("adsense.accounts.reports.generateCsv",
		map[string]any{"accountsId": mine}, map[string]any{"dateRange": "LAST_7_DAYS",
			"metrics": []any{"ESTIMATED_EARNINGS"}, "dimensions": []any{"DATE"}}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if answer := out.(googlegrowth.Answer).Answer; !strings.Contains(answer, `2026-11-20,1.23`) ||
		!strings.Contains(answer, "text/csv") {
		t.Errorf("the CSV report answered %s", answer)
	}
	if q := h.lastQuery(); !slices.Equal(q["dimensions"], []string{"DATE"}) || q.Get("metrics") != "ESTIMATED_EARNINGS" {
		t.Errorf("Google was asked %v", q)
	}
}

// A GOOGLE REFUSAL SAYS WHAT TO ASK FOR: a read-only sign-in names signing in
// again, a method Google has not enabled names the account manager, a Google
// account with no AdSense account names who signs in, and the API turned off
// names the project.
func TestAGoogleRefusalSaysWhatToAskFor(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reason, message, want string
	}{
		"a read-only sign-in": {"ACCESS_TOKEN_SCOPE_INSUFFICIENT", "Request had insufficient authentication scopes.",
			"signs in with Google again"},
		"a method not enabled": {"", "The caller does not have permission", "account manager"},
		"no AdSense account":   {"", "User does not have an AdSense account.", "has no AdSense account"},
		"the API off":          {"SERVICE_DISABLED", "AdSense Management API has not been used in project 1", "turns it on"},
	} {
		h := newHarness(t, "operate")
		h.vend.refuse = http.StatusForbidden
		h.vend.refuseBody = map[string]any{"error": map[string]any{"code": 403, "status": "PERMISSION_DENIED",
			"message": tc.message, "details": []any{map[string]any{
				"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": tc.reason}}}}
		call := h.call("adsense_custom_channel_upsert", map[string]any{"account": mine, "ad_client": client,
			"display_name": "Spring"})
		hold, err := h.p.Prepare(context.Background(), call)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.p.Apply(context.Background(), call, hold.Binding)
		if !refusedAs(err, growth.RefusedForbidden) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s answered %v, want a refusal saying %q", name, err, tc.want)
		}
	}
}

// THE PASS NAMES WHAT THE SIGN-IN LACKS: a sign-in made for reading while an
// agent may change settings, and an account the sign-in cannot reach; the
// accounts it reached are the grant's record.
//
// Mutation: drop the scope check, or count a refused account reached, and
// the pass says nothing.
func TestThePassNamesWhatTheSignInLacks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "operate")
	h.company.Integrations.AdSense.Accounts = []string{mine, theirs}
	findings, reached, err := h.p.Check(context.Background(), adsense.ReadScopes())
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, f := range findings {
		if f.Kind != integration.FindingGrantShort {
			t.Errorf("finding %+v is not a short grant", f)
		}
		subjects = append(subjects, f.Subject)
	}
	if !slices.Equal(subjects, []string{"adsense", "accounts"}) || !slices.Equal(reached, []string{mine}) {
		t.Errorf("the pass found %v and reached %v; want the scope and the unreached account, and %s reached",
			subjects, reached, mine)
	}
	if findings, _, _ := h.p.Check(context.Background(), adsense.OperateScopes()); len(findings) != 1 {
		t.Errorf("a sign-in allowing changes still found %+v, want the unreached account alone", findings)
	}
}

// A SIGN-IN ASKS FOR WHAT THE SEATS NEED NOW: reading alone while every seat
// reads, and changes too once one may make them, never more.
func TestASignInAsksForWhatTheSeatsNeedNow(t *testing.T) {
	t.Parallel()
	on := true
	c := &config.Company{Name: "Acme"}
	c.Integrations.AdSense = &config.AdSense{Enabled: true}
	c.Roles = []config.Role{{Name: "Finance", Handle: "finance", Integrations: config.RoleIntegrations{
		AdSense: &config.RoleAdSense{Enabled: &on, Accounts: []string{mine}}}}}
	if got := adsense.GrantScopes(c); !slices.Equal(got, adsense.ReadScopes()) {
		t.Errorf("a company that reads asks for %v", got)
	}
	c.Roles[0].Integrations.AdSense.Level = "operate"
	if got := adsense.GrantScopes(c); !slices.Equal(got, adsense.OperateScopes()) {
		t.Errorf("a company with a seat at operate asks for %v", got)
	}
}

// A SITE MOVED BETWEEN REVENUE GROUPS IS ASKED AS THE SHARE IT MOVES: both
// groups' revenue shares are read first, a person is asked the share before
// and after, and once the joining group's share changes the approval is void
// and nothing is sent.
//
// Mutation: drop the groups' read, and the share is neither asked nor bound.
func TestASiteMovedBetweenRevenueGroupsIsAskedAsTheShareItMoves(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "administer")
	ctx := context.Background()
	call := h.call("adsense_admin", generic("adsenseplatform.accounts.platforms.childAccounts.sites.patch",
		map[string]any{"accountsId": mine, "platformsId": "p1", "childAccountsId": child, "sitesId": "s1"},
		map[string]any{"updateMask": "platform_group"},
		map[string]any{"platformGroup": "accounts/" + mine + "/platforms/p1/groups/g2"}))
	hold, err := h.p.Prepare(ctx, call)
	if err != nil || hold.Class != growth.ClassAdmin {
		t.Fatalf("the move prepared as %v, %v; want admin", hold.Class, err)
	}
	if !strings.Contains(hold.Context, "revshareMillipercent: 70000 to 60000") {
		t.Errorf("the question does not say the share it moves: %s", hold.Context)
	}
	h.vend.mu.Lock()
	h.vend.shares["g2"] = "50000"
	h.vend.mu.Unlock()
	call.HeldCall, call.Approver = "held-1", "founder"
	if _, err := h.p.Apply(ctx, call, hold.Binding); !errors.Is(err, growth.ErrMoved) || h.writes() != 0 {
		t.Errorf("applying after the group's share changed answered %v with %d writes, want ErrMoved and none",
			err, h.writes())
	}
}

// THE ACCOUNT LISTING SAYS EACH ACCOUNT AS GOOGLE ANSWERS NOW: an account the
// sign-in cannot reach is named with why, a state that changed is the new
// one whatever a report read earlier, and the call is one outcome.
//
// Mutation: read the listing from the cache, and the old state is answered.
func TestTheAccountListingSaysEachAccountAsGoogleAnswersNow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "read")
	ctx := context.Background()
	h.company.Integrations.AdSense.Accounts = nil
	h.company.Roles[0].Integrations.AdSense.Accounts = []string{mine, theirs}
	if _, err := h.p.Run(ctx, h.call("adsense_report", map[string]any{"account": mine, "range": "yesterday",
		"metrics": []any{"CLICKS"}})); err != nil {
		t.Fatal(err)
	}
	h.audit.take()
	h.vend.mu.Lock()
	h.vend.state = "NEEDS_ATTENTION"
	h.vend.mu.Unlock()
	out, err := h.p.Run(ctx, h.call("adsense_accounts_list", map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	var got struct {
		Accounts []struct{ Account, State, Unreached string } `json:"accounts"`
	}
	_ = json.Unmarshal(raw, &got)
	if len(got.Accounts) != 2 || got.Accounts[0].State != "NEEDS_ATTENTION" || got.Accounts[1].Unreached == "" {
		t.Errorf("the listing answered %s, want %s needing attention and %s unreached", raw, mine, theirs)
	}
	if _, records := h.audit.take(); len(records) != 1 || records[0].Failure != "" {
		t.Errorf("the access log holds %+v, want the listing made, once", records)
	}
}

// A SAVED REPORT RUNS ITS SAVED COLUMNS OVER THE CALL'S DAYS, CUT HERE: its
// listing is a page of saved reports, and running one sends no row limit,
// which Google does not take for it, so the answer is cut to the call's.
func TestASavedReportRunsOverTheCallsDays(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "read")
	ctx := context.Background()
	if _, err := h.p.Run(ctx, h.call("adsense_saved_report", map[string]any{"account": mine})); err != nil ||
		!strings.HasSuffix(h.calls()[0], "/reports/saved") {
		t.Fatalf("the saved reports listed with %v, calling %v", err, h.calls())
	}
	out, err := h.p.Run(ctx, h.call("adsense_saved_report", map[string]any{"account": mine, "report": "r1",
		"range": "last_7_days", "limit": 2}))
	if err != nil {
		t.Fatal(err)
	}
	calls := h.calls()
	if q := h.lastQuery(); !strings.HasSuffix(calls[len(calls)-1], "/reports/r1/saved:generate") ||
		q.Has("limit") || q.Get("dateRange") != "CUSTOM" {
		t.Errorf("the saved report called %s with %v", calls[len(calls)-1], q)
	}
	raw, _ := json.Marshal(out)
	var got struct{ Report growth.Report }
	_ = json.Unmarshal(raw, &got)
	if len(got.Report.Rows) != 2 || !got.Report.Truncated {
		t.Errorf("the saved report answered %s, want two rows, cut", raw)
	}
}

// AD CODE IS READ FROM THE UNIT OR THE AD CLIENT THE CALL NAMES, fenced as
// somebody else's words.
func TestAdCodeIsReadFromWhatTheCallNames(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "read")
	ctx := context.Background()
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"account": mine, "ad_client": client}, "/adclients/" + client + "/adcode"},
		{map[string]any{"account": mine, "ad_client": client, "ad_unit": "7"}, "/adunits/7/adcode"},
	} {
		out, err := h.p.Run(ctx, h.call("adsense_ad_code", tc.args))
		if err != nil {
			t.Fatal(err)
		}
		calls := h.calls()
		if !strings.HasSuffix(calls[len(calls)-1], tc.want) || !strings.Contains(out.(googlegrowth.Answer).Answer, "script") {
			t.Errorf("ad code %v called %s and answered %v", tc.args, calls[len(calls)-1], out)
		}
	}
}

// A WRITE IS NEVER RUN AS A READ, NOR A READ PREPARED AS A WRITE: each goes
// by the path its tool's class sets, so a change is always classed and bound.
//
// Mutation: drop Run's class check, and a write is sent unclassed.
func TestAWriteIsNeverRunAsARead(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "administer")
	ctx := context.Background()
	if _, err := h.p.Run(ctx, h.call("adsense_admin", generic("adsense.accounts.adclients.customchannels.delete",
		channelPath(), nil, nil))); !refusedAs(err, growth.RefusedInvalid) {
		t.Errorf("a delete run as a read answered %v, want invalid", err)
	}
	if _, err := h.p.Prepare(ctx, h.call("adsense_sites_list", map[string]any{"account": mine})); !refusedAs(err, growth.RefusedInvalid) {
		t.Errorf("a listing prepared as a change answered %v, want invalid", err)
	}
	if got := h.calls(); len(got) != 0 {
		t.Errorf("Google was called: %v", got)
	}
}

// A QUOTA REFUSAL SAYS WHEN QUOTAS FREE UP.
func TestAQuotaRefusalSaysWhenQuotasFreeUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "operate")
	h.vend.refuse = http.StatusTooManyRequests
	h.vend.refuseBody = map[string]any{"error": map[string]any{"code": 429, "status": "RESOURCE_EXHAUSTED",
		"message": "Quota exceeded"}}
	call := h.call("adsense_custom_channel_upsert", map[string]any{"account": mine, "ad_client": client,
		"display_name": "Spring"})
	hold, err := h.p.Prepare(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.p.Apply(context.Background(), call, hold.Binding); !refusedAs(err, growth.RefusedUnavailable) ||
		!strings.Contains(err.Error(), "midnight Pacific time") {
		t.Errorf("a quota refusal answered %v", err)
	}
}
