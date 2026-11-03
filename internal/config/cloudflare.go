package config

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/words"
)

// Cloudflare gives each agent seat its own account API token, minted and kept
// narrow by the engine through the company's provisioner token.
//
// It ingests nothing: v1 has no inbound route, so the block is provisioning
// alone and [Cloudflare.validate] requires it whenever the block is enabled.
type Cloudflare struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Provisioning is the account the agents' tokens are minted in and the
	// token they are minted with.
	Provisioning *CloudflareProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"The Cloudflare account and the provisioner token the engine mints each agent's token with. Required when enabled."`
}

// CloudflareProvisioning is what the engine needs to mint, narrow, roll and
// delete one account API token per agent seat.
type CloudflareProvisioning struct {
	// AccountID is the account every agent token is owned by and scoped to.
	AccountID string `yaml:"account_id,omitempty" json:"account_id,omitempty" desc:"Cloudflare account ID (32 hexadecimal characters) the agents' tokens are created in."`

	// APIToken is an account API token holding Account API Tokens Write and
	// every permission group a seat level grants, since a token is only
	// trusted to grant what it holds itself.
	APIToken string `secret:"true" yaml:"api_token,omitempty" json:"api_token,omitempty" desc:"Account API token with Account API Tokens Write, Account Settings Read, Zone Read and every permission an agent level grants."`

	// Zones are the zone names every agent token is scoped to; empty is
	// every zone in the account.
	Zones []string `yaml:"zones,omitempty" json:"zones,omitempty" desc:"Zone names the agents' tokens are scoped to. Empty means every zone in the account."`
}

// The levels a seat's Cloudflare token is minted at, least access first.
// internal/cloudflare maps each to the permission groups it grants.
const (
	CloudflareLevelRead      = "read"
	CloudflareLevelOperate   = "operate"
	CloudflareLevelConfigure = "configure"
)

// CloudflareLevels is every level a seat may name, least access first.
func CloudflareLevels() []string {
	return []string{CloudflareLevelRead, CloudflareLevelOperate, CloudflareLevelConfigure}
}

// RoleCloudflare is a seat's own Cloudflare account API token.
type RoleCloudflare struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has its own Cloudflare token. Off deletes the token."`

	// Level is what the token may do; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|configure" desc:"read (the default): zone, DNS, cache, firewall, SSL and analytics reads. operate: read plus cache purge and cache rule edits. configure: operate plus compression rule edits and every zone setting, the SSL mode and security level included."`
}

// LevelOrDefault is the seat's level, read where it names none.
func (r *RoleCloudflare) LevelOrDefault() string {
	if r == nil {
		return CloudflareLevelRead
	}
	if level := strings.TrimSpace(r.Level); level != "" {
		return level
	}
	return CloudflareLevelRead
}

// ZoneNames is the zones the block names, nil-safe: nil is every zone.
func (c *Cloudflare) ZoneNames() []string {
	if c == nil || c.Provisioning == nil {
		return nil
	}
	return c.Provisioning.Zones
}

// cloudflareAccountID is the shape Cloudflare gives every account identifier.
// Compiled at first use, not at init: the code host's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go); the
// bounded repeat compiles to a program holding every count, about 13 KB.
var cloudflareAccountID = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9a-f]{32}$`)
})

// cloudflareZoneName is a DNS name as Cloudflare lists a zone: labels of
// letters, digits and hyphens, at least two of them.
// Compiled at first use, not at init, for the same reason: its two {0,61}
// repeats are about 87 KB at init, a third of a package's whole budget there.
var cloudflareZoneName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(
		`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
})

// CloudflareZoneKey is a zone name as Cloudflare matches it: trimmed and
// lowercased, without a trailing dot.
func CloudflareZoneKey(zone string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(zone)), ".")
}

func (c *Cloudflare) validate(path Path) error {
	var p problems
	if !c.Enabled {
		return p.err()
	}
	if c.Provisioning == nil {
		p.add(at(path, "provisioning"), ErrMissing,
			"required when cloudflare is enabled: it names the account the agents' "+
				"tokens are created in and the token they are created with")
		return p.err()
	}
	prov := c.Provisioning
	id := strings.TrimSpace(prov.AccountID)
	switch {
	case id == "":
		p.add(at(path, "provisioning.account_id"), ErrMissing,
			"required when cloudflare is enabled: the account ID shown on the "+
				"account's overview page in the Cloudflare dashboard")
	case envref.Has(id):
		// Resolved and checked where the client is built.
	case !cloudflareAccountID().MatchString(id):
		p.add(at(path, "provisioning.account_id"), ErrUnknownValue,
			"%q is not a Cloudflare account ID, which is 32 lowercase hexadecimal "+
				"characters", prov.AccountID)
	}
	if strings.TrimSpace(prov.APIToken) == "" {
		p.add(at(path, "provisioning.api_token"), ErrMissing,
			"required when cloudflare is enabled: the account API token the "+
				"engine mints, narrows, rolls and deletes the agents' tokens with")
	}
	seen := map[string]bool{}
	for i, zone := range prov.Zones {
		key := CloudflareZoneKey(zone)
		switch {
		case key == "":
			p.add(idx(at(path, "provisioning.zones"), i), ErrMissing,
				"a zone name is empty; remove the entry or name the zone")
		case !cloudflareZoneName().MatchString(key):
			p.add(idx(at(path, "provisioning.zones"), i), ErrUnknownValue,
				"%q is not a zone name: give the domain as Cloudflare lists it, "+
					"such as example.com, with no scheme or path", zone)
		case seen[key]:
			p.add(idx(at(path, "provisioning.zones"), i), ErrConflict,
				"%q is listed twice", zone)
		}
		seen[key] = true
	}
	return p.err()
}

// checkCloudflare holds a seat's level to the closed set.
func (c *Company) checkCloudflare(p *problems, path Path, b *RoleCloudflare) {
	if b == nil {
		return
	}
	if level := strings.TrimSpace(b.Level); level != "" && !slices.Contains(CloudflareLevels(), level) {
		p.add(at(path, "level"), ErrUnknownValue,
			"%q is not a Cloudflare level this engine offers; give %s",
			b.Level, words.Or(CloudflareLevels()))
	}
}
