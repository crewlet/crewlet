package config

import (
	"crypto/rsa"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// SSH lets agent seats reach the company's servers over SSH with short-lived
// certificates the engine signs with a certificate authority it holds itself.
// Servers trust the authority's public line once; every session carries a
// certificate whose principal names one host and one level.
//
// It ingests nothing. The hosts are written here by people, each with its
// pinned host keys, so the engine never learns a key on first use.
type SSH struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Provisioning is the certificate authority every certificate is signed
	// with, and where certificates may be used from.
	Provisioning *SSHProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"The engine's SSH certificate authority, a staged next one during a rotation, and the addresses certificates may be used from. Required when enabled."`

	// Approvers are the people, beside administrators, who may approve an
	// operate command.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Handles of human seats that may approve an agent's operate command, beside administrators. Optional."`

	// AlwaysEscalate adds to the engine's own always-escalate list, which no
	// setting shortens (internal/ssh's AlwaysEscalateClasses, ADR-0125).
	AlwaysEscalate []string `yaml:"always_escalate,omitempty" json:"always_escalate,omitempty" desc:"Regular expressions (RE2) matched against an operate command and its stdin, added to the engine's own always-escalate list: a command any of them matches is never allowed by a delegate (delegation.rules). At most 64, each up to 512 bytes. Optional."`

	// Commands bounds every command the engine runs on a server.
	Commands *SSHCommands `yaml:"commands,omitempty" json:"commands,omitempty" desc:"How long a command may run and how much of its output reaches the agent. Optional."`

	// Hosts are the servers agents may be granted, each with its pinned keys.
	Hosts []SSHHost `yaml:"hosts,omitempty" json:"hosts,omitempty" desc:"The servers agents may be granted, at most 200, each with its address, pinned host keys and the two login users the server setup creates."`
}

// SSHProvisioning is the engine's certificate authority and its limits.
type SSHProvisioning struct {
	// CertificateAuthority is the engine's Ed25519 private key. Servers hold
	// only its public line.
	CertificateAuthority string `secret:"true" yaml:"certificate_authority,omitempty" json:"certificate_authority,omitempty" desc:"Reference to the engine's SSH certificate authority, an unencrypted OpenSSH Ed25519 private key such as ${SSH_CERTIFICATE_AUTHORITY}. Minted by the connect form and never shown; servers trust only its public line. It can sign a certificate for any host's read or operate user."`

	// NextCertificateAuthority is a rotation's staged authority, trusted by
	// servers beside the current one until it is promoted.
	NextCertificateAuthority string `secret:"true" yaml:"next_certificate_authority,omitempty" json:"next_certificate_authority,omitempty" desc:"Reference to a staged certificate authority, such as ${SSH_CERTIFICATE_AUTHORITY_NEXT}, present only while a rotation is in progress. The engine never signs with it until it is promoted."`

	// SourceAddresses are written into every certificate's source-address.
	SourceAddresses []string `yaml:"source_addresses,omitempty" json:"source_addresses,omitempty" desc:"Address ranges, such as 203.0.113.7/32, every certificate may be used from: the engine's egress addresses. At most 16. Empty leaves certificates usable from anywhere until they expire."`
}

// The bounds of the added always-escalate patterns.
const (
	sshMaxAlwaysPatterns = 64
	sshMaxPatternBytes   = 512
)

// SSHCommands bounds a command the engine runs.
type SSHCommands struct {
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty" desc:"Seconds a command may run before the engine stops it: 10 to 900. Empty is 120."`
	MaxOutputBytes int `yaml:"max_output_bytes,omitempty" json:"max_output_bytes,omitempty" desc:"Bytes of each of a command's output streams returned to the agent, the rest cut: 4096 to 1048576. Empty is 65536."`
}

