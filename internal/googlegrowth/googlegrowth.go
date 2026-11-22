// Package googlegrowth is what every Google growth product shares on top of the
// growth framework (internal/growth; ADR-0170, ADR-0171): a vendor request
// built from a catalog operation, sent as a seat's own Google growth identity
// (internal/googleauth, ADR-0172), held to its target's live state, and
// recorded on the access log. Each product keeps its catalog, its scope (the
// properties, sites or customers a seat names), its tool words and how it
// shapes an answer.
//
// # A request addresses its operation's resource and nothing else
//
// [NewRequest] fills an operation's `{name}` path parameters with IDs alone,
// so a call can never reach another resource than the one its operation
// names, and the host is the catalog's, so a seat's token only goes to Google.
//
// # A held call binds to what a person saw
//
// [Request.Digest] is the SHA-256 of the exact request and of its target's
// before-state. [Client.Apply] reads the target again and sends nothing where
// the digest moved, recording the outcome: applied, void, or failed with a
// closed reason.
//
// # Every act on the access log
//
// A call made at once is made, or failed with its reason; a call refused
// before Google is no act. A change's free words (a name, a description, an
// email) go as their digest and length, never as themselves ([RecordChange]).
//
// # Credentials Google returns
//
// A secret an answer carries (a Measurement Protocol secret, a channel token)
// is replaced before an agent reads it ([Client.Secrets], [Scrub]).
package googlegrowth

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/growth"
)

// Request is one vendor call as the engine will send it: the operation, its
// address with every path parameter filled, its query and body, and the
// account or property it acts in, as the access log names it.
type Request struct {
	Op      growth.Operation
	Address string
	Body    map[string]any
	Query   url.Values
	// Params are the path parameters the call gave, by name.
	Params map[string]string
	// Header is what the API asks beyond the engine's own headers: Google
	// Ads' login-customer-id, naming the manager an account is reached through.
	Header http.Header
	// Account is set by the product: "properties/123", a site, a customer;
	// "" where the call acts in none, and the log then names its target.
	Account string
}

// pathParam is one `{name}` of an operation's path.
var pathParam = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\{([A-Za-z]+)\}`) })

// idValue is an ID: letters, digits and `_ . : ~ -`.
var idValue = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_.:~-]{1,200}$`) })

// IDParam reports whether value is an ID, the shape most path parameters take.
func IDParam(_, value string) bool { return idValue().MatchString(value) }

// NewRequest builds op's request from a call's path parameters, query and
// body, every path parameter an ID ([IDParam]).
func NewRequest(op growth.Operation, path, query map[string]string, body map[string]any) (Request, error) {
	return NewRequestWith(op, path, query, body, IDParam)
}

// NewRequestWith is [NewRequest] with the product's own rule for what each
// path parameter may hold (a site's URL, a sitemap's). Every `{name}` must be
// given and is escaped into one path segment, and a value made only of dots is
// refused whatever the rule says, since a dot segment would address the
// resource above: a call can never reach another resource than its
// operation's.
func NewRequestWith(op growth.Operation, path, query map[string]string, body map[string]any,
	valid func(name, value string) bool) (Request, error) {
	var missing []string
	address := pathParam().ReplaceAllStringFunc(op.Path, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := path[name]
		if !ok || strings.Trim(v, ".") == "" || !valid(name, v) {
			missing = append(missing, name)
			return m
		}
		return url.PathEscape(v)
	})
	if len(missing) > 0 {
		return Request{}, growth.Refuse(growth.RefusedInvalid, fmt.Sprintf("%s needs these path parameters, "+
			"each in its own form: %s. No vendor was reached.", op.ID, strings.Join(missing, ", ")))
	}
	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}
	return Request{Op: op, Address: address, Body: body, Query: q, Params: path}, nil
}

// URL is the request's address with its query.
func (r Request) URL() string {
	if len(r.Query) == 0 {
		return r.Address
	}
	return r.Address + "?" + r.Query.Encode()
}

// Reads reports whether the request changes nothing.
func (r Request) Reads() bool { return r.Op.Class == growth.ClassRead }

