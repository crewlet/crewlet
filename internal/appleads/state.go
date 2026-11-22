package appleads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
)

// A write is classed against its target as it reads just before the call: an
// update by the fields it changes ([growth.Classify]), a creative or location
// group by whether anything uses it. A state the engine cannot read keeps the
// stricter class (growth.StateRule).

// before is a target as it reads now: its object, nil for one a POST makes,
// and the digest of what the approval binds.
type before struct {
	attrs  map[string]any
	digest string
}

// decode reads a JSON answer exactly: numbers stay their digits.
func decode(body []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	return dec.Decode(into)
}

// reads reports whether t's target is read before it is written: an update or
// a delete of one object.
func reads(t target) bool {
	_, id := typeAndID(t.path)
	return id != "" && (t.op.Method == http.MethodPut || t.op.Method == http.MethodDelete)
}

// beforeOf reads t's target through m's connection, held to m's ad account,
// and digests what an approval binds: every field the body sets, the target's
// status, deletion, last change and ad accounts. A POST binds its request
// alone: what it makes does not exist yet.
func (p *Product) beforeOf(ctx context.Context, m made, t target) (before, error) {
	if !reads(t) {
		data, _ := json.Marshal(map[string]any{"path": t.path, "account": m.account})
		return before{digest: hexSum(data)}, nil
	}
	resp, err := m.conn.client.Do(ctx, Request{Method: http.MethodGet, Path: t.path, Account: m.account, Read: true,
		Reserved: true})
	if err != nil {
		return before{}, err
	}
	var doc struct {
		Result map[string]any `json:"result"`
	}
	if err := decode(resp.Body, &doc); err != nil || doc.Result == nil {
		return before{}, fmt.Errorf("appleads: read %s: the answer holds no object", t.path)
	}
	if err := m.owns(doc.Result); err != nil {
		return before{}, err
	}
	bound := map[string]any{}
	for path := range flatten(t.body) {
		bound[path] = lookup(doc.Result, path)
	}
	for _, k := range []string{"status", "deleted", "modificationTime", "adAccountIds", "name"} {
		if v, ok := doc.Result[k]; ok {
			bound[k] = v
		}
	}
	data, _ := json.Marshal(map[string]any{"path": t.path, "bound": bound})
	return before{attrs: doc.Result, digest: hexSum(data)}, nil
}

// ErrOtherAccount is an object of an ad account the call is not placed in:
// whether Apple refuses one under another account's X-AP-Context is not
// documented, so the engine checks the answer itself.
var ErrOtherAccount = errors.New("appleads: the object is in another ad account")

// owns refuses an object of another ad account than m's, and a budget order
// naming an ad account outside the seat's.
func (m made) owns(obj map[string]any) error {
	if m.account != "" {
		if v, ok := obj["adAccountId"]; ok && idText(v) != m.account {
			return fmt.Errorf("%w: %v, not %s", ErrOtherAccount, v, m.account)
		}
	}
	if ids, ok := obj["adAccountIds"].([]any); ok {
		for _, id := range ids {
			if !slices.Contains(m.seat.AdAccounts, idText(id)) {
				return fmt.Errorf("%w: the budget order names ad account %v, which is not this agent's", ErrOtherAccount, id)
			}
		}
	}
	return nil
}

// flatten is v's leaves by dotted path: objects are walked, a list or a value
// is a leaf, as Apple replaces a list whole.
func flatten(v any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		obj, ok := v.(map[string]any)
		if !ok || len(obj) == 0 {
			if prefix != "" {
				out[prefix] = v
			}
			return
		}
		for k, child := range obj {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			walk(key, child)
		}
	}
	walk("", v)
	return out
}

