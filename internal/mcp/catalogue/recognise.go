package catalogue

import (
	"net/url"
	"path"
	"slices"
	"strings"
)

// Launch is what recognition reads of a configured server: its transport and
// its address or its command. Never a credential.
type Launch struct {
	Transport Transport
	URL       string
	Command   string
	Args      []string
}

// SeatKey is one seat credential a recognised per-seat server reads, and the
// key it reads it under on each transport.
type SeatKey struct {
	// Field is which of the integration's seat credentials: TOKEN or
	// EMAIL for Atlassian, the integration's own field name otherwise.
	Field string
	// Env is the variable a local server reads it under, Header the header
	// a remote one does, and Prefix what precedes the value there
	// ("Bearer "). An empty key is one that transport does not read.
	Env    string
	Header string
	Prefix string
}

// Binding is a recognised server's declaration of the seat credential it
// reads: which integration's, under which keys (D15).
type Binding struct {
	// Vendor is the integration whose seat credential it reads.
	Vendor string
	// Server is the name such a server is conventionally declared under,
	// the curated entry's suggested name.
	Server string
	Keys   []SeatKey
}

// recognition is one way a configured server is recognised as an entry, and
// the seat credential a server recognised that way reads (none for a server
// that acts as no seat).
type recognition struct {
	match func(Launch) bool
	keys  []SeatKey
}

// Recognise is the curated entry a configured server is, by its address or
// its command, or false where it is none this build knows. The first entry
// whose recognition matches, in table order, answers.
func Recognise(l Launch) (Entry, bool) {
	e, _, ok := recognise(l)
	if !ok {
		return Entry{}, false
	}
	return e.clone(), true
}

// SeatKeysFor is the seat credential a recognised per-seat server reads: the
// integration and the keys, or false where this build recognises the server
// as reading none. The caller decides whether the server acts as a seat at
// all (a shared one reads no seat's credential).
func SeatKeysFor(l Launch) (string, []SeatKey, bool) {
	e, r, ok := recognise(l)
	if !ok || len(r.keys) == 0 {
		return "", nil, false
	}
	return e.Integration, slices.Clone(r.keys), true
}

// BindingFor is the conventional recognised server for an integration: the
// first recognition in table order that binds its seat credential, or false
// where this build recognises none for it. A company binds any other server
// by declaring its `seat_credentials`.
func BindingFor(integration string) (Binding, bool) {
	for _, e := range curated() {
		if e.Integration != integration {
			continue
		}
		for _, r := range e.recognise {
			if len(r.keys) > 0 {
				return Binding{Vendor: e.Integration, Server: e.Name, Keys: slices.Clone(r.keys)}, true
			}
		}
	}
	return Binding{}, false
}

// Bindings is every binding this build recognises, in table order: the
// conventional one per integration first.
func Bindings() []Binding {
	var out []Binding
	for _, e := range curated() {
		for _, r := range e.recognise {
			if len(r.keys) > 0 {
				out = append(out, Binding{Vendor: e.Integration, Server: e.Name, Keys: slices.Clone(r.keys)})
			}
		}
	}
	return out
}

func recognise(l Launch) (*Entry, *recognition, bool) {
	table := curated()
	for i := range table {
		for j := range table[i].recognise {
			if table[i].recognise[j].match(l) {
				return &table[i], &table[i].recognise[j], true
			}
		}
	}
	return nil, nil, false
}

// launches recognises a local server whose command or one of its arguments
// names one of the packages (`npx -y @korotovsky/slack-mcp-server`, a
// container image).
func launches(packages ...string) func(Launch) bool {
	return func(l Launch) bool {
		if l.Transport != TransportStdio {
			return false
		}
		for _, word := range append([]string{l.Command}, l.Args...) {
			for _, pkg := range packages {
				if strings.Contains(word, pkg) {
					return true
				}
			}
		}
		return false
	}
}

// glabServe recognises GitLab's own CLI serving MCP: `glab mcp serve`.
func glabServe(l Launch) bool {
	return l.Transport == TransportStdio && path.Base(l.Command) == "glab" && slices.Contains(l.Args, "mcp")
}

// hostIs recognises a remote server at exactly host.
func hostIs(host string) func(Launch) bool {
	return func(l Launch) bool { return l.Transport == TransportHTTP && HostOf(l.URL) == host }
}

// hostUnder recognises a remote server at one of the domains or a host
// beneath one (`mcp.us5.datadoghq.com` under `datadoghq.com`).
//
// WHOLE LABELS, NEVER A SUBSTRING: a seat's credential is handed to whatever
// server this recognises, so `datadoghq.com.example.net` and
// `evil-datadoghq.com` must not be.
func hostUnder(domains ...string) func(Launch) bool {
	return func(l Launch) bool {
		if l.Transport != TransportHTTP {
			return false
		}
		host := HostOf(l.URL)
		return slices.ContainsFunc(domains, func(d string) bool {
			return host == d || strings.HasSuffix(host, "."+d)
		})
	}
}

// either recognises what a or b does.
func either(a, b func(Launch) bool) func(Launch) bool {
	return func(l Launch) bool { return a(l) || b(l) }
}

// HostOf is the lowercased host of an address, "" for one that does not
// parse: a url that does not parse names no host this build recognises.
func HostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
