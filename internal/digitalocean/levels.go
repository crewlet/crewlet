package digitalocean

import (
	"slices"

	"github.com/crewlet/crewlet/internal/config"
)

// Every scope below is DigitalOcean's own name from its Custom Scopes picker.
// Together, the levels' grants, refusedScopes and unneededScopes classify every
// scope the picker offers, each once (TestEveryPickerScopeIsClassifiedOnce).

// readScopes is the Read level: every read an SRE needs, and account:read,
// which every check starts with.
var readScopes = []string{
	"account:read", "actions:read", "regions:read", "sizes:read", "image:read", "snapshot:read",
	"droplet:read", "block_storage:read", "block_storage_action:read", "block_storage_snapshot:read",
	"firewall:read", "load_balancer:read", "vpc:read", "vpc_peering:read", "nat_gateway:read",
	"reserved_ip:read", "domain:read", "certificate:read", "cdn:read", "spaces:read", "database:read",
	"kubernetes:read", "app:read", "monitoring:read", "uptime:read", "project:read", "tag:read",
	"ssh_key:read",
}

// operateScopes is what Operate adds to Read: creating, changing and deleting
// compute, storage and network resources, with no billing, team, key or
// secret scope.
var operateScopes = []string{
	"droplet:create", "droplet:update", "droplet:delete",
	"image:create", "image:update", "image:delete", "snapshot:delete",
	"kubernetes:create", "kubernetes:update", "kubernetes:delete",
	"app:create", "app:update", "app:delete",
	"block_storage:create", "block_storage:update", "block_storage:delete", "block_storage_action:create",
	"block_storage_snapshot:create", "block_storage_snapshot:update", "block_storage_snapshot:delete",
	"spaces:update", "database:create", "database:update", "database:delete",
	"firewall:create", "firewall:update", "firewall:delete",
	"load_balancer:create", "load_balancer:update", "load_balancer:delete",
	"vpc:create", "vpc:update", "vpc:delete", "vpc_peering:create", "vpc_peering:update", "vpc_peering:delete",
	"nat_gateway:create", "nat_gateway:update", "nat_gateway:delete",
	"reserved_ip:create", "reserved_ip:update", "reserved_ip:delete",
	"domain:create", "domain:update", "domain:delete", "certificate:create", "certificate:delete",
	"monitoring:create", "monitoring:update", "monitoring:delete",
	"uptime:create", "uptime:update", "uptime:delete",
	"tag:create", "tag:delete", "project:assign_resource",
}

// administerScopes is what Administer adds to Operate: access on resources the
// agent manages that can outlive its token.
var administerScopes = []string{
	"droplet:admin", "kubernetes:access_cluster", "database:view_credentials", "app:access_console",
}

// refusedScopes is what no level grants and the paste refuses where it can
// prove it held: billing, team and organization, the account's keys and
// secrets, and every scope that mints a credential.
var refusedScopes = []string{
	"billing:read", "security:read", "security:create", "security:update", "security:delete",
	"registry:read", "registry:create", "registry:update", "registry:delete",
	"spaces_key:read", "spaces_key:create", "spaces_key:create_credentials", "spaces_keys:create",
	"spaces_key:update", "spaces_key:delete",
	"ssh_key:create", "ssh_key:update", "ssh_key:delete",
	"dedicated_inference_token:read", "dedicated_inference_token:create",
	"dedicated_inference_token:update", "dedicated_inference_token:delete",
	"team_membership:read", "role_management:read", "organization:create_team",
	"organization_membership:read_teams",
	"function:admin", "functions:admin", "partner_network_connect:view_credentials",
}

// unneededScopes is what no level grants and the paste accepts: an agent does
// not need them, and none reveals or mints a credential.
var unneededScopes = []string{
	"addon:read", "addon:create", "addon:update", "addon:delete",
	"byoip_prefix:read", "byoip_prefix:create", "byoip_prefix:update", "byoip_prefix:delete",
	"cdn:create", "cdn:update", "cdn:delete",
	"dedicated_inference:read", "dedicated_inference:create", "dedicated_inference:update",
	"dedicated_inference:delete",
	"function:read", "function:create", "function:update", "function:delete",
	"genai:read", "genai:create", "genai:update", "genai:delete",
	"genai_genie:read", "genai_genie:create", "genai_genie:update", "genai_genie:delete",
	"nfs:read", "nfs:create", "nfs:update", "nfs:delete",
	"partner_network_connect:read", "partner_network_connect:create", "partner_network_connect:update",
	"partner_network_connect:delete",
	"project:create", "project:update", "project:delete", "projects:update",
	"signals:query", "signals:update",
	"vendor_portal:read", "vendor_portal:create", "vendor_portal:update", "vendor_portal:delete",
}

// level is one seat level: its name in Crewlet and the sentence saying what it
// allows.
type level struct {
	id, name, note string
	adds           []string
	administers    bool
}

// levels is THE ONE TABLE of seat levels, least access first; each holds the
// levels before it.
var levels = []level{
	{config.DigitalOceanLevelRead, "Read",
		"Reads Droplets, volumes, snapshots, networking, DNS, databases, Kubernetes clusters, apps, " +
			"monitoring and projects. Changes nothing.", readScopes, false},
	{config.DigitalOceanLevelOperate, "Operate",
		"Read, plus creating, changing and deleting Droplets, images, volumes and snapshots, firewalls, " +
			"load balancers, VPCs, reserved IPs, DNS records and certificates, databases, Kubernetes " +
			"clusters, apps, and alerts and uptime checks. No billing, team, keys or secrets.",
		operateScopes, false},
	{config.DigitalOceanLevelAdminister, "Administer",
		"Operate, plus downloading cluster credentials, reading database passwords and opening consoles " +
			"on Droplets and apps. Access gained that way can outlive this agent's token: rotate those " +
			"credentials when you take the level away.", administerScopes, true},
}

// Levels is every seat level, least access first.
func Levels() []string { return config.DigitalOceanLevels() }

// find is the table's row for id, and its index.
func find(id string) (level, int, bool) {
	for i, l := range levels {
		if l.id == id {
			return l, i, true
		}
	}
	return level{}, -1, false
}

// LevelName is a level's name, or id where it is no level.
func LevelName(id string) string {
	if l, _, ok := find(id); ok {
		return l.name
	}
	return id
}

// LevelNote is the sentence saying what a level allows.
func LevelNote(id string) string {
	l, _, _ := find(id)
	return l.note
}

// Administers reports a level that grants access able to outlive the token,
// which asks for a fresh step-up to grant.
func Administers(id string) bool {
	l, _, _ := find(id)
	return l.administers
}

// Grants is every scope a level holds, the levels before it included, or nil
// for no level.
func Grants(id string) []string {
	_, at, ok := find(id)
	if !ok {
		return nil
	}
	var out []string
	for _, l := range levels[:at+1] {
		out = append(out, l.adds...)
	}
	return out
}

// Refused is every scope the paste refuses where it can prove it held.
func Refused() []string { return slices.Clone(refusedScopes) }

// Unneeded is every scope no level grants that the paste accepts.
func Unneeded() []string { return slices.Clone(unneededScopes) }
