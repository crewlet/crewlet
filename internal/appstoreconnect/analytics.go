package appstoreconnect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/es256"
	"github.com/crewlet/crewlet/internal/growth"
)

// App Store analytics needs an ongoing report request per app, which only an
// Admin key may create. A team that keeps no Admin key turns it on with one a
// person pastes once: each app's request is signed with it at once, and the
// key is never sealed, logged or recorded, and dropped when the call returns.

// AnalyticsEnable is one turn-on: the connection, the pasted Admin key's ID
// and .p8, and the human seat of the administrator who pasted it.
type AnalyticsEnable struct {
	Connection, KeyID, PrivateKey, By string
}

// AnalyticsApp is one app's outcome of a turn-on.
type AnalyticsApp struct {
	App string `json:"app"`
	// State is AnalyticsOn, AnalyticsAlreadyOn or AnalyticsFailed.
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// The outcomes of one app's turn-on.
const (
	AnalyticsOn        = "on"
	AnalyticsAlreadyOn = "already_on"
	AnalyticsFailed    = "failed"
)

// AnalyticsStates are the outcomes, for the dashboard's copy of them.
func AnalyticsStates() []string { return []string{AnalyticsOn, AnalyticsAlreadyOn, AnalyticsFailed} }

// ToolAnalytics is the access log's name for a turn-on, which a person makes.
const ToolAnalytics = "app_store_connect_analytics"

// The operations a turn-on makes: one reporting read per app, then the create.
const (
	opAnalyticsList   = "apps_analyticsReportRequests_getToManyRelated"
	opAnalyticsCreate = "analyticsReportRequests_createInstance"
)

// keyIDShape is an App Store Connect key ID.
var keyIDShape = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Z0-9]{10}$`) })

// EnableAnalytics turns App Store analytics on for every app in the
// connection's reach, through the pasted Admin key alone.
func (p *Product) EnableAnalytics(ctx context.Context, in AnalyticsEnable) ([]AnalyticsApp, error) {
	block := p.deps.Company().Integrations.AppStoreConnect
	if !block.GrowthEnabled() {
		return nil, growth.Refuse(growth.RefusedUnavailable, "App Store Connect is not connected. Nothing was sent.")
	}
	teams, err := p.teamsNow()
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(teams, func(t *team) bool { return t.id == in.Connection })
	switch {
	case i < 0:
		return nil, growth.Refuse(growth.RefusedNotFound, fmt.Sprintf("no App Store Connect connection is named %q. "+
			"Nothing was sent.", in.Connection))
	case !keyIDShape().MatchString(strings.TrimSpace(in.KeyID)):
		return nil, growth.Refuse(growth.RefusedInvalid, "the key ID is ten capital letters and digits, shown "+
			"beside the key in App Store Connect. Nothing was sent.")
	}
	t := teams[i]
	// NOT ECHOED: a parse error may quote the pasted key.
	key, err := es256.ParseKey(in.PrivateKey)
	if err != nil {
		return nil, growth.Refuse(growth.RefusedInvalid, "the pasted key does not read as an App Store Connect "+
			".p8 key (BEGIN PRIVATE KEY). Nothing was sent.")
	}
	apps, err := p.analyticsApps(ctx, t, block.Apps)
	if err != nil {
		return nil, err
	}
	listOp, _ := Catalog().Operation(opAnalyticsList)
	reporting, canList := keyFor(listOp, t.slots, false)
	createOp, _ := Catalog().Operation(opAnalyticsCreate)
	out := make([]AnalyticsApp, 0, len(apps))
	for _, app := range apps {
		if canList {
			if on, err := p.analyticsOn(ctx, t, reporting, app); err == nil && on {
				out = append(out, AnalyticsApp{App: app, State: AnalyticsAlreadyOn})
				continue
			}
		}
		out = append(out, p.createAnalytics(ctx, t, createOp, app, in.By, Key{ID: strings.TrimSpace(in.KeyID), Key: key}))
	}
	return out, nil
}

// analyticsApps are the apps a turn-on covers: the company's named apps this
// connection reaches, or every app its own keys reach.
func (p *Product) analyticsApps(ctx context.Context, t *team, named []string) ([]string, error) {
	infos, err := p.appsOf(ctx, t)
	if len(named) > 0 {
		if err != nil {
			//nolint:nilerr // Not listable here: each named app is tried, and another team's fails by its own answer.
			return named, nil
		}
		// ANOTHER CONNECTION'S APPS ARE ITS OWN: the named apps this team reaches.
		return slices.DeleteFunc(slices.Clone(named), func(app string) bool {
			return !slices.ContainsFunc(infos, func(a appInfo) bool { return a.ID == app })
		}), nil
	}
	if errors.Is(err, ErrNoKey) {
		return nil, growth.Refuse(growth.RefusedForbidden, "the connection holds no key that lists its apps: add "+
			"a Marketing, Finance or Sales key, or name the apps in the integration's settings. Nothing was sent.")
	}
	if err != nil {
		return nil, said(err)
	}
	out := make([]string, 0, len(infos))
	for _, a := range infos {
		out = append(out, a.ID)
	}
	return out, nil
}

// analyticsOn reports whether app has an ongoing request Apple has not
// stopped, read through the connection's reporting key.
func (p *Product) analyticsOn(ctx context.Context, t *team, slot, app string) (bool, error) {
	resp, err := t.client.Do(ctx, slot, Request{Method: "GET", Path: "/v1/apps/" + url.PathEscape(app) +
		"/analyticsReportRequests", Query: url.Values{"filter[accessType]": {"ONGOING"}}, Read: true, Reserved: true})
	if err != nil {
		return false, err
	}
	var doc struct {
		Data []analyticsRequest `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return false, fmt.Errorf("appstoreconnect: read app %s's analytics requests: %w", app, err)
	}
	return slices.ContainsFunc(doc.Data, func(r analyticsRequest) bool { return !r.Attributes.Stopped }), nil
}

