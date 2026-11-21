package appstoreconnect

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
)

// Answer is a generic call's answer: Apple's document, fenced, since it holds
// customers', testers' and colleagues' words at places no type declares.
type Answer struct {
	Operation  string `json:"operation"`
	Status     int    `json:"status"`
	Connection string `json:"connection"`
	// Role is the Apple role of the key the call went through.
	Role       string `json:"key_role"`
	Document   string `json:"document" untrusted:"fence"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Run makes a read tool's call; a write is prepared and applied ([Product.Prepare]).
func (p *Product) Run(ctx context.Context, call growth.Call) (any, error) {
	switch call.Tool {
	case ToolOperations:
		return p.listOperations(call)
	case ToolReviews:
		return p.reviews(ctx, call)
	case ToolSales:
		return p.sales(ctx, call)
	case ToolRead:
		return p.generic(ctx, call)
	}
	return nil, invalid("%s is not a read tool of App Store Connect.", call.Tool)
}

// generic makes a read of any read operation.
func (p *Product) generic(ctx context.Context, call growth.Call) (any, error) {
	m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
	if err != nil {
		return nil, err
	}
	resp, err := m.team.client.Do(ctx, m.slot, Request{Method: m.op.Method, Path: m.path, Query: m.query,
		Body: m.body, Read: m.op.Method == "GET"})
	rec := recordOf(call, m)
	if err != nil {
		var refused *growth.Refusal
		if !errors.As(err, &refused) {
			rec.Failure = failureOf(err)
			p.note(ctx, growth.StepFailed, rec)
		}
		return nil, said(err)
	}
	p.note(ctx, growth.StepMade, rec)
	body := resp.Body
	if m.op.Method == "GET" {
		if m.app != "" {
			p.owners.remember(m.app, body)
		} else if m.op.Path == "/v1/apps" {
			body = onlyApps(body, p.seatApps(call.Handle))
		}
	}
	return Answer{Operation: m.op.ID, Status: resp.Status, Connection: m.team.id, Role: slotRole[m.slot],
		Document: string(body), NextCursor: nextOf(body)}, nil
}

func (p *Product) seatApps(handle string) []string {
	if b := p.deps.Company().AppStoreConnectSeat(handle); b != nil {
		return b.Apps
	}
	return nil
}

// onlyApps is the team's app list with the apps outside allowed removed.
func onlyApps(body []byte, allowed []string) []byte {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return []byte(`{"data": []}`)
	}
	data, _ := doc["data"].([]any)
	kept := make([]any, 0, len(data))
	for _, d := range data {
		r, _ := d.(map[string]any)
		if id, _ := r["id"].(string); slices.Contains(allowed, id) {
			kept = append(kept, d)
		}
	}
	doc["data"] = kept
	delete(doc, "included")
	out, err := json.Marshal(doc)
	if err != nil {
		return []byte(`{"data": []}`)
	}
	return out
}

func nextOf(body []byte) string {
	var doc struct {
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	return cursorOf(doc.Links.Next)
}

// said is err as the agent is told it: a refusal as itself, Apple's own
// refusal in its words, anything else as the engine's fault.
func said(err error) error {
	var r *growth.Refusal
	if errors.As(err, &r) {
		return r
	}
	var e *APIError
	if errors.As(err, &e) {
		kind := growth.RefusedInvalid
		switch e.Status {
		case 401, 403:
			kind = growth.RefusedForbidden
		case 404:
			kind = growth.RefusedNotFound
		case 409:
			kind = growth.RefusedConflict
		case 429, 500, 502, 503, 504:
			kind = growth.RefusedUnavailable
		}
		return growth.Refuse(kind, "App Store Connect answered: "+e.Error())
	}
	if errors.Is(err, ErrReserve) {
		return growth.Refuse(growth.RefusedUnavailable, "this key's calls for the hour are down to the ones kept "+
			"for approved acts; try again later in the hour.")
	}
	return err
}

// OperationRow is one operation as the discovery tool lists it.
type OperationRow struct {
	ID       string   `json:"operation"`
	Method   string   `json:"method"`
	Path     string   `json:"path"`
	Resource string   `json:"resource"`
	Class    string   `json:"class"`
	Lowered  []string `json:"lowered_to,omitempty"`
	Danger   bool     `json:"cannot_be_undone,omitempty"`
	Roles    []string `json:"roles"`
	Tool     string   `json:"tool"`
}

// OperationList is the discovery tool's answer.
type OperationList struct {
	Operations []OperationRow `json:"operations"`
	Truncated  bool           `json:"truncated"`
	Total      int            `json:"total"`
}

// maxListed bounds the discovery tool's answer.
const maxListed = 200

// listOperations lists the operations the seat's level allows, filtered.
func (p *Product) listOperations(call growth.Call) (any, error) {
	seat, err := p.seat(call.Handle)
	if err != nil {
		return nil, err
	}
	level := growth.LevelOf(seat.Level)
	resource, _ := call.Args["resource"].(string)
	class, _ := call.Args["class"].(string)
	text, _ := call.Args["text"].(string)
	var out OperationList
	for _, op := range operations {
		lowest := op.Class
		var lowered []string
		for _, s := range op.States {
			lowered = append(lowered, string(s.Class)+" when "+stateWords[s.When])
			if s.Class.Rank() < lowest.Rank() {
				lowest = s.Class
			}
		}
		switch {
		case !level.Allows(lowest):
			continue
		case resource != "" && !strings.EqualFold(resource, op.Resource):
			continue
		case class != "" && class != string(op.Class) && !slices.ContainsFunc(op.States,
			func(s growth.StateRule) bool { return string(s.Class) == class }):
			continue
		case text != "" && !strings.Contains(strings.ToLower(op.ID+" "+op.Path), strings.ToLower(text)):
			continue
		}
		out.Total++
		if len(out.Operations) >= maxListed {
			out.Truncated = true
			continue
		}
		out.Operations = append(out.Operations, OperationRow{ID: op.ID, Method: op.Method, Path: op.Path,
			Resource: op.Resource, Class: string(op.Class), Lowered: lowered, Danger: op.Danger,
			Roles: rolesNames(op.Roles), Tool: toolFor(op.Class)})
	}
	return out, nil
}
