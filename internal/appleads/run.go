package appleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
)

// Answer is a generic read's answer: Apple's JSON, fenced, since it holds
// campaign names, keywords and search terms other people wrote.
type Answer struct {
	Operation  string   `json:"operation"`
	Status     int      `json:"status"`
	Connection string   `json:"connection"`
	Account    string   `json:"account,omitempty"`
	Roles      []string `json:"roles_held"`
	Document   string   `json:"document" untrusted:"fence"`
}

// Run makes a read tool's call; a write is prepared and applied ([Product.Prepare]).
func (p *Product) Run(ctx context.Context, call growth.Call) (any, error) {
	switch call.Tool {
	case ToolOperations:
		return p.listOperations(call)
	case ToolReport:
		return p.report(ctx, call)
	case ToolRead:
		return p.generic(ctx, call)
	}
	return nil, invalid("%s is not a read tool of Apple Ads.", call.Tool)
}

// generic makes any read operation, its answer held to the seat's ad accounts.
func (p *Product) generic(ctx context.Context, call growth.Call) (any, error) {
	m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
	if err != nil {
		return nil, err
	}
	t := m.targets[0]
	rec := recordOf(call, m, growth.ClassRead)
	if parent := parentOf(t.path); parent != "" {
		if rerr := p.checkRefs(ctx, m, target{op: t.op, path: t.path}); rerr != nil {
			return nil, said(rerr)
		}
	}
	resp, err := m.conn.client.Do(ctx, Request{Method: t.op.Method, Path: t.path, Query: t.query, Body: t.body,
		Account: m.account, Read: true})
	if err == nil {
		var body []byte
		if body, err = m.scope(resp.Body); err == nil {
			p.note(ctx, growth.StepMade, rec)
			return Answer{Operation: t.op.ID, Status: resp.Status, Connection: m.conn.id, Account: m.account,
				Roles: rolesNames(m.roles), Document: string(body)}, nil
		}
	}
	var refused *growth.Refusal
	if !errors.As(err, &refused) {
		rec.Failure = failureOf(err)
		p.note(ctx, growth.StepFailed, rec)
	}
	return nil, said(err)
}

// scope holds a read's answer to m's ad account and the seat's: an object of
// another ad account is refused, and a list keeps only the seat's.
func (m made) scope(body []byte) ([]byte, error) {
	var doc map[string]any
	if err := decode(body, &doc); err != nil {
		return nil, fmt.Errorf("appleads: read the answer: %w", err)
	}
	keep := func(o map[string]any) bool { return m.owns(o) == nil && m.ownsACL(o) }
	switch r := doc["result"].(type) {
	case map[string]any:
		if acls, ok := r["acls"].([]any); ok {
			r["acls"] = kept(acls, keep)
		} else if rows, ok := r["rows"].([]any); ok {
			r["rows"] = kept(rows, func(o map[string]any) bool {
				meta, _ := o["metadata"].(map[string]any)
				return meta == nil || keep(meta)
			})
		} else if err := m.owns(r); err != nil {
			return nil, err
		}
	case []any:
		doc["result"] = kept(r, keep)
	}
	return json.Marshal(doc)
}

// ownsACL keeps an ACL entry of the seat's ad accounts alone.
func (m made) ownsACL(o map[string]any) bool {
	acct, ok := o["adAccount"].(map[string]any)
	return !ok || slices.Contains(m.seat.AdAccounts, idText(acct["id"]))
}

func kept(list []any, keep func(map[string]any) bool) []any {
	out := make([]any, 0, len(list))
	for _, item := range list {
		if o, ok := item.(map[string]any); !ok || keep(o) {
			out = append(out, item)
		}
	}
	return out
}

// said is err as the agent is told it: a refusal as itself, Apple's own
// refusal in its words, the reserve and the quota as waits, anything else as
// the engine's fault. It never says a token, a secret or a key.
func said(err error) error {
	var r *growth.Refusal
	if errors.As(err, &r) {
		return r
	}
	var e *APIError
	switch {
	case errors.Is(err, errSent):
		return growth.Refuse(growth.RefusedConflict, strings.TrimPrefix(err.Error(), errSent.Error()+": ")+
			". What was made stands: read the targets again before calling again.")
	case errors.As(err, &e):
		kind := growth.RefusedInvalid
		switch e.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			kind = growth.RefusedForbidden
		case http.StatusNotFound:
			kind = growth.RefusedNotFound
		case http.StatusConflict:
			kind = growth.RefusedConflict
		case http.StatusTooManyRequests, 500, 502, 503, 504:
			kind = growth.RefusedUnavailable
		}
		return growth.Refuse(kind, e.Error())
	case errors.Is(err, ErrReserve):
		return growth.Refuse(growth.RefusedUnavailable, "this connection's calls for Apple's window are down to the ones "+
			"kept for approved acts; try again once the window resets.")
	case errors.Is(err, ErrQuota):
		return growth.Refuse(growth.RefusedUnavailable, "Apple Ads asked Crewlet to slow down; reads resume once its "+
			"window resets.")
	case errors.Is(err, ErrCredentials):
		return growth.Refuse(growth.RefusedUnavailable, "Apple Ads refused the connection's key: its public key was "+
			"removed or replaced, or the API user lost its role. A person makes a new key and uploads it. No call was made.")
	case errors.Is(err, ErrOtherAccount):
		return growth.Refuse(growth.RefusedForbidden, "the object is in another ad account than the one named, or a "+
			"budget order names an ad account outside this agent's. Nothing was sent.")
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
	Danger   bool     `json:"cannot_be_undone"`
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
const maxListed = 120

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
	out := OperationList{Operations: []OperationRow{}}
	for _, op := range operations() {
		classes := classesOf(op)
		lowest := op.Class
		var lowered []string
		for _, c := range classes[1:] {
			if c.Rank() < lowest.Rank() {
				lowest = c
			}
			if c != op.Class && !slices.Contains(lowered, string(c)) {
				lowered = append(lowered, string(c))
			}
		}
		switch {
		case resource != "" && !strings.EqualFold(resource, op.Resource):
			continue
		case class != "" && !slices.Contains(classes, growth.Class(class)):
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
			Roles: rolesNames(op.Roles), Tool: genericTool[op.Class], Level: string(lowest.Level()),
			Above: !level.Allows(lowest)})
	}
	return out, nil
}