// analyticsRequest is an analytics report request as Apple lists it.
type analyticsRequest struct {
	Attributes struct {
		// Stopped is a request Apple stopped after long inactivity.
		Stopped bool `json:"stoppedDueToInactivity"`
	} `json:"attributes"`
}

// createAnalytics creates app's ongoing request with the pasted key, signed
// for that one request by a client made for it and dropped after.
func (p *Product) createAnalytics(ctx context.Context, t *team, op growth.Operation, app, by string, key Key) AnalyticsApp {
	body := map[string]any{"data": map[string]any{"type": "analyticsReportRequests",
		"attributes":    map[string]any{"accessType": "ONGOING"},
		"relationships": map[string]any{"app": map[string]any{"data": map[string]any{"type": "apps", "id": app}}}}}
	m := made{op: op, class: op.Class, path: op.Path, body: body, app: app, team: t, slot: adminSlot}
	rec := recordOf(growth.Call{ID: uuid.Must(uuid.NewV7()).String(), Handle: by, Tool: ToolAnalytics}, m)
	once := NewClient(p.deps.Base, t.client.issuer, map[string]Key{adminSlot: key}, p.deps.HTTP)
	_, err := once.Do(ctx, adminSlot, Request{Method: "POST", Path: op.Path, Body: body, Reserved: true})
	var apiErr *APIError
	switch {
	case err == nil:
		p.note(ctx, growth.StepMade, rec)
		return AnalyticsApp{App: app, State: AnalyticsOn}
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict:
		// ONE ONGOING REQUEST PER APP: Apple refuses a second.
		rec.Failure = growth.FailureVendorRefused
		p.note(ctx, growth.StepFailed, rec)
		return AnalyticsApp{App: app, State: AnalyticsAlreadyOn}
	}
	rec.Failure = failureOf(err)
	p.note(ctx, growth.StepFailed, rec)
	return AnalyticsApp{App: app, State: AnalyticsFailed, Detail: said(err).Error()}
}