// lookup is the value at a dotted path, nil for none.
func lookup(obj map[string]any, path string) any {
	var cur any = obj
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// same reports whether two values Apple and an agent wrote are one value:
// numbers by their value, "10.00" and 10 alike, everything else by its JSON.
func same(a, b any) bool {
	ra, okA := rat(a)
	rb, okB := rat(b)
	if okA && okB {
		return ra.Cmp(rb) == 0
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// idText is an ID as Apple and an agent may write it, a JSON number or a
// string, in its digits: a decoded float64 never prints in exponent form.
func idText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case string:
		return t
	}
	return fmt.Sprint(v)
}

func rat(v any) (*big.Rat, bool) {
	var s string
	switch t := v.(type) {
	case json.Number:
		s = t.String()
	case float64:
		s = fmt.Sprint(t)
	case string:
		s = t
	default:
		return nil, false
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	return r, ok
}

// changesOf are the fields t's body changes on its target, in path order.
func changesOf(t target, b before) []growth.Change {
	leaves := flatten(t.body)
	paths := make([]string, 0, len(leaves))
	for path := range leaves {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var out []growth.Change
	for _, path := range paths {
		was := lookup(b.attrs, path)
		if b.attrs != nil && same(was, leaves[path]) {
			continue
		}
		out = append(out, growth.Change{Field: path, Before: was, After: leaves[path]})
	}
	return out
}

// classOf is the class t comes to against its target as it reads now; read
// for an update that changes nothing, which is not sent.
func (p *Product) classOf(ctx context.Context, m made, t target, b before) growth.Class {
	switch {
	case len(t.op.Fields) > 0:
		changes := changesOf(t, b)
		if len(changes) == 0 && b.attrs != nil {
			return growth.ClassRead
		}
		return growth.Classify(t.op, changes)
	case len(t.op.States) > 0:
		for _, rule := range t.op.States {
			if holds, err := p.inState(ctx, m, t, rule.When); err == nil && holds {
				return rule.Class
			}
		}
	}
	return t.op.Class
}

// maxStatePages bounds the pages one state read walks: 20 pages of 1,000 is
// 20,000 ads or ad groups, past which the state is unread and the stricter
// class kept.
const maxStatePages = 20

// errUnread is a state the engine could not read within its bounds.
var errUnread = errors.New("appleads: the state could not be read within its bounds")

// inState reports whether t's target is in state.
func (p *Product) inState(ctx context.Context, m made, t target, state string) (bool, error) {
	if state != stateUnused {
		return false, nil
	}
	_, id := typeAndID(t.path)
	switch t.op.ID {
	case "creativesPost":
		// A CREATIVE BEING MADE is one no ad serves yet.
		return true, nil
	case "creativesIdPut":
		used, err := p.anyOf(ctx, m, "/v1/ads/query", func(o map[string]any) bool { return idText(o["creativeId"]) == id })
		return !used, err
	case "updateLocationGroup":
		used, err := p.anyOf(ctx, m, "/v1/adgroups/query", func(o map[string]any) bool {
			include, _ := lookup(o, "targeting.locationGroup.include").([]any)
			return slices.ContainsFunc(include, func(v any) bool { return idText(v) == id })
		})
		return !used, err
	}
	return false, nil
}

// anyOf reports whether any object a query lists holds match.
func (p *Product) anyOf(ctx context.Context, m made, path string, match func(map[string]any) bool) (bool, error) {
	const pageSize = 1000
	for page := range maxStatePages {
		resp, err := m.conn.client.Do(ctx, Request{Method: http.MethodPost, Path: path, Account: m.account, Read: true,
			Reserved: true, Body: map[string]any{"pagination": map[string]any{"offset": page * pageSize,
				"pageSize": pageSize}}})
		if err != nil {
			return false, err
		}
		var doc struct {
			Result []map[string]any `json:"result"`
		}
		if err := decode(resp.Body, &doc); err != nil {
			return false, fmt.Errorf("appleads: read %s: %w", path, err)
		}
		if slices.ContainsFunc(doc.Result, match) {
			return true, nil
		}
		if len(doc.Result) < pageSize {
			return false, nil
		}
	}
	return false, errUnread
}