// SSHHost is one server agents may be granted.
type SSHHost struct {
	// ID is in every certificate principal for this host, so it is fixed
	// once the server is set up.
	ID string `yaml:"id" json:"id" js:"required" desc:"Short name of the server: up to 40 lowercase letters, digits and hyphens. It is part of every certificate principal for this server, so changing it means running the server setup again."`

	Address string `yaml:"address" json:"address" js:"required" desc:"The server's host name or IP address, with no scheme, user, port or path."`
	Port    int    `yaml:"port,omitempty" json:"port,omitempty" desc:"The server's SSH port. Empty is 22."`

	// HostKeys are the only keys the engine accepts from this server.
	HostKeys []string `yaml:"host_keys" json:"host_keys" js:"required" desc:"The server's public host keys, one to eight lines as /etc/ssh/ssh_host_*_key.pub holds them. The engine accepts no other key from this server and never learns one on first use."`

	Users SSHHostUsers `yaml:"users" json:"users" desc:"The login users the server setup creates."`

	Description string `yaml:"description,omitempty" json:"description,omitempty" desc:"What the server is, in one line of up to 200 characters, shown to agents and approvers."`

	// Containers adds the container reads to the server's allowlist and the
	// sudo line they need.
	Containers bool `yaml:"containers,omitempty" json:"containers,omitempty" desc:"Let the read user list containers and read their logs through sudo -n docker ps and docker logs --tail. Off by default."`

	// ReadAllowlist replaces the rendered default allowlist when set.
	ReadAllowlist []string `yaml:"read_allowlist,omitempty" json:"read_allowlist,omitempty" desc:"The commands the read user may run, one rule each, replacing the default list: an absolute path, then words, where * is any one word not starting with -, a word ending in =* is that option with any value, and a final ... is any further words. At most 100 rules. Empty renders the default list."`
}

// SSHHostUsers are a server's two login users, one per level.
type SSHHostUsers struct {
	Read    string `yaml:"read" json:"read" js:"required" desc:"The login user for read access, whose every command is forced through the server's read allowlist, such as crewlet-read."`
	Operate string `yaml:"operate,omitempty" json:"operate,omitempty" desc:"The login user for approved operate commands, such as crewlet-ops. Absent leaves the server read-only."`
}

// The levels a seat holds across its hosts, least access first.
const (
	SSHLevelRead    = "read"
	SSHLevelOperate = "operate"
)

// SSHLevels is every level a seat may name, least access first.
func SSHLevels() []string { return []string{SSHLevelRead, SSHLevelOperate} }

// RoleSSH is a seat's SSH access: its level and its hosts.
type RoleSSH struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may reach servers over SSH. Off stops issuing its certificates at once; ones already issued expire within minutes."`

	// Level is one level for every host the seat names; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate" desc:"read (the default): the server's allowlisted read commands, with no approval. operate: read, plus any command, each one approved by a person before it runs."`

	Hosts []string `yaml:"hosts,omitempty" json:"hosts,omitempty" desc:"Ids of the servers in integrations.ssh.hosts this agent may reach. Required when the agent is on."`
}

// LevelOrDefault is the seat's level, read where it names none.
func (r *RoleSSH) LevelOrDefault() string {
	if r == nil || strings.TrimSpace(r.Level) == "" {
		return SSHLevelRead
	}
	return strings.TrimSpace(r.Level)
}

// The bounds on a command, each with the reason for its value.
const (
	// SSHDefaultTimeout covers a package listing or a log read on a loaded
	// server; a deploy step that needs longer sets timeout_seconds.
	SSHDefaultTimeout = 120 * time.Second
	sshMinTimeout     = 10
	// sshMaxTimeout is the engine-side read certificate's validity; work
	// that runs longer belongs on the server's own scheduler.
	sshMaxTimeout = 900

	// SSHDefaultMaxOutputBytes is about a thousand log lines per stream,
	// within what an agent reads in one turn.
	SSHDefaultMaxOutputBytes = 64 << 10
	sshMinOutputBytes        = 4 << 10
	sshMaxOutputBytes        = 1 << 20

	// SSHDefaultPort is the port a host that names none is reached on.
	SSHDefaultPort = 22

	sshMaxHosts           = 200
	sshMaxAllowlistRules  = 100
	sshMaxRuleBytes       = 512
	sshMaxHostKeys        = 8
	sshMaxSourceAddresses = 16
	sshMaxDescription     = 200
	// sshMinRSABits is the size OpenSSH and NIST accept for new RSA keys.
	sshMinRSABits = 3072
)

