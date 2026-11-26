package metaads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// Answer is a generic read's answer: Meta's document, fenced, since it holds
// names and ad text other people wrote at places no type declares.
type Answer struct {
	Operation string `json:"operation"`
	Status    int    `json:"status"`
	Account   string `json:"account"`
	// Token is the token slot the read went through.
	Token    string `json:"token"`
	Document string `json:"document" untrusted:"fence"`
	// Withheld counts the listed objects of ad accounts not this agent's,
	// left out of Document.
	Withheld int    `json:"withheld"`
	After    string `json:"after,omitempty"`
}

// Run makes a read tool's call; a write is prepared and applied ([Product.Prepare]).
func (p *Product) Run(ctx context.Context, call growth.Call) (any, error) {
	switch call.Tool {
	case ToolOperations:
		return p.listOperations(call)
	case ToolRead:
		return p.generic(ctx, call)
	case ToolAccounts:
		return p.accounts(ctx, call)
	case ToolCampaigns, ToolAdSets, ToolAds:
		return p.objects(ctx, call)
	case ToolInsights:
		return p.insights(ctx, call)
	}
	return nil, invalid("%s is not a read tool of Meta Ads.", call.Tool)
}

// generic makes a read of any read operation, answering only the seat's ad
// accounts' objects where its list can name others'.
func (p *Product) generic(ctx context.Context, call growth.Call) (any, error) {
	m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
	if err != nil {
		return nil, err
	}
	t := m.targets[0]
	rec := recordOf(call, m, growth.ClassRead, "")
	tok, err := p.tokenFor(ctx, m.conn, m.own, growth.ClassRead, t.row.op.Permissions)
	if err != nil {
		return nil, err
	}
	rec.Via, rec.Signer = tok.signed()
	filter := filtered(t.row)
	params := url.Values{}
	for k, v := range t.params {
		params.Set(k, formValue(v))
	}
	if filter {
		// THE ACCOUNT EACH LISTED OBJECT NAMES is read with it, so the list is
		// held to the seat's ad accounts.
		switch fields := params.Get("fields"); {
		case fields == "":
			params.Set("fields", "id,name,account_id")
		case !slices.Contains(strings.Split(fields, ","), "account_id"):
			params.Set("fields", fields+",account_id")
		}
	}
	resp, err := m.conn.client.Do(ctx, tok.valueNow(), Request{Method: t.row.op.Method, Path: t.path, Params: params,
		Object: objectOf(m.account), Read: true})
	if err != nil {
		var refused *growth.Refusal
		if !errors.As(err, &refused) {
			rec.Failure = failureOf(err)
			p.note(ctx, growth.StepFailed, rec)
		}
		return nil, said(err)
	}
	p.note(ctx, growth.StepMade, rec)
	body, withheld := resp.Body, 0
	if filter {
		body, withheld = onlySeats(body, seatAccounts(m.seat), t.row.returns)
	}
	return Answer{Operation: t.row.op.ID, Status: resp.Status, Account: m.account, Token: tok.slot,
		Document: scrub(m.conn.client, body), Withheld: withheld, After: afterOf(body)}, nil
}

// filtered reports whether r's answer can list another ad account's objects:
// a list of ad accounts or of an account's objects.
func filtered(r row) bool {
	return r.op.Method == "GET" && r.returns != "" && accountOwned[r.returns] && strings.Count(r.op.Path, "/") > 1
}

// onlySeats is a list with every object of an ad account not in allowed left
// out, and how many were; an object naming no ad account is left out too.
func onlySeats(body []byte, allowed []string, returns string) ([]byte, int) {
	var doc map[string]any
	if decode(body, &doc) != nil {
		return []byte(`{"data": []}`), 0
	}
	data, ok := doc["data"].([]any)
	if !ok {
		return body, 0
	}
	kept := make([]any, 0, len(data))
	for _, d := range data {
		obj, _ := d.(map[string]any)
		account := ""
		if returns == "AdAccount" {
			account, _ = obj["id"].(string)
		}
		if account == "" {
			raw, _ := obj["account_id"].(string)
			account = config.MetaAdAccountID(raw)
		}
		if slices.Contains(allowed, account) {
			kept = append(kept, d)
		}
	}
	doc["data"] = kept
	out, err := json.Marshal(doc)
	if err != nil {
		return []byte(`{"data": []}`), len(data)
	}
	return out, len(data) - len(kept)
}

