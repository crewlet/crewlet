package googleanalytics

import (
	"context"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// Run makes a read call; every other call is prepared and applied
// ([Product.Prepare], [Product.Apply]).
func (p *Product) Run(ctx context.Context, call growth.Call) (any, error) {
	switch call.Tool {
	case toolListProperties:
		return p.listProperties(ctx, call)
	case toolRunReport:
		return p.runReport(ctx, call)
	case toolRealtime:
		return p.realtime(ctx, call)
	}
	req, class, err := p.requestOf(call)
	if err != nil {
		return nil, err
	}
	if class != growth.ClassRead {
		return nil, refusal(growth.RefusedInvalid, call.Tool+" changes Google Analytics, so it is prepared and "+
			"applied, never run as a read. No vendor was reached.")
	}
	if err = p.inScope(ctx, call.Handle, req); err != nil {
		return nil, err
	}
	out, err := p.client.SendAndRead(ctx, call.Handle, req, p.cut(call.Handle, req))
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// requestOf is a write or generic tool's request and the class of its tool:
// a curated tool's built from its typed arguments, a generic tool's from the
// operation it names.
func (p *Product) requestOf(call growth.Call) (googlegrowth.Request, growth.Class, error) {
	if call.Tool == toolMarkKeyEvent {
		req, err := p.markKeyEvent(call)
		return req, growth.ClassOperate, err
	}
	return p.generic(call)
}

// built is call's request held to the seat's scope, kept where that failed,
// for the record of it.
func (p *Product) built(ctx context.Context, call growth.Call) (googlegrowth.Request, error) {
	req, _, err := p.requestOf(call)
	if err == nil {
		err = p.inScope(ctx, call.Handle, req)
	}
	return req, err
}

// cut is how an answer to req is cut to handle's properties: only where req
// lists accounts or properties the identity reaches beyond the seat's scope.
func (p *Product) cut(handle string, req googlegrowth.Request) func(any) any {
	if !listsAccounts(req.Op.ID) {
		return nil
	}
	return func(v any) any { return p.cutToScope(handle, v) }
}

// listsAccounts reports an operation that lists accounts or properties the
// identity reaches beyond the seat's scope.
func listsAccounts(id string) bool {
	return slices.Contains([]string{"analyticsadmin.accountSummaries.list", "analyticsadmin.accounts.list",
		"analyticsadmin.properties.list"}, id)
}

// cutToScope drops every account and property of a listing the seat may not
// reach: an account stays only where one of its properties is in scope.
func (p *Product) cutToScope(handle string, v any) any {
	b := p.seat(handle)
	obj, ok := v.(map[string]any)
	if b == nil || !ok {
		return map[string]any{}
	}
	inScope := func(name any) bool {
		s, _ := name.(string)
		return slices.Contains(b.Properties, strings.TrimPrefix(s, "properties/"))
	}
	if list, ok := obj["properties"].([]any); ok {
		obj["properties"] = slices.DeleteFunc(list, func(e any) bool {
			m, _ := e.(map[string]any)
			return !inScope(m["name"])
		})
	}
	if list, ok := obj["accountSummaries"].([]any); ok {
		var kept []any
		for _, e := range list {
			m, _ := e.(map[string]any)
			props, _ := m["propertySummaries"].([]any)
			props = slices.DeleteFunc(props, func(e any) bool {
				pm, _ := e.(map[string]any)
				return !inScope(pm["property"])
			})
			if len(props) > 0 {
				m["propertySummaries"] = props
				kept = append(kept, m)
			}
		}
		obj["accountSummaries"] = kept
	}
	if _, ok := obj["accounts"]; ok {
		// AN ACCOUNT LISTING NAMES NO PROPERTY, so only accounts holding a
		// property in scope are named, read through the summaries.
		obj["accounts"] = []any{}
	}
	return obj
}

// generic builds a generic tool's request and the class its tool makes; a
// request built before a refusal is kept, for the record of it.
func (p *Product) generic(call growth.Call) (googlegrowth.Request, growth.Class, error) {
	tool, ok := p.Catalog().Tool(call.Tool)
	if !ok {
		return googlegrowth.Request{}, "", refusal(growth.RefusedNotFound, call.Tool+" is not a Google Analytics tool.")
	}
	id, _ := call.Args["operation"].(string)
	if !slices.Contains(tool.Operations, id) {
		return googlegrowth.Request{}, tool.Class, p.Catalog().Outside(call.Tool, id, p.Level(call.Handle))
	}
	op, _ := p.Catalog().Operation(id)
	req, err := newRequest(op, stringMap(call.Args["path"]), stringMap(call.Args["query"]), objectArg(call.Args["body"]))
	if err == nil && strings.HasPrefix(id, "mp.") {
		err = p.measurementSecret(call.Handle, &req)
	}
	return req, tool.Class, err
}

// readScope is the scope a write's target is read back with: the readonly
// scope, but the users scopes for an access binding.
func readScope(op growth.Operation) []string {
	if strings.Contains(op.Path, "accessBindings") {
		return []string{scopeUsersRead}
	}
	return []string{scopeReadonly}
}

// stringMap is an argument object of strings.
func stringMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

// objectArg is an argument object, nil where none.
func objectArg(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// markKeyEvent is google_analytics_mark_key_event's request.
func (p *Product) markKeyEvent(call growth.Call) (googlegrowth.Request, error) {
	op, _ := p.Catalog().Operation("analyticsadmin.properties.keyEvents.create")
	property, _ := call.Args["property"].(string)
	event, _ := call.Args["event_name"].(string)
	method, _ := call.Args["counting_method"].(string)
	if method == "" {
		method = "ONCE_PER_EVENT"
	}
	if strings.TrimSpace(event) == "" {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, "name the event to mark. No vendor was reached.")
	}
	return newRequest(op, map[string]string{"propertiesId": property}, nil,
		map[string]any{"eventName": event, "countingMethod": method})
}