// Timeout is how long a command may run.
func (c *SSHCommands) Timeout() time.Duration {
	if c == nil || c.TimeoutSeconds == 0 {
		return SSHDefaultTimeout
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// MaxOutput is how many bytes of each output stream reach the agent.
func (c *SSHCommands) MaxOutput() int {
	if c == nil || c.MaxOutputBytes == 0 {
		return SSHDefaultMaxOutputBytes
	}
	return c.MaxOutputBytes
}

// PortOrDefault is the port the host is reached on.
func (h SSHHost) PortOrDefault() int {
	if h.Port == 0 {
		return SSHDefaultPort
	}
	return h.Port
}

// Host is the host with id, or false where the block names none.
func (s *SSH) Host(id string) (SSHHost, bool) {
	if s == nil {
		return SSHHost{}, false
	}
	for _, h := range s.Hosts {
		if h.ID == id {
			return h, true
		}
	}
	return SSHHost{}, false
}

// HostIDs is every host's id, in document order.
func (s *SSH) HostIDs() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Hosts))
	for _, h := range s.Hosts {
		out = append(out, h.ID)
	}
	return out
}

// SSHHostGrantees is the handle of every agent seat whose SSH block names host,
// on or off, sorted: a host is not removed or renamed while one does.
func (c *Company) SSHHostGrantees(host string) []string {
	var out []string
	for role := range c.EachRole() {
		if b := role.Integrations.SSH; b != nil && slices.Contains(b.Hosts, host) {
			out = append(out, role.IdentityKey())
		}
	}
	slices.Sort(out)
	return out
}

// SSHReach is what one agent seat reaches over SSH now: its level and the
// hosts its block grants, in the block's order. The zero value reaches nothing.
type SSHReach struct {
	Level string
	Hosts []SSHHost
}

// Host is the granted host id, or false where the seat is not granted it.
func (r SSHReach) Host(id string) (SSHHost, bool) {
	for _, h := range r.Hosts {
		if h.ID == id {
			return h, true
		}
	}
	return SSHHost{}, false
}

// HostIDs is the granted hosts' ids, in the block's order.
func (r SSHReach) HostIDs() []string {
	out := make([]string, 0, len(r.Hosts))
	for _, h := range r.Hosts {
		out = append(out, h.ID)
	}
	return out
}

// SSHReachOf is handle's SSH reach as the document says now: nothing where the
// integration, its authority or the seat's block is off.
func (c *Company) SSHReachOf(handle string) SSHReach {
	s := c.Integrations.SSH
	if s == nil || !s.Enabled || s.Provisioning == nil || strings.TrimSpace(s.Provisioning.CertificateAuthority) == "" {
		return SSHReach{}
	}
	for role := range c.EachRole() {
		if role.IdentityKey() != handle {
			continue
		}
		b := role.Integrations.SSH
		if b == nil || !SeatAccessOn(b.Enabled) {
			return SSHReach{}
		}
		var hosts []SSHHost
		for _, id := range b.Hosts {
			if h, ok := s.Host(id); ok {
				hosts = append(hosts, h)
			}
		}
		if len(hosts) == 0 {
			return SSHReach{}
		}
		return SSHReach{Level: b.LevelOrDefault(), Hosts: hosts}
	}
	return SSHReach{}
}

// The shapes a host's names take, compiled at first use for the reason
// internal/setup's shape patterns give.
var (
	// sshHostID is in a principal, crewlet.<id>.read, so it holds no dot.
	sshHostID = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	})
	// sshUser is the portable POSIX user name useradd accepts by default.
	sshUser = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	})
)

// SSHHostID reports whether id can name a host.
func SSHHostID(id string) bool { return sshHostID().MatchString(id) }

// SSHUser reports whether name can name a login user.
func SSHUser(name string) bool { return sshUser().MatchString(name) }

// SSHAddress checks a host's address: a DNS name or an IP address alone, never
// one this engine or its link reaches by definition. It answers the sentence
// a person reads, "" for an address it takes.
func SSHAddress(address string) string {
	if a, err := netip.ParseAddr(address); err == nil {
		if !httpx.ServerAddress(a) {
			return fmt.Sprintf("%q is the engine host or a link-local address, which no server should be reached at", address)
		}
		return ""
	}
	name := strings.ToLower(address)
	if !gitref.DNSName(name) {
		return fmt.Sprintf("%q is not a server address: give a host name or an IP address alone", address)
	}
	// RFC 6761 reserves localhost and every name under it for this host.
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return fmt.Sprintf("%q is the engine host or a link-local address, which no server should be reached at", address)
	}
	return ""
}

