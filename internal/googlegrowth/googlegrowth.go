// Package googlegrowth is what every Google growth product shares on top of the
// growth framework (internal/growth; ADR-0170, ADR-0171): a vendor request
// built from a catalog operation, sent as a seat's own Google growth identity
// (internal/googleauth, ADR-0172), held to its target's live state, and
// recorded on the access log. Each product keeps its catalog, its scope (the
// properties, sites or customers a seat names), its tool words and how it
// shapes an answer.
//
// # A consent product sends as the company's grant
//
// Google refuses service accounts at AdSense and AdMob, so their calls go as
// the company's consent grant ([Client.Grant]): one token every seat shares,
// renewed once where Google answers 401, on the same call path as a seat's
// ([googleauth.Transport]). Nothing at Google then tells two seats apart, so
// the product holds each seat to its level and scope alone.
//
// # A seat's own identity at the product
//
// A seat's identity at Analytics, Ads, Search Console and Play is its Google
// growth identity, its own by construction (ADR-0172), so the lane never
// takes the company's. What a person still does is add it at the product, at
// the role the seat's level needs ([Identity.Needs]); Play and Analytics can
// add it themselves with an admin's sign-in, a consent grant that signs no
// seat's call ([Admin]). The identity is standing ([seataccess.IdentitySpec.Standing]): every pass
// checks it, as the identity, reaching each entry the seat names and reading
// its role where the product shows it (to the identity at Search Console and
// Ads, to the admin at Play and Analytics), and keeps it exactly while it
// passes, so the seat Needs you until it does. A role the product shows to
// nobody Crewlet signs in as is read from the first call it refuses.
//
// # A request addresses its operation's resource and nothing else
//
// [NewRequest] fills an operation's `{name}` path parameters with IDs alone
// and refuses one its path does not name, so a call can never reach another
// resource than the one its operation names, and the host is the catalog's,
// so a seat's token only goes to Google.
//
// # A write is classed as Google reads it
//
// Google takes a field by its lowerCamel JSON name or its proto name, in the
// body and in the update mask (`updateMask` or `update_mask`), so
// [Request.Changes] reads a field's value in either spelling and
// [Held.Class] classes it by the lowerCamel name a catalog's rules use, while
// a card and a record show the request as written; a body naming one field in
// two spellings with two values is read as neither value, which no rule
// lowers. A `*` mask replaces the whole resource, so it changes every field
// the target holds as well as the body's.
//
// # A held call binds to what a person saw
//
// [Request.Digest] is the SHA-256 of the exact request and of its target's
// before-state. [Client.Apply] records the administrator's answer first and
// acts only on the act's one answer; then, under the account's apply lease
// and within growth.ApplyBound, it reads the target again and sends nothing
// where the digest moved or the product's last check refuses it (a spend's
// caps), recording the outcome: applied, void, or failed with a closed
// reason. A write that went unanswered may have reached Google, so its
// outcome is unknown, never "unavailable" ([FailureOf]). A failure before the
// change left (the lease, the read again, the last check, its context's end)
// changed nothing: it answers [growth.ErrNotSent], and an answer lost there
// is recorded timed out at a deadline and unavailable otherwise, never
// unknown.
//
// A call that says a change to its target rather than the whole of it (an
// owner added to a list Google takes whole) is made from the before-state the
// binding holds ([Request.Shape]): read once, under the lease, so no other
// change is overwritten, and a state the change no longer applies to is a
// target that moved, void rather than failed.
//
// # Every act on the access log
//
// A call made at once is made, or failed with its reason; a call refused
// before Google is no act. A read records no change. A change to a field whose
// name says it holds somebody's words (a name, a description, an email, a
// URI), or whose value is structured, goes as its digest and length, never as
// itself; a field the log may not name is left out and counted
// ([RecordChange]).
//
// # Credentials
//
// A secret an answer carries (a Measurement Protocol secret, a channel token)
// is replaced before an agent reads it ([Client.Secrets], [Scrub]). A secret a
// request carries is filled into its query only as it is sent
// ([Request.Fill]), so no digest, card or record holds it, and a failed
// request's error names its path alone (internal/googleauth).
package googlegrowth

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

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
	// Fill makes the query sent from Query, adding a credential only as the
	// request is sent; nil sends Query. It reads nothing it changes.
	Fill func(ctx context.Context, q url.Values) (url.Values, error)
	// Diff, where set, is the product's reading of what the request changes
	// on its target: for an update mask that is not the body's field names
	// (a map entry's key, a snake_case path). Nil reads the mask ([Request.Changes]).
	Diff func(before map[string]any) []growth.Change
	// Raw marks a method answering google.api.HttpBody (a CSV report): its
	// answer is {"contentType", "data"}, data the bytes Google sent as text.
	Raw bool
	// Shape makes the request sent from its target's before-state, where the
	// call says a change to the target rather than the whole of it (owners
	// added and removed, sent as the whole list), so the change is built on
	// the state its approval binds; nil sends the request as built. A refusal
	// is a state the change does not apply to.
	Shape func(before []byte) (Request, error)
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
// resource above; a parameter the path does not name is refused, since a
// product reads where a call acts from its parameters. A refused request
// keeps its operation and body, for the record of it.
func NewRequestWith(op growth.Operation, path, query map[string]string, body map[string]any,
	valid func(name, value string) bool) (Request, error) {
	var missing []string
	named := map[string]bool{}
	address := pathParam().ReplaceAllStringFunc(op.Path, func(m string) string {
		name := m[1 : len(m)-1]
		named[name] = true
		v, ok := path[name]
		if !ok || strings.Trim(v, ".") == "" || !valid(name, v) {
			missing = append(missing, name)
			return m
		}
		return url.PathEscape(v)
	})
	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}
	refused := Request{Op: op, Body: body, Query: q}
	if len(missing) > 0 {
		return refused, growth.Refuse(growth.RefusedInvalid, fmt.Sprintf("%s needs these path parameters, "+
			"each in its own form: %s. No vendor was reached.", op.ID, strings.Join(missing, ", ")))
	}
	var stray []string
	for name := range path {
		if !named[name] {
			stray = append(stray, name)
		}
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		return refused, growth.Refuse(growth.RefusedInvalid, fmt.Sprintf("%s's path names no %s; give only "+
			"the path parameters its path names. No vendor was reached.", op.ID, strings.Join(stray, ", ")))
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
// path without its custom verb (`:archive`), or "" for a create and for a
// custom verb on a collection (`properties:createRollupProperty`), which act
// on nothing that one read returns. The verb is read off the operation's
// template, since an ID may hold a colon itself.
func (r Request) Resource() string {
	verb := r.verb()
	onCollection := verb != "" && !strings.HasSuffix(strings.TrimSuffix(r.Op.Path, ":"+verb), "}")
	if r.Op.Method == "POST" && (verb == "" || onCollection) {
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

// Changes is what the request changes on before, field by field, each named
// and valued as the request writes it, so a card shows what Google receives:
// the fields its update masks name, every field the body sets where it names
// none, and for a `*` mask those and every field before holds; or its
// product's own reading ([Request.Diff]). Values are read in either spelling
// Google takes (package doc), and [Held.Class] classes each field by its
// lowerCamel name.
func (r Request) Changes(before map[string]any) []growth.Change {
	if r.Diff != nil {
		return r.Diff(before)
	}
	fields, masked := r.masked()
	if !masked || slices.Contains(fields, "*") {
		fields = slices.Collect(maps.Keys(r.Body))
		if masked {
			// A `*` MASK REPLACES THE RESOURCE: a field the body leaves out is cleared.
			named := map[string]bool{}
			for _, f := range fields {
				named[LowerCamel(f)] = true
			}
			for k := range before {
				if !named[LowerCamel(k)] {
					fields = append(fields, k)
				}
			}
		}
	}
	sort.Strings(fields)
	fields = slices.Compact(fields)
	out := make([]growth.Change, 0, len(fields))
	for _, f := range fields {
		out = append(out, growth.Change{Field: f, Before: Lookup(before, f), After: Lookup(r.Body, f)})
	}
	return out
}

// masked is every path the request's update masks name, as written, and
// whether it names any: Google reads `updateMask` and `update_mask` alike.
func (r Request) masked() ([]string, bool) {
	var out []string
	for key, values := range r.Query {
		if k := LowerCamel(key); k != "updateMask" && !strings.HasPrefix(k, "updateMask.") {
			continue
		}
		for _, v := range values {
			for _, f := range strings.Split(v, ",") {
				if f = strings.TrimSpace(f); f != "" {
					out = append(out, f)
				}
			}
		}
	}
	return out, len(out) > 0
}

// Field is a dotted field's value in the request's body, each key read in
// either spelling Google takes (package doc), nil where it is absent.
func (r Request) Field(field string) any { return Lookup(r.Body, field) }

// LowerCamel is a dotted field path as Google's lowerCamel JSON names it:
// `daily_export_enabled` is `dailyExportEnabled`, and a lowerCamel path is
// itself.
func LowerCamel(path string) string {
	parts := strings.Split(path, ".")
	for i, part := range parts {
		var b strings.Builder
		upper := false
		for _, c := range part {
			switch {
			case c == '_' && b.Len() > 0:
				upper = true
			case upper:
				b.WriteRune(unicode.ToUpper(c))
				upper = false
			default:
				b.WriteRune(c)
			}
		}
		parts[i] = b.String()
	}
	return strings.Join(parts, ".")
}

// conflict is a field a body names in two spellings with two values. It is no
// string, number, boolean or list, so no rule reads it as a pause or a lower.
type conflict struct{}

// MarshalJSON says the field is named twice, for a card and a record.
func (conflict) MarshalJSON() ([]byte, error) { return []byte(`"(named twice, with two values)"`), nil }

// Lookup is a dotted field's value in a JSON object, each key on its path
// matched by its lowerCamel name and its value returned as written: a key
// named in two spellings with two values is a [conflict], and the keys inside
// the value (a Measurement Protocol event's own) are left as they are.
func Lookup(m map[string]any, field string) any {
	var cur any = m
	for _, part := range strings.Split(LowerCamel(field), ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		var found any
		seen := false
		for _, k := range slices.Sorted(maps.Keys(obj)) {
			if LowerCamel(k) != part {
				continue
			}
			if seen && !reflect.DeepEqual(found, obj[k]) {
				return conflict{}
			}
			found, seen = obj[k], true
		}
		cur = found
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
