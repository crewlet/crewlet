package catalog

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/exec"
)

// at is a request on route, "METHOD /pattern", with its wildcards filled from
// path: each value one path segment, escaped, so a value can never name a
// second segment or another route.
func at(route string, path map[string]string, query url.Values) (exec.Request, error) {
	method, pattern, ok := strings.Cut(route, " ")
	if !ok {
		return exec.Request{}, fmt.Errorf("catalog: %q is not METHOD /pattern", route)
	}
	segments := strings.Split(pattern, "/")
	for i, segment := range segments {
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}"), "...")
		value := strings.TrimSpace(path[name])
		if value == "" {
			return exec.Request{}, fmt.Errorf("catalog: %s needs %s", route, name)
		}
		segments[i] = url.PathEscape(value)
	}
	req := exec.Request{Method: method, Path: strings.Join(segments, "/")}
	if len(query) > 0 {
		req.Query = query
	}
	return req, nil
}

// get is a read on route.
func get(route string, path map[string]string, query url.Values) (exec.Request, error) {
	return at(route, path, query)
}

// params is a query string built from the call's arguments: each named
// argument, when given, under its query key.
type params struct {
	args Args
	q    url.Values
}

func query(args Args) *params { return &params{args: args, q: url.Values{}} }

// str carries a string argument under key.
func (p *params) str(arg, key string) *params {
	if v := p.args.String(arg); v != "" {
		p.q.Set(key, v)
	}
	return p
}

// num carries a whole-number argument under key, or fallback when the call
// gave none (zero carries nothing).
func (p *params) num(arg, key string, fallback int) *params {
	n, ok := p.args.Int(arg)
	if !ok {
		n = fallback
	}
	if n > 0 {
		p.q.Set(key, strconv.Itoa(n))
	}
	return p
}

// truth carries a boolean argument under key as `true`, when set.
func (p *params) truth(arg, key string) *params {
	if p.args.Bool(arg) {
		p.q.Set(key, "true")
	}
	return p
}

// list carries a list argument under key, comma separated.
func (p *params) list(arg, key string) *params {
	if v := p.args.Strings(arg); len(v) > 0 {
		p.q.Set(key, strings.Join(v, ","))
	}
	return p
}

// fixed carries a constant.
func (p *params) fixed(key, value string) *params {
	p.q.Set(key, value)
	return p
}

func (p *params) values() url.Values { return p.q }

// act is the request an approved write executes as: the act route's one
// tool, with the call's arguments as its `args`. THE SAME NAMES: every write
// tool's arguments are the act tool's own, so what the person approves and
// what the route reads are spelled one way.
func act(tool string, args Args) (exec.Request, error) {
	body, err := json.Marshal(map[string]any{"args": map[string]any(args)})
	if err != nil {
		return exec.Request{}, fmt.Errorf("catalog: encode the %s request: %w", tool, err)
	}
	req, err := at(ActRoute, map[string]string{"tool": tool}, nil)
	if err != nil {
		return exec.Request{}, err
	}
	req.Body = body
	return req, nil
}

// withActArg is an act request's body with one argument more: one its preview
// fills from what it read rather than one the model writes, so the model's
// schema for the tool never offers it and the call the model made is checked
// against that schema before this is added. The body is [act]'s own shape,
// `{"args": {...}}`; anything else is a request this was not built for.
func withActArg(body []byte, key string, value any) ([]byte, error) {
	var call struct {
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal(body, &call); err != nil || call.Args == nil {
		return nil, fmt.Errorf("catalog: add %s to an act request that sends no args", key)
	}
	call.Args[key] = value
	encoded, err := json.Marshal(map[string]any{"args": call.Args})
	if err != nil {
		return nil, fmt.Errorf("catalog: encode the request with %s: %w", key, err)
	}
	return encoded, nil
}

// ActRoute is the act transport's route (internal/api/operator's
// ActPattern), the one every approved operator-catalogue write is sent to.
const ActRoute = "POST /operator/act/{tool}"
