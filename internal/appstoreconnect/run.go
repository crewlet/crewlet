package appstoreconnect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	resp, err := m.do(ctx, Request{Method: m.op.Method, Path: m.path, Query: m.query,
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
	body, err := p.cut(ctx, call.Handle, m, resp.Body)
	if err != nil {
		return nil, err
	}
	return Answer{Operation: m.op.ID, Status: resp.Status, Connection: m.team.id, Role: m.key.role,
		Document: string(body), NextCursor: nextOf(body)}, nil
}

// cut is an answer as the seat may read it, and fills the cache only from an
// answer rooted at the seat's app (package doc, "Scope").
func (p *Product) cut(ctx context.Context, handle string, m made, body []byte) ([]byte, error) {
	seat, err := p.seat(handle)
	if err != nil {
		return nil, err
	}
	if rootedAt(m, body) {
		p.owners.remember(m.app, body)
	} else if namesOwned(body) {
		covers, err := p.covers(ctx, seat, m)
		if err != nil {
			return nil, said(err)
		}
		if !covers {
			return nil, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s answered resources of the team's "+
				"apps through a resource no app owns, and this agent's apps are not all of the team's: read "+
				"them through one of its apps. Nothing is shown.", m.op.ID))
		}
	}
	return cutApps(body, seat.Apps)
}

// rootedAt reports whether m's answer is rooted at m's app through owned
// hops: its path starts at an app or an owned resource placed in m's app, and
// every resource it answers is an app or owned.
func rootedAt(m made, body []byte) bool {
	typ, _ := typeAndID(m.path)
	if m.app == "" || (typ != "apps" && !topology().owned[typ]) {
		return false
	}
	var doc struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	return !slices.ContainsFunc(refsOf(doc.Data), func(r resourceRef) bool { return r.Type != "apps" && !topology().owned[r.Type] })
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
	// Level is the least level offering it; Above marks one above this agent's.
	Level string `json:"level"`
	Above bool   `json:"above_your_level,omitempty"`
}

// OperationList is the discovery tool's answer.
type OperationList struct {
	Operations []OperationRow `json:"operations"`
	Truncated  bool           `json:"truncated"`
	Total      int            `json:"total"`
}

// maxListed bounds the discovery tool's answer.
const maxListed = 200

// listOperations lists the operations, filtered, each with the least level
// offering it and whether that is above the seat's.
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
	for _, op := range operations() {
		lowest := op.Class
		var lowered []string
		for _, s := range op.States {
			lowered = append(lowered, string(s.Class)+" when "+stateWords[s.When])
			if s.Class.Rank() < lowest.Rank() {
				lowest = s.Class
			}
		}
		switch {
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
			Roles: rolesNames(op.Roles), Tool: toolFor(op.Class), Level: string(lowest.Level()),
			Above: !level.Allows(lowest)})
	}
	return out, nil
}
