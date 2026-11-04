package scaleway

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/words"
)

// What each seat level grants, by Scaleway's own permission set names. No
// level holds a set that mints or reveals a credential outliving the seat's
// own key, except Administer, which a person grants knowingly with a step-up
// and whose note names what it can leave behind.

// level is one seat level: the ID a document names, how a person reads it, the
// project-scoped sets it grants on top of every level before it, and whether
// it is an administration grant ([seataccess.Level.Administers]).
type level struct {
	id, name, note string
	administers    bool
	adds           []string
}

// levels is every seat level, least access first, each granting everything
// the levels before it do.
//
//nolint:gochecknoglobals // an immutable table, not state
var levels = []level{
	{
		id: config.ScalewayLevelRead, name: "Read",
		note: "Reads servers, storage, networks, DNS and databases in its projects, plus the audit " +
			"trail across the organization.",
		// Bucket configuration and object names, never object contents:
		// those hold Terraform state and backups.
		adds: []string{
			"InstancesReadOnly", "BlockStorageReadOnly", "ObjectStorageBucketsRead",
			"LoadBalancersReadOnly", "PrivateNetworksReadOnly", "VPCReadOnly",
			"VPCGatewayReadOnly", "IPAMReadOnly", "DomainsDNSReadOnly",
			"RelationalDatabasesReadOnly", "RedisReadOnly", "ContainerRegistryReadOnly",
			"ObservabilityReadOnly",
		},
	},
	{
		id: config.ScalewayLevelOperate, name: "Operate",
		note: "Also restarts servers and changes firewalls, IPs, load balancers and private " +
			"networks in its projects.",
		// Power and networking only: no server creation or rewrite, no
		// console, no disk copy, so nothing that plants a credential.
		adds: []string{
			"InstancesServerStart", "InstancesServerStop",
			"InstancesSecurityGroupCreate", "InstancesSecurityGroupWrite", "InstancesSecurityGroupDelete",
			"InstancesIpCreate", "InstancesIpWrite", "InstancesIpDelete",
			"InstancesPrivateNicCreate", "InstancesPrivateNicWrite", "InstancesPrivateNicDelete",
			"InstancesPrivateNetworkInterfaceCreate", "InstancesPrivateNetworkInterfaceWrite",
			"InstancesPrivateNetworkInterfaceDelete",
			"InstancesPlacementGroupCreate", "InstancesPlacementGroupWrite", "InstancesPlacementGroupDelete",
			"LoadBalancersFullAccess", "PrivateNetworksFullAccess", "VPCFullAccess",
			"VPCGatewayFullAccess", "IPAMFullAccess", "ObservabilityGrafanaEditor",
		},
	},
	{
		id: config.ScalewayLevelAdminister, name: "Administer", administers: true,
		note: "Also creates, rebuilds and deletes servers, disks, databases, Kubernetes clusters and " +
			"registry images, and can leave access behind in them: SSH keys and startup scripts on " +
			"servers, database users, kubeconfigs with the cluster's admin token, and public images. " +
			"When you take this level away, rotate what it could reach.",
		adds: []string{
			"InstancesFullAccess", "BlockStorageFullAccess", "RelationalDatabasesFullAccess",
			"RedisFullAccess", "KubernetesFullAccess", "ContainerRegistryFullAccess",
		},
	},
}

// orgSets is what every level grants across the organization: listing the
// projects the CLI resolves, the organization's quotas, and the audit trail.
// Never billing, never IAM.
//
//nolint:gochecknoglobals // an immutable list, not state
var orgSets = []string{"AuditTrailReadOnly", "OrganizationReadOnly", "ProjectReadOnly"}

// neverGranted is what no level may hold, the reason beside each in the
// package docs: IAM and organization writes, billing, secrets and keys, SSH
// keys, Cockpit token minting, object contents and their public exposure,
// domain registrar writes, Kubernetes' system:masters and the opaque bundles.
//
//nolint:gochecknoglobals // an immutable list, not state
var neverGranted = []string{
	"IAMManager", "IAMApplicationManager", "IAMPolicyManager", "IAMUserManager", "IAMGroupManager",
	"IAMReadOnly", "OrganizationManager", "ProjectManager", "BillingManager", "BillingReadOnly",
	"SecretManagerFullAccess", "SecretManagerSecretAccess", "SecretManagerSecretCreate",
	"SecretManagerSecretWrite", "SecretManagerSecretDelete", "KeyManagerFullAccess",
	"KeyManagerKeyDecrypt", "KeyManagerKeyCreate", "SSHKeysFullAccess", "ObservabilityFullAccess",
	"ObjectStorageFullAccess", "ObjectStorageReadOnly", "ObjectStorageObjectsRead",
	"ObjectStorageObjectsWrite", "ObjectStorageBucketsWrite", "ObjectStorageBucketPolicyFullAccess",
	"DomainsDNSFullAccess", "KubernetesSystemMastersGroupAccess", "AllProductsFullAccess",
	"AllProductsReadOnly",
}

