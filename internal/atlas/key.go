package atlas

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/redact"
)

// Namespace is the one name space every entry id is minted under. Fixed for
// good: a different namespace is a different id for every entry ever recorded.
var Namespace = uuid.MustParse("93381a7d-aa4c-4cc0-ad76-62e1e40eb857")

// MaxKeyBytes bounds a natural key. The longest real one is a pipeline of a
// forge repository, well under it.
const MaxKeyBytes = 512

// Ref is one entry by its identity: its kind and its canonical natural key.
// Two writers that meet the same thing name the same entry without asking
// anybody, which is what lets a fact computed at read and a recorded one meet.
type Ref struct {
	Kind Kind   `json:"kind"`
	Key  string `json:"key"`
}

// ID is the entry's id: a name-based version 8 UUID over SHA-256 (RFC 9562,
// section 5.8) of the kind and the key. UUID-shaped, as every engine id is,
// and carrying no address in the clear into a URL or a log line.
func (r Ref) ID() uuid.UUID {
	return uuid.NewHash(sha256.New(), Namespace, []byte(string(r.Kind)+"\x00"+r.Key), 8)
}

// String is the ref as kind:key, for messages.
func (r Ref) String() string { return string(r.Kind) + ":" + r.Key }

var (
	// segmentRE is one segment of a path key: a vendor's identifier, a
	// configured name, a resource id.
	segmentRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[A-Za-z0-9_~:+-][A-Za-z0-9._~:+-]{0,127}$`)
	})
	// slugRE is a service's key: the name people use for it.
	slugRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`) })
)

// NewRef is the ref of an entry of kind k named by key, its key canonical. It
// refuses a key its kind's grammar does not admit, naming the grammar.
func NewRef(k Kind, key string) (Ref, error) {
	if !k.Valid() {
		return Ref{}, fmt.Errorf("%w: not a kind of entry", ErrInvalid)
	}
	canonical, err := canonicalKey(k, strings.TrimSpace(key))
	if err != nil {
		return Ref{}, fmt.Errorf("%s key: %w", k, err)
	}
	if len(canonical) > MaxKeyBytes {
		return Ref{}, fmt.Errorf("%s key: %w: at most %d bytes", k, ErrInvalid, MaxKeyBytes)
	}
	return Ref{Kind: k, Key: canonical}, nil
}

// RecordedRef is [NewRef] for an entry somebody tells the engine about: a key
// shaped like a credential is REFUSED ([ErrCredential]), as it was written and
// in its canonical form, since the canonical form of a host name is lowercase
// and a token's shape can live in its case. A key's grammar admits a vendor's
// own identifiers, and some share a token's alphabet; the keys the engine
// computes are read from the redacting copy, so [NewRef] stays the fold's check.
func RecordedRef(k Kind, key string) (Ref, error) {
	if redact.Contains(key) {
		return Ref{}, fmt.Errorf("%s key: %w", k, ErrCredential)
	}
	r, err := NewRef(k, key)
	if err != nil {
		return Ref{}, err
	}
	if err := r.Recordable(); err != nil {
		return Ref{}, err
	}
	return r, nil
}

// Recordable refuses a ref whose canonical key is shaped like a credential
// ([ErrCredential]), naming its kind and never the key.
func (r Ref) Recordable() error {
	if redact.Contains(r.Key) {
		return fmt.Errorf("%s key: %w", r.Kind, ErrCredential)
	}
	return nil
}

// OnGitlet reports an entry Gitlet names: its own account, a repository keyed
// gitlet/, or one of its checks.
func (r Ref) OnGitlet() bool {
	if r.Kind == KindAccount {
		return r.Key == string(VendorGitlet)
	}
	return (r.Kind == KindRepository || r.Kind == KindPipeline) && strings.HasPrefix(r.Key, GitletPrefix+"/")
}

// EngineNamed is where an entry the engine itself names is declared, and false
// for one a person may add by hand: a model and a tool server are declared in
// the settings, a server keyed ssh/ in the SSH settings, and a repository or a
// check keyed gitlet/ in Gitlet. Such an entry enters the atlas from there.
func (r Ref) EngineNamed() (Source, bool) {
	switch {
	case r.Kind == KindModel, r.Kind == KindToolServer,
		r.Kind == KindHost && strings.HasPrefix(r.Key, HostSSHPrefix+"/"):
		return Source{Kind: SourceSettings}, true
	case r.OnGitlet():
		return RecordSource(RecordGitlet), true
	}
	return Source{}, false
}

// MustRef is [NewRef] for a key the caller built from values already parsed.
func MustRef(k Kind, key string) Ref {
	r, err := NewRef(k, key)
	if err != nil {
		panic(err)
	}
	return r
}

