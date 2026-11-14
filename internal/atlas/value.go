package atlas

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/words"
)

// ValueType is the grammar one fact's value is written in. A closed set, and
// what it leaves out is the point: there is no type for a connection string, a
// command line, a file body, an environment value or a vendor's raw answer, so
// none of them has anywhere to go.
type ValueType string

// The value types.
const (
	TypeWord       ValueType = "word"
	TypeInt        ValueType = "int"
	TypePort       ValueType = "port"
	TypeHostname   ValueType = "hostname"
	TypeIP         ValueType = "ip"
	TypeAddress    ValueType = "address"
	TypeURL        ValueType = "url"
	TypeVersion    ValueType = "version"
	TypeRegion     ValueType = "region"
	TypeAccountRef ValueType = "account_ref"
	TypeBranch     ValueType = "branch"
	TypeLabel      ValueType = "label"
	TypeInstant    ValueType = "instant"
	// TypeImage is a container image's reference: its repository and, where
	// it names one, its tag or its digest.
	TypeImage ValueType = "image"
)

// ValueTypes is every value type.
var ValueTypes = []ValueType{
	TypeWord, TypeInt, TypePort, TypeHostname, TypeIP, TypeAddress, TypeURL,
	TypeVersion, TypeRegion, TypeAccountRef, TypeBranch, TypeLabel, TypeInstant, TypeImage,
}

// Valid reports whether t is a member of the closed set.
func (t ValueType) Valid() bool { return slices.Contains(ValueTypes, t) }

// The bounds of the free-standing grammars, each a name rather than a sentence.
const (
	// MaxLabelRunes bounds a label: a name, never a description. Prose lives
	// in Doclet, which an entry links with documented_in.
	MaxLabelRunes = 120
	// MaxURLBytes bounds an address; an address longer than this is carrying
	// something an address does not need.
	MaxURLBytes = 512
	// MaxImageBytes bounds an image reference, the limit registries hold a
	// repository's name to.
	MaxImageBytes = 255
	// MaxInt bounds a count: far past any real inventory, short of overflow
	// in any reader.
	MaxInt = 1_000_000_000
)

// ErrInvalid is a value its type's grammar does not admit. Its message names
// the rule and never echoes the value, which may be a secret somebody pasted.
var ErrInvalid = errors.New("atlas: invalid value")

// ErrCredential is a value shaped like a credential. It is REFUSED rather than
// masked and kept, because a masked value is a value nobody meant to record.
var ErrCredential = errors.New("atlas: a value shaped like a credential is never recorded")

// Value is one fact's value in its canonical text. Private is the engine's
// reading of an address, a host name or a URL's host: on a network the public
// internet does not route.
type Value struct {
	Type    ValueType `json:"type"`
	Text    string    `json:"text" untrusted:"name"`
	Private bool      `json:"private,omitempty"`
}

// Equal reports whether two values are one value.
func (v Value) Equal(o Value) bool { return v.Type == o.Type && v.Text == o.Text }

// The grammars, compiled at first use rather than at init: Gitlet's push hook
// is this binary and pays every linked package's init on every push.
var (
	versionRE    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,31}$`) })
	regionRE     = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`) })
	accountRefRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z0-9]([a-z0-9._:/-]{0,126}[a-z0-9])?$`)
	})
	// imageRE is an image reference as a registry writes one: an optional
	// registry host and port, a lowercase path, then a tag, a digest or both.
	imageRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]([a-z0-9._-]*[a-z0-9])?)*` +
			`(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?(@sha256:[a-f0-9]{64})?$`)
	})
)

// urlSchemes is every scheme an endpoint may be addressed with.
var urlSchemes = []string{"http", "https", "ws", "wss", "grpc", "grpcs", "tcp"}

// Parse reads raw as a value of type t. set is the closed set a word must be
// one of, and nil for every other type. The answer is canonical, so two
// spellings of one value compare equal.
func Parse(t ValueType, raw string, set []string) (Value, error) {
	if t != TypeWord && t != TypeInt && t != TypePort && redact.Contains(raw) {
		return Value{}, ErrCredential
	}
	switch t {
	case TypeWord:
		if !slices.Contains(set, raw) {
			return Value{}, fmt.Errorf("%w: a word is %s", ErrInvalid, words.Or(set))
		}
		return Value{Type: t, Text: raw}, nil
	case TypeInt:
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > MaxInt {
			return Value{}, fmt.Errorf("%w: a count is a whole number from 0 to %d", ErrInvalid, MaxInt)
		}
		return Value{Type: t, Text: strconv.Itoa(n)}, nil
	case TypePort:
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			return Value{}, fmt.Errorf("%w: a port is a whole number from 1 to 65535", ErrInvalid)
		}
		return Value{Type: t, Text: strconv.Itoa(n)}, nil
	case TypeHostname:
		host, err := hostname(raw)
		if err != nil {
			return Value{}, err
		}
		return Value{Type: t, Text: host, Private: config.MCPPrivateHost(host)}, nil
	case TypeIP:
		ip, err := address(raw)
		if err != nil {
			return Value{}, err
		}
		return Value{Type: t, Text: ip.String(), Private: config.MCPPrivateHost(ip.String())}, nil
	case TypeAddress:
		if ip, err := address(raw); err == nil {
			return Value{Type: t, Text: ip.String(), Private: config.MCPPrivateHost(ip.String())}, nil
		}
		host, err := hostname(raw)
		if err != nil {
			return Value{}, fmt.Errorf("%w: an address is a host name or an IP address", ErrInvalid)
		}
		return Value{Type: t, Text: host, Private: config.MCPPrivateHost(host)}, nil
	case TypeURL:
		return parseURL(raw)
	case TypeVersion:
		return matched(t, raw, versionRE(), "a version is up to 32 letters, digits, dots, plus signs and hyphens")
	case TypeRegion:
		return matched(t, raw, regionRE(), "a region is a vendor's region code, such as eu-west-1")
	case TypeAccountRef:
		ref := strings.ToLower(strings.TrimSpace(raw))
		if strings.Contains(ref, "..") || strings.Contains(ref, "//") {
			return Value{}, fmt.Errorf("%w: an account reference is the vendor's own identifier", ErrInvalid)
		}
		return matched(t, ref, accountRefRE(), "an account reference is the vendor's own identifier")
	case TypeBranch:
		if !gitref.ValidBranch(raw) {
			return Value{}, fmt.Errorf("%w: a branch is a git branch name", ErrInvalid)
		}
		return Value{Type: t, Text: raw}, nil
	case TypeLabel:
		return parseLabel(raw)
	case TypeImage:
		if len(raw) > MaxImageBytes {
			return Value{}, fmt.Errorf("%w: an image reference is at most %d bytes", ErrInvalid, MaxImageBytes)
		}
		return matched(t, raw, imageRE(), "an image is a registry's reference: a repository, then a tag or a digest")
	case TypeInstant:
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return Value{}, fmt.Errorf("%w: an instant is an RFC 3339 time", ErrInvalid)
		}
		return Instant(at), nil
	}
	return Value{}, fmt.Errorf("%w: %q is no value type", ErrInvalid, t)
}