// provisionerSets is what the provisioner's own policy must grant on the
// organization: applications and their keys, policies, and the projects a
// seat may name.
//
//nolint:gochecknoglobals // an immutable list, not state
var provisionerSets = []string{"IAMApplicationManager", "IAMPolicyManager", "ProjectReadOnly"}

// standsIn is, for each provisioner set, the wider sets that hold it, in
// Scaleway's own catalogue words: IAMManager is "full access to IAM and
// ProjectManager permissions", and OrganizationManager manages projects but
// not IAM.
//
//nolint:gochecknoglobals // an immutable table, not state
var standsIn = map[string][]string{
	"IAMApplicationManager": {"IAMManager"},
	"IAMPolicyManager":      {"IAMManager"},
	"ProjectReadOnly":       {"IAMManager", "ProjectManager", "OrganizationManager"},
}

// Levels is every seat level, least access first.
func Levels() []string { return config.ScalewayLevels() }

// levelOf is the level with id, or false.
func levelOf(id string) (level, bool) {
	i := slices.IndexFunc(levels, func(l level) bool { return l.id == id })
	if i < 0 {
		return level{}, false
	}
	return levels[i], true
}

// LevelName is a level as a person reads it.
func LevelName(id string) string {
	if l, ok := levelOf(id); ok {
		return l.name
	}
	return id
}

// LevelNote is the sentence saying what a level grants.
func LevelNote(id string) string {
	if l, ok := levelOf(id); ok {
		return l.note
	}
	return ""
}

// Administers reports a level that is an administration grant.
func Administers(id string) bool {
	l, ok := levelOf(id)
	return ok && l.administers
}

// ProjectSets is the project-scoped sets a level grants, its own and every
// lower level's, sorted, and false for a level this build does not offer.
func ProjectSets(id string) ([]string, bool) {
	i := slices.IndexFunc(levels, func(l level) bool { return l.id == id })
	if i < 0 {
		return nil, false
	}
	var out []string
	for _, l := range levels[:i+1] {
		out = append(out, l.adds...)
	}
	slices.Sort(out)
	return slices.Compact(out), true
}

// OrgSets is what every level grants across the organization, sorted.
func OrgSets() []string { return slices.Clone(orgSets) }

// rulesFor is a seat's policy at a level on its projects: one rule with the
// level's sets on the projects, one with the organization reads.
func rulesFor(levelID, org string, projects []string) ([]RuleSpec, error) {
	sets, ok := ProjectSets(levelID)
	if !ok {
		return nil, fmt.Errorf("%q is not a Scaleway level; give %s", levelID, words.Or(Levels()))
	}
	scope := normalProjects(projects)
	if len(scope) == 0 {
		return nil, fmt.Errorf("the agent names no project, so its policy would grant nothing")
	}
	return []RuleSpec{
		{PermissionSetNames: sets, ProjectIDs: scope},
		{PermissionSetNames: OrgSets(), OrganizationID: org},
	}, nil
}

// normalProjects is projects as Scaleway matches them, each once and sorted.
func normalProjects(projects []string) []string {
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		if id := config.ScalewayID(p); id != "" {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// heldBy is what rules grant: the level they are exactly, "" where they are
// none, and the projects the project-scoped rules name.
func heldBy(rules []Rule) (string, []string) {
	var project, org, projects []string
	for _, r := range rules {
		if r.OnOrganization() {
			org = append(org, r.PermissionSetNames...)
			continue
		}
		project = append(project, r.PermissionSetNames...)
		projects = append(projects, r.ProjectIDs...)
	}
	slices.Sort(project)
	slices.Sort(org)
	project, org = slices.Compact(project), slices.Compact(org)
	projects = normalProjects(projects)
	for _, id := range Levels() {
		sets, _ := ProjectSets(id)
		if slices.Equal(project, sets) && slices.Equal(org, OrgSets()) {
			return id, projects
		}
	}
	return "", projects
}

// unlisted is, by level, the sets the organization's catalogue does not offer
// at the scope the level grants them at.
func unlisted(catalogue []PermissionSet) map[string][]string {
	scope := map[string]string{}
	for _, s := range catalogue {
		scope[s.Name] = s.ScopeType
	}
	out := map[string][]string{}
	for _, id := range Levels() {
		sets, _ := ProjectSets(id)
		for _, name := range sets {
			if scope[name] != "projects" {
				out[id] = append(out[id], name)
			}
		}
		for _, name := range OrgSets() {
			if scope[name] != "organization" {
				out[id] = append(out[id], name)
			}
		}
	}
	return out
}