// Resource is the address of what a write acts on, for its before-state: the
// path without its custom verb (`:archive`), or "" for a create, which acts on
// nothing that exists yet. The verb is read off the operation's template, since
// an ID may hold a colon itself.
func (r Request) Resource() string {
	verb := r.verb()
	if r.Op.Method == "POST" && verb == "" {
		return ""
	}
	return strings.TrimSuffix(r.Address, ":"+verb)
}

// verb is the operation's custom verb, "" where its path ends in a resource.
func (r Request) verb() string {
	last := r.Op.Path[strings.LastIndex(r.Op.Path, "/")+1:]
	i := strings.LastIndex(last, ":")
	if i < 0 || strings.Contains(last[i:], "}") {
		return ""
	}
	return last[i+1:]
}

// Changes is what the request changes on before, field by field: the fields
// its update mask names (every field the body sets for `*` or no mask).
func (r Request) Changes(before map[string]any) []growth.Change {
	var fields []string
	mask := strings.TrimSpace(r.Query.Get("updateMask"))
	if mask == "" || mask == "*" {
		for k := range r.Body {
			fields = append(fields, k)
		}
		sort.Strings(fields)
	} else {
		for _, f := range strings.Split(mask, ",") {
			if f = strings.TrimSpace(f); f != "" {
				fields = append(fields, f)
			}
		}
	}
	out := make([]growth.Change, 0, len(fields))
	for _, f := range fields {
		out = append(out, growth.Change{Field: f, Before: Lookup(before, f), After: Lookup(r.Body, f)})
	}
	return out
}

// Lookup is a dotted field's value in a JSON object, nil where it is absent.
func Lookup(m map[string]any, field string) any {
	var cur any = m
	for _, part := range strings.Split(field, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[part]
	}
	return cur
}

// Digest is the SHA-256 of the request as it will be sent and of the
// before-state it changes: what an approval binds to.
func (r Request) Digest(before []byte) string {
	body, _ := json.Marshal(r.Body)
	h := sha256.New()
	for _, part := range [][]byte{[]byte(r.Op.Method), []byte(r.URL()), body, before} {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Subject names what the request decides, for joining one seat's repeats:
// its operation, its address and the account it acts in, which an address
// alone does not always name (a site given in the body).
func (r Request) Subject(kind string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + r.Op.ID + "\x00" + r.Address + "\x00" + r.AccountOrTarget()))
	return hex.EncodeToString(sum[:])
}

// apiVersion is a path segment naming an API's version: v1, v1beta, v3alpha2.
var apiVersion = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^v[0-9]+([a-z]+[0-9]*)?$`) })

// Target is the resource a request names, as Google writes it: its path after
// the host and through the API's version, wherever that sits (`/v1beta/...`,
// `/webmasters/v3/...`).
func (r Request) Target() string {
	path := r.Address
	if u, err := url.Parse(r.Address); err == nil && u.Host != "" {
		path = u.EscapedPath()
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if i := slices.IndexFunc(segments, apiVersion().MatchString); i >= 0 && i < len(segments)-1 {
		segments = segments[i+1:]
	}
	path = strings.Join(segments, "/")
	if decoded, err := url.PathUnescape(path); err == nil {
		return decoded
	}
	return path
}

// AccountOrTarget is the account the access log names the request under.
func (r Request) AccountOrTarget() string {
	if r.Account != "" {
		return r.Account
	}
	return r.Target()
}

// Describe is what a request does, in a few words.
func (r Request) Describe() string {
	verb := cmp.Or(r.verb(), map[string]string{"POST": "create or run", "PATCH": "change", "PUT": "replace",
		"DELETE": "delete"}[r.Op.Method])
	return verb + " " + r.Op.Resource + " (" + r.Op.ID + ") on " + r.Target()
}

// withheld is what replaces a credential in an answer.
const withheld = "[withheld by Crewlet]"

// Scrub replaces every field named in secrets, at any depth.
func Scrub(v any, secrets []string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, inner := range t {
			if slices.Contains(secrets, k) {
				t[k] = withheld
				continue
			}
			t[k] = Scrub(inner, secrets)
		}
	case []any:
		for i := range t {
			t[i] = Scrub(t[i], secrets)
		}
	}
	return v
}

// Compact is JSON as one line, for an answer.
func Compact(raw []byte) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// Words is a JSON value as a person reads it: "(none)" for absent.
func Words(v any) string {
	if v == nil {
		return "(none)"
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