// afterOf is a list's after cursor, "" for none.
func afterOf(body []byte) string {
	var doc struct {
		Paging struct {
			Cursors struct {
				After string `json:"after"`
			} `json:"cursors"`
			Next string `json:"next"`
		} `json:"paging"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Paging.Next == "" {
		return ""
	}
	return doc.Paging.Cursors.After
}

// OperationRow is one operation as the discovery tool lists it.
type OperationRow struct {
	ID          string   `json:"operation"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Resource    string   `json:"resource"`
	Purpose     string   `json:"purpose"`
	Class       string   `json:"class"`
	Lowered     []string `json:"lowered_to"`
	Fields      []string `json:"field_rules"`
	Danger      bool     `json:"cannot_be_undone"`
	Permissions []string `json:"permissions"`
	Tool        string   `json:"tool"`
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
	out := OperationList{Operations: []OperationRow{}}
	for _, r := range table() {
		op := r.op
		if op.Class == growth.ClassNever {
			continue
		}
		cls := classesOf(op)
		lowest := op.Class
		for _, c := range cls {
			if c.Rank() < lowest.Rank() {
				lowest = c
			}
		}
		switch {
		case resource != "" && !strings.EqualFold(resource, op.Resource):
			continue
		case class != "" && !slices.Contains(cls, growth.Class(class)):
			continue
		case text != "" && !strings.Contains(strings.ToLower(op.ID+" "+op.Path+" "+r.purpose), strings.ToLower(text)):
			continue
		}
		out.Total++
		if len(out.Operations) >= maxListed {
			out.Truncated = true
			continue
		}
		row := OperationRow{ID: op.ID, Method: op.Method, Path: op.Path, Resource: op.Resource, Purpose: r.purpose,
			Class: string(op.Class), Danger: op.Danger, Permissions: op.Permissions, Tool: genericTool[op.Class],
			Level: string(lowest.Level()), Above: !level.Allows(lowest),
			Lowered: []string{}, Fields: []string{}}
		for _, s := range op.States {
			row.Lowered = append(row.Lowered, string(s.Class)+" when "+s.When)
		}
		for _, f := range op.Fields {
			row.Fields = append(row.Fields, fieldWords(f))
		}
		out.Operations = append(out.Operations, row)
	}
	return out, nil
}

// fieldWords is a field rule in a line an agent reads.
func fieldWords(f growth.FieldRule) string {
	s := f.Field + ": " + string(f.Class)
	if f.Lower != "" {
		s += "; lowered: " + string(f.Lower)
	}
	if len(f.Pausing) > 0 {
		// A pause is a JSON value: a state quoted, a switch's flag bare.
		pauses := make([]words.Said, len(f.Pausing))
		for i, p := range f.Pausing {
			pauses[i] = words.Said(fmt.Sprint(p))
			if state, ok := p.(string); ok {
				pauses[i] = words.Said(strconv.Quote(state))
			}
		}
		s += "; " + words.Or(pauses) + ": operate"
	}
	for _, v := range f.Values {
		// A cleared field is no value, so it is said as a word, not quoted.
		vals := make([]words.Said, len(v.Values))
		for i, x := range v.Values {
			vals[i] = words.Said(strconv.Quote(x))
			if x == "" {
				vals[i] = "cleared"
			}
		}
		s += "; " + words.Or(vals) + ": " + string(v.Class)
	}
	for _, w := range f.While {
		s += "; while " + w.When + ": " + string(w.Class)
	}
	return s
}