func canonicalKey(k Kind, key string) (string, error) {
	switch k {
	case KindDomain, KindHostname:
		host, err := hostname(key)
		if err != nil {
			return "", err
		}
		if k == KindDomain && strings.HasPrefix(host, "*.") {
			return "", fmt.Errorf("%w: a domain is the zone itself, never a wildcard", ErrInvalid)
		}
		return host, nil
	case KindEndpoint:
		v, err := parseURL(key)
		if err != nil {
			return "", err
		}
		return v.Text, nil
	case KindService:
		if !slugRE().MatchString(key) {
			return "", fmt.Errorf("%w: a service is 1 to 40 lowercase letters, digits and hyphens", ErrInvalid)
		}
		return key, nil
	case KindAccount:
		vendor, account, _ := strings.Cut(key, "/")
		if !Vendor(vendor).Valid() {
			return "", fmt.Errorf("%w: an account's key starts with its vendor", ErrInvalid)
		}
		if account == "" {
			return vendor, nil
		}
		v, err := Parse(TypeAccountRef, account, nil)
		if err != nil {
			return "", err
		}
		return vendor + "/" + v.Text, nil
	case KindRepository:
		return repositoryKey(key)
	case KindPipeline:
		repo, name, ok := strings.Cut(key, "#")
		if !ok || !segmentRE().MatchString(name) {
			return "", fmt.Errorf("%w: a pipeline is its repository's key, '#' and its name", ErrInvalid)
		}
		canonical, err := repositoryKey(repo)
		if err != nil {
			return "", err
		}
		return canonical + "#" + name, nil
	case KindModel:
		if key == ModelEmbeddings {
			return key, nil
		}
		return pathKey(key, ModelLLMPrefix)
	case KindToolServer:
		return pathKey(key, ToolServerPrefix)
	}
	return pathKey(key, "")
}

// The namespaces of the keys the engine itself mints.
const (
	// ModelLLMPrefix starts a model entry's key: llm/<entry key>.
	ModelLLMPrefix = "llm"
	// ModelEmbeddings is the key of the company's one embeddings entry.
	ModelEmbeddings = "embeddings"
	// ToolServerPrefix starts a tool server's key: mcp/<server name>.
	ToolServerPrefix = "mcp"
	// HostSSHPrefix starts the key of a server the SSH settings declare:
	// ssh/<host id>.
	HostSSHPrefix = "ssh"
	// GitletPrefix starts the key of a repository on Gitlet: gitlet/<name>.
	GitletPrefix = "gitlet"
)

// pathKey reads key as segments joined by '/', the first one prefix where
// prefix is set, with at least two segments.
func pathKey(key, prefix string) (string, error) {
	segments := strings.Split(key, "/")
	if len(segments) < 2 || (prefix != "" && segments[0] != prefix) {
		if prefix != "" {
			return "", fmt.Errorf("%w: the key is %s/ and a name", ErrInvalid, prefix)
		}
		return "", fmt.Errorf("%w: the key is a namespace, '/' and an identifier", ErrInvalid)
	}
	for _, s := range segments {
		if !segmentRE().MatchString(s) {
			return "", fmt.Errorf("%w: each part of the key is letters, digits and . _ ~ : + -", ErrInvalid)
		}
	}
	return key, nil
}

// repositoryKey reads key as a repository's: gitlet/<name> on the engine's own
// host, or <host>/<path> on a forge, lowercase, as a forge compares them.
func repositoryKey(key string) (string, error) {
	if name, ok := strings.CutPrefix(key, GitletPrefix+"/"); ok {
		if !gitref.ValidRepoName(name) {
			return "", fmt.Errorf("%w: a Gitlet repository's key is gitlet/ and its name", ErrInvalid)
		}
		return key, nil
	}
	host, path, ok := strings.Cut(strings.ToLower(key), "/")
	if !ok {
		return "", fmt.Errorf("%w: a repository's key is its host and its path", ErrInvalid)
	}
	canonicalHost, err := hostname(host)
	if err != nil || strings.HasPrefix(canonicalHost, "*.") {
		return "", fmt.Errorf("%w: a repository's key is its host and its path", ErrInvalid)
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if _, err := pathKey(path, ""); err != nil {
		return "", fmt.Errorf("%w: a repository's path is its owner and its name", ErrInvalid)
	}
	return canonicalHost + "/" + path, nil
}

// RepositoryKeyOf is the repository key of a forge address
// (https://github.com/acme/api.git is github.com/acme/api): what a mirror's
// source, a push mirror's target and a forge's own repository all reduce to,
// so the three meet on one key.
func RepositoryKeyOf(address string) (string, error) {
	v, err := parseURL(address)
	if err != nil {
		return "", err
	}
	_, rest, _ := strings.Cut(v.Text, "://")
	host, path, _ := strings.Cut(rest, "/")
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.HasSuffix(host, "]") {
		host = host[:i]
	}
	return repositoryKey(host + "/" + path)
}