// Instant is at as an instant value, in UTC to the second.
func Instant(at time.Time) Value {
	return Value{Type: TypeInstant, Text: at.UTC().Truncate(time.Second).Format(time.RFC3339)}
}

func matched(t ValueType, raw string, re *regexp.Regexp, rule string) (Value, error) {
	if !re.MatchString(raw) {
		return Value{}, fmt.Errorf("%w: %s", ErrInvalid, rule)
	}
	return Value{Type: t, Text: raw}, nil
}

// hostname reads raw as a host name: RFC 1123 labels, the IDNA ASCII form,
// lowercase, no trailing dot, and a wildcard only as a whole first label.
func hostname(raw string) (string, error) {
	name := strings.TrimSuffix(strings.TrimSpace(raw), ".")
	wildcard := strings.HasPrefix(name, "*.")
	if wildcard {
		name = strings.TrimPrefix(name, "*.")
	}
	ascii, err := idna.Lookup.ToASCII(name)
	if err != nil || ascii == "" || len(ascii) > 253 || !gitref.DNSName(ascii) {
		return "", fmt.Errorf("%w: a host name is a DNS name, without a port, a path or credentials", ErrInvalid)
	}
	if wildcard {
		ascii = "*." + ascii
	}
	return ascii, nil
}

// address reads raw as an IP address, refusing a zone, which names one of this
// host's interfaces rather than a machine.
func address(raw string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || ip.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("%w: an IP address, without a zone", ErrInvalid)
	}
	return ip.Unmap(), nil
}

// parseURL reads raw as an address something answers on. It REFUSES user
// information, a query and a fragment instead of dropping them, because an
// address a writer sent with credentials is an address it should resend without.
func parseURL(raw string) (Value, error) {
	if len(raw) > MaxURLBytes {
		return Value{}, fmt.Errorf("%w: an address is at most %d bytes", ErrInvalid, MaxURLBytes)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !slices.Contains(urlSchemes, strings.ToLower(u.Scheme)) || u.Opaque != "" {
		return Value{}, fmt.Errorf("%w: an address is %s, with a host", ErrInvalid, words.Or(urlSchemes))
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return Value{}, fmt.Errorf("%w: record the address without credentials, a query or a fragment", ErrCredential)
	}
	if redact.URL(raw) != raw {
		return Value{}, ErrCredential
	}
	var host string
	var private bool
	if ip, err := address(strings.Trim(u.Hostname(), "[]")); err == nil {
		host, private = ip.String(), config.MCPPrivateHost(ip.String())
		if ip.Is6() {
			host = "[" + host + "]"
		}
	} else {
		if host, err = hostname(u.Hostname()); err != nil || strings.HasPrefix(host, "*.") {
			return Value{}, fmt.Errorf("%w: an address names one host", ErrInvalid)
		}
		private = config.MCPPrivateHost(host)
	}
	if p := u.Port(); p != "" {
		if _, err := Parse(TypePort, p, nil); err != nil {
			return Value{}, err
		}
		host += ":" + p
	}
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	text := strings.ToLower(u.Scheme) + "://" + host + path
	return Value{Type: TypeURL, Text: text, Private: private}, nil
}

// parseLabel reads raw as a name: plain text of at most [MaxLabelRunes]
// runes, with no control or format character and nothing a renderer would
// read as markup or a link.
func parseLabel(raw string) (Value, error) {
	text := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(text)
	if !utf8.ValidString(text) || n == 0 || n > MaxLabelRunes {
		return Value{}, fmt.Errorf("%w: a label is plain text of 1 to %d characters", ErrInvalid, MaxLabelRunes)
	}
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune("<>[]`", r) {
			return Value{}, fmt.Errorf("%w: a label carries no control character and no markup", ErrInvalid)
		}
	}
	if strings.Contains(text, "://") {
		return Value{}, fmt.Errorf("%w: a label carries no link; an address is a fact of its own", ErrInvalid)
	}
	return Value{Type: TypeLabel, Text: text}, nil
}