// sshRuleWord is one word of a read allowlist rule: only characters the
// server's gate lets through, and * where a rule allows any word.
var sshRuleWord = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z0-9_./:=,@%+*-]+$`)
})

// SSHAllowlistRule checks one read allowlist rule and answers the sentence a
// person reads, "" for a rule the gate can hold a command to: an absolute path
// first, then words of the gate's own characters, * only as a whole word or
// after =, and ... only last.
func SSHAllowlistRule(rule string) string {
	words := strings.Fields(rule)
	switch {
	case len(words) == 0:
		return "a rule is empty; remove it or name the command"
	case len(rule) > sshMaxRuleBytes || strings.Join(words, " ") != rule:
		return fmt.Sprintf("a rule is one line of at most %d characters, its words one space apart", sshMaxRuleBytes)
	case !strings.HasPrefix(words[0], "/") || strings.ContainsAny(words[0], "*="):
		return fmt.Sprintf("%q does not start with a command's absolute path, such as /usr/bin/uptime", rule)
	}
	for i, w := range words {
		switch {
		case !sshRuleWord().MatchString(w):
			return fmt.Sprintf("%q holds a character the server's gate refuses: write it without quotes or shell characters", rule)
		case w == "..." && i != len(words)-1:
			return fmt.Sprintf("%q has ... before its last word; ... stands for any further words, so it goes last", rule)
		case strings.Contains(w, "*") && w != "*" && (!strings.HasSuffix(w, "=*") || strings.Count(w, "*") != 1):
			return fmt.Sprintf("%q uses * inside a word; * is a whole word, or the value after an option's =", rule)
		}
	}
	return ""
}

// SSHHostKeyAlgorithms are the host key types the engine pins, strongest first.
func SSHHostKeyAlgorithms() []string {
	return []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA521, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSA}
}

// ParseSSHHostKey reads one pinned host key line: a plain public key of a type
// the engine pins, with no options and nothing after its comment. RSA needs at
// least 3072 bits.
func ParseSSHHostKey(line string) (ssh.PublicKey, error) {
	pk, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	switch {
	case err != nil:
		return nil, fmt.Errorf("it does not parse as a public key")
	case len(options) > 0 || len(strings.TrimSpace(string(rest))) > 0:
		return nil, fmt.Errorf("it carries options or a second line")
	}
	if _, isCert := pk.(*ssh.Certificate); isCert {
		return nil, fmt.Errorf("it is a certificate, not a host key")
	}
	if !slices.Contains(SSHHostKeyAlgorithms(), pk.Type()) {
		return nil, fmt.Errorf("its type %s is not one of %s", pk.Type(), words.Or(SSHHostKeyAlgorithms()))
	}
	if pk.Type() == ssh.KeyAlgoRSA {
		crypto, ok := pk.(ssh.CryptoPublicKey)
		if !ok {
			return nil, fmt.Errorf("its RSA key cannot be read")
		}
		if rsaKey, ok := crypto.CryptoPublicKey().(*rsa.PublicKey); !ok || rsaKey.N.BitLen() < sshMinRSABits {
			return nil, fmt.Errorf("its RSA key is shorter than %d bits", sshMinRSABits)
		}
	}
	return pk, nil
}

func (s *SSH) validate(path Path) error {
	var p problems
	if s.Enabled && s.Provisioning == nil {
		p.add(at(path, "provisioning"), ErrMissing,
			"required when ssh is enabled: it holds the certificate authority the engine signs "+
				"every agent's certificate with")
	}
	if prov := s.Provisioning; prov != nil {
		prov.validate(&p, at(path, "provisioning"), s.Enabled)
	}
	if c := s.Commands; c != nil {
		if t := c.TimeoutSeconds; t != 0 && (t < sshMinTimeout || t > sshMaxTimeout) {
			p.add(at(path, "commands.timeout_seconds"), ErrOutOfRange,
				"%d is outside %d to %d seconds; leave it empty for %d", t, sshMinTimeout, sshMaxTimeout,
				int(SSHDefaultTimeout/time.Second))
		}
		if b := c.MaxOutputBytes; b != 0 && (b < sshMinOutputBytes || b > sshMaxOutputBytes) {
			p.add(at(path, "commands.max_output_bytes"), ErrOutOfRange,
				"%d is outside %d to %d bytes; leave it empty for %d", b, sshMinOutputBytes, sshMaxOutputBytes,
				SSHDefaultMaxOutputBytes)
		}
	}
	if n := len(s.AlwaysEscalate); n > sshMaxAlwaysPatterns {
		p.add(at(path, "always_escalate"), ErrOutOfRange, "%d patterns is more than %d", n, sshMaxAlwaysPatterns)
	}
	for i, pattern := range s.AlwaysEscalate {
		here := idx(at(path, "always_escalate"), i)
		if strings.TrimSpace(pattern) == "" || len(pattern) > sshMaxPatternBytes {
			p.add(here, ErrOutOfRange, "a pattern is 1 to %d bytes", sshMaxPatternBytes)
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			p.add(here, ErrUnknownValue, "%q is not a regular expression (RE2): %s", pattern, err.Error())
		}
	}
	if len(s.Hosts) > sshMaxHosts {
		p.add(at(path, "hosts"), ErrOutOfRange, "%d servers is more than the %d one company may register",
			len(s.Hosts), sshMaxHosts)
	}
	ids := map[string]bool{}
	for i, h := range s.Hosts {
		here := idx(at(path, "hosts"), i)
		h.validate(&p, here)
		if ids[h.ID] && h.ID != "" {
			p.add(at(here, "id"), ErrConflict, "host id %q is used twice", h.ID)
		}
		ids[h.ID] = true
	}
	return p.err()
}

func (prov *SSHProvisioning) validate(p *problems, path Path, enabled bool) {
	ca := strings.TrimSpace(prov.CertificateAuthority)
	caName, caRef := envref.Whole(ca)
	switch {
	case ca == "" && enabled:
		p.add(at(path, "certificate_authority"), ErrMissing,
			"must be a reference to a sealed value such as ${SSH_CERTIFICATE_AUTHORITY}, which Save mints")
	case ca != "" && !caRef:
		p.add(at(path, "certificate_authority"), ErrConflict,
			"must be a reference to a sealed value such as ${SSH_CERTIFICATE_AUTHORITY}, which Save mints")
	}
	if next := strings.TrimSpace(prov.NextCertificateAuthority); next != "" {
		switch nextName, nextRef := envref.Whole(next); {
		case !nextRef:
			p.add(at(path, "next_certificate_authority"), ErrConflict,
				"must be a reference to a sealed value such as ${SSH_CERTIFICATE_AUTHORITY_NEXT}, which a rotation mints")
		case caRef && nextName == caName:
			p.add(at(path, "next_certificate_authority"), ErrConflict,
				"names the same value as certificate_authority; a rotation stages a new one")
		}
	}
	if len(prov.SourceAddresses) > sshMaxSourceAddresses {
		p.add(at(path, "source_addresses"), ErrOutOfRange, "%d address ranges is more than %d",
			len(prov.SourceAddresses), sshMaxSourceAddresses)
	}
	for i, cidr := range prov.SourceAddresses {
		if prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr)); err != nil || prefix.Masked() != prefix {
			p.add(idx(at(path, "source_addresses"), i), ErrUnknownValue,
				"%q is not an address range such as 203.0.113.7/32", cidr)
		}
	}
}

func (h SSHHost) validate(p *problems, path Path) {
	if !SSHHostID(h.ID) {
		p.add(at(path, "id"), ErrUnknownValue,
			"a host id is up to 40 lowercase letters, digits and hyphens; it is part of the certificate principal")
	}
	if strings.TrimSpace(h.Address) == "" {
		p.add(at(path, "address"), ErrMissing, "required: the server's host name or IP address")
	} else if sentence := SSHAddress(h.Address); sentence != "" {
		p.add(at(path, "address"), ErrUnknownValue, "%s", sentence)
	}
	if h.Port < 0 || h.Port > 65535 {
		p.add(at(path, "port"), ErrOutOfRange, "%d is not a port from 1 to 65535; leave it empty for 22", h.Port)
	}
	switch n := len(h.HostKeys); {
	case n == 0:
		p.add(at(path, "host_keys"), ErrMissing,
			"required: the server's public host keys, as /etc/ssh/ssh_host_*_key.pub holds them")
	case n > sshMaxHostKeys:
		p.add(at(path, "host_keys"), ErrOutOfRange, "%d host keys is more than %d", n, sshMaxHostKeys)
	}
	seen := map[string]bool{}
	for i, line := range h.HostKeys {
		pk, err := ParseSSHHostKey(line)
		if err != nil {
			p.add(idx(at(path, "host_keys"), i), ErrUnknownValue,
				"host key %d is not a public key line as /etc/ssh/ssh_host_*_key.pub holds it: %s", i+1, err.Error())
			continue
		}
		if wire := string(pk.Marshal()); seen[wire] {
			p.add(idx(at(path, "host_keys"), i), ErrConflict, "host key %d is listed twice", i+1)
		} else {
			seen[wire] = true
		}
	}
	switch read := h.Users.Read; {
	case read == "":
		p.add(at(path, "users.read"), ErrMissing, "required: the login user for read access, such as crewlet-read")
	case !SSHUser(read):
		p.add(at(path, "users.read"), ErrUnknownValue,
			"%q is not a login user: up to 32 lowercase letters, digits, hyphens and underscores, "+
				"starting with a letter or an underscore", read)
	}
	switch op := h.Users.Operate; {
	case op == "":
	case !SSHUser(op):
		p.add(at(path, "users.operate"), ErrUnknownValue,
			"%q is not a login user: up to 32 lowercase letters, digits, hyphens and underscores, "+
				"starting with a letter or an underscore", op)
	case op == h.Users.Read:
		p.add(at(path, "users.operate"), ErrConflict,
			"the operate user must differ from the read user, whose every command is forced through the read allowlist")
	}
	if len(h.ReadAllowlist) > sshMaxAllowlistRules {
		p.add(at(path, "read_allowlist"), ErrOutOfRange, "%d rules is more than %d", len(h.ReadAllowlist),
			sshMaxAllowlistRules)
	}
	for i, rule := range h.ReadAllowlist {
		if sentence := SSHAllowlistRule(rule); sentence != "" {
			p.add(idx(at(path, "read_allowlist"), i), ErrUnknownValue, "%s", sentence)
		}
	}
	if utf8.RuneCountInString(h.Description) > sshMaxDescription || strings.ContainsAny(h.Description, "\r\n") {
		p.add(at(path, "description"), ErrOutOfRange, "a description is one line of at most %d characters",
			sshMaxDescription)
	}
}

// validateSSHApprovers holds every approver to a person's seat, each once.
func (c *Company) validateSSHApprovers() error {
	var p problems
	s := c.Integrations.SSH
	if s == nil {
		return nil
	}
	humans := map[string]bool{}
	for role := range c.EachRole() {
		if role.Kind == org.KindHuman {
			humans[role.IdentityKey()] = true
		}
	}
	seen := map[string]bool{}
	for i, handle := range s.Approvers {
		here := idx(field("integrations.ssh.approvers"), i)
		switch {
		case !humans[handle]:
			p.add(here, ErrUnknownValue, "%q is not a person's seat; only people approve production commands", handle)
		case seen[handle]:
			p.add(here, ErrConflict, "%q is listed twice", handle)
		}
		seen[handle] = true
	}
	return p.err()
}

// checkSSH holds a seat's level to the closed set and its hosts to the
// company's servers, each once.
func (c *Company) checkSSH(p *problems, path Path, b *RoleSSH) {
	if b == nil {
		return
	}
	if level := strings.TrimSpace(b.Level); level != "" && !slices.Contains(SSHLevels(), level) {
		p.add(at(path, "level"), ErrUnknownValue, "%q is not an SSH level this engine offers; give %s",
			b.Level, words.Or(SSHLevels()))
	}
	seen := map[string]bool{}
	for i, host := range b.Hosts {
		here := idx(at(path, "hosts"), i)
		switch _, ok := c.Integrations.SSH.Host(host); {
		case !ok:
			// A REMOVED OR RENAMED HOST reads as the agents still naming it,
			// in the words the host route answers a removal with.
			p.add(here, ErrUnknownValue, "host %q is not in integrations.ssh.hosts and is granted to %s; "+
				"remove it from those agents first, or add the host back", host, words.And(c.SSHHostGrantees(host)))
		case seen[host]:
			p.add(here, ErrConflict, "%q is listed twice", host)
		}
		seen[host] = true
	}
}

// sshAdvisories says where a seat's operate level reaches a host only at read,
// since the host names no operate user.
func (c *Company) sshAdvisories() []Warning {
	var out []Warning
	for role, path := range c.EachRole() {
		b := role.Integrations.SSH
		if b == nil || role.Kind == org.KindHuman || b.LevelOrDefault() != SSHLevelOperate {
			continue
		}
		for i, id := range b.Hosts {
			if h, ok := c.Integrations.SSH.Host(id); ok && h.Users.Operate == "" {
				out = append(out, SeatAdvisory(idx(at(path, "integrations.ssh.hosts"), i), role.IdentityKey(),
					fmt.Sprintf("host %q has no operate user, so %s reaches it at read", id, role.IdentityKey())))
			}
		}
	}
	return out
}
