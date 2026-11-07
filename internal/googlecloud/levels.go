package googlecloud

import (
	"regexp"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/words"
)

// What each seat level grants, by Google's own role names. Each level's roles
// are one hasOnly list on its own conditional binding of the provisioner, at
// most ten (Google's limit), and every level holds the lists below it. A write
// touches one list only, so it is allowed by exactly one condition.

// CustomRoleID is the owner's custom role on each project that Operator holds:
// restart and scale, which no predefined role grants without a credential.
const CustomRoleID = "crewletOperator"

// level is one seat level: the ID a document names, how a person reads it, and
// whether it is an administration grant ([seataccess.Level.Administers]).
type level struct {
	id, name, note string
	administers    bool
}

// levels is every seat level, least access first. The names and notes are
// Google's own role titles, as its IAM page shows them on the agent's row.
//
//nolint:gochecknoglobals // an immutable table, not state
var levels = []level{
	{
		id: config.GoogleCloudLevelViewer, name: "Viewer",
		note: "Browser, Cloud SQL Viewer, Compute Viewer, Kubernetes Engine Viewer, DNS Reader, Logs Viewer, " +
			"MCP Tool User, Monitoring Viewer, Cloud Run Viewer and Storage Bucket Viewer on each chosen project.",
	},
	{
		id: config.GoogleCloudLevelOperator, name: "Operator", administers: true,
		note: "Viewer's roles, plus Monitoring Editor, Cloud Run Jobs Executor, Storage Object User and " +
			"Crewlet Operator (restart and scale only).",
	},
	{
		id: config.GoogleCloudLevelAdminister, name: "Administer", administers: true,
		note: "Operator's roles, plus Cloud SQL Admin, Compute Instance Admin (v1), Compute Network Admin, " +
			"Compute Security Admin, Kubernetes Engine Cluster Admin, DNS Administrator and Storage Admin. " +
			"It can create access that outlives the agent's token: SSH keys or startup scripts on VMs, " +
			"database users and certificates, and bucket or DNS zone policies.",
	},
}

// viewerRoles is binding A: per-product viewer roles, never basic Viewer,
// which reads every API key's string.
//
//nolint:gochecknoglobals // an immutable list, not state
var viewerRoles = []string{
	"roles/browser",
	"roles/cloudsql.viewer",
	"roles/compute.viewer",
	"roles/container.viewer",
	"roles/dns.reader",
	"roles/logging.viewer",
	"roles/mcp.toolUser",
	"roles/monitoring.viewer",
	"roles/run.viewer",
	"roles/storage.bucketViewer",
}

// operatorPredefined and the project's own crewletOperator are binding B.
// Cloud SQL Editor is out: live, it holds cloudsql.sslCerts.create.
//
//nolint:gochecknoglobals // an immutable list, not state
var operatorPredefined = []string{
	"roles/monitoring.editor",
	"roles/run.jobsExecutor",
	"roles/storage.objectUser",
}

// administerRoles is binding C: roles kept below Administer only because they
// can create access on what the agent manages.
//
//nolint:gochecknoglobals // an immutable list, not state
var administerRoles = []string{
	"roles/cloudsql.admin",
	"roles/compute.instanceAdmin.v1",
	"roles/compute.networkAdmin",
	"roles/compute.securityAdmin",
	"roles/container.clusterAdmin",
	"roles/dns.admin",
	"roles/storage.admin",
}

// customRolePerms is exactly what crewletOperator holds, sorted: restart and
// scale, with no metadata, pod spec, exec, secret or certificate access.
//
//nolint:gochecknoglobals // an immutable list, not state
var customRolePerms = []string{
	"cloudsql.instances.restart",
	"compute.instanceGroupManagers.update",
	"compute.instances.reset",
	"compute.instances.resume",
	"compute.instances.start",
	"compute.instances.stop",
	"compute.instances.suspend",
	"container.deployments.updateScale",
	"container.pods.delete",
	"container.pods.evict",
	"container.replicaSets.updateScale",
	"container.statefulSets.updateScale",
}

// never is what no level may hold, Administer included: acting as or minting
// for another identity, granting on the project or an account, reading a
// secret store, running as a workload's identity, and billing.
//
// Compiled at first use, not at init: Gitlet's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
var never = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(neverPattern) })

// neverPattern is never's source.
const neverPattern = `^(` +
	`iam\.serviceAccounts\.(actAs|getAccessToken|getOpenIdToken|signBlob|signJwt|implicitDelegation|setIamPolicy)` +
	`|iam\.serviceAccountKeys\.create|iam\.roles\.(create|update)` +
	`|resourcemanager\.(projects|folders|organizations)\.setIamPolicy` +
	`|secretmanager\.versions\.access|cloudkms\.cryptoKeyVersions\.useToDecrypt|apikeys\.keys\.getKeyString` +
	`|storage\.hmacKeys\.create|run\.jobs\.runWithOverrides` +
	`|container\.(secrets\.(get|list)|serviceAccounts\.createToken|pods\.(create|exec|attach|portForward))` +
	`|container\.(deployments|statefulSets|daemonSets|replicaSets|jobs|cronJobs|replicationControllers|podTemplates)\.(create|update)` +
	`|billing\..*` +
	`)$`

// credential is what Viewer and Operator may not hold either: access on what
// the agent manages that outlives its token, which only Administer may create.
// It is compiled at first use, as never is.
var credential = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(credentialPattern) })

// credentialPattern is credential's source.
const credentialPattern = `^(` +
	`.*\.setIamPolicy` +
	`|compute\.instances\.(setMetadata|setServiceAccount)|compute\.projects\.setCommonInstanceMetadata` +
	`|container\.clusters\.getCredentials|cloudsql\.(users\.(create|update)|sslCerts\.create)` +
	`)$`

// Levels is every seat level, least access first.
func Levels() []string { return config.GoogleCloudLevels() }

// levelOf is the level with id, its place in the table, or false.
func levelOf(id string) (level, int, bool) {
	i := slices.IndexFunc(levels, func(l level) bool { return l.id == id })
	if i < 0 {
		return level{}, -1, false
	}
	return levels[i], i, true
}

// LevelName is a level as a person reads it.
func LevelName(id string) string {
	if l, _, ok := levelOf(id); ok {
		return l.name
	}
	return id
}

// Administers reports a level that is an administration grant.
func Administers(id string) bool {
	l, _, ok := levelOf(id)
	return ok && l.administers
}

// CustomRole is project's crewletOperator by its full name.
func CustomRole(project string) string { return "projects/" + project + "/roles/" + CustomRoleID }

// CustomRolePermissions is exactly what crewletOperator must hold, sorted.
func CustomRolePermissions() []string { return slices.Clone(customRolePerms) }

// Lists is each level's own list on project, least access first: what each of
// the provisioner's three conditions names there.
func Lists(project string) [][]string {
	return [][]string{
		slices.Clone(viewerRoles),
		append(slices.Clone(operatorPredefined), CustomRole(project)),
		slices.Clone(administerRoles),
	}
}

// LevelRoles is every role a level grants on project, its own list and each
// below it, sorted, and false for a level this build does not offer.
func LevelRoles(id, project string) ([]string, bool) {
	_, i, ok := levelOf(id)
	if !ok {
		return nil, false
	}
	var out []string
	for _, list := range Lists(project)[:i+1] {
		out = append(out, list...)
	}
	slices.Sort(out)
	return out, true
}

// levelHeld is the level whose roles on project are exactly roles, "" where
// they are no level's.
func levelHeld(roles []string, project string) string {
	got := slices.Sorted(slices.Values(roles))
	for _, l := range levels {
		if want, _ := LevelRoles(l.id, project); slices.Equal(got, want) {
			return l.id
		}
	}
	return ""
}

// levelRole reports a role of some level's list on project.
func levelRole(role, project string) bool {
	for _, list := range Lists(project) {
		if slices.Contains(list, role) {
			return true
		}
	}
	return false
}

// predefinedRoles is every predefined role a level grants, sorted, by the level
// that first grants it: the roles whose permissions Google may change under
// the engine, which the credential rule is checked against.
func predefinedRoles() map[string]string {
	out := map[string]string{}
	for i, list := range [][]string{viewerRoles, operatorPredefined, administerRoles} {
		for _, role := range list {
			out[role] = levels[i].id
		}
	}
	return out
}

// barred is the permissions of a role that the level granting it may not
// hold: [never] at every level, and [credential] below Administer.
func barred(levelID string, permissions []string) []string {
	var out []string
	for _, p := range permissions {
		if never().MatchString(p) || (levelID != config.GoogleCloudLevelAdminister && credential().MatchString(p)) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// customRoleDiff is how a read of crewletOperator differs from its fixed
// list, "" where it holds exactly that.
func customRoleDiff(r Role) string {
	if r.Deleted {
		return "crewletOperator is deleted"
	}
	got := slices.Sorted(slices.Values(r.Permissions))
	if slices.Equal(got, customRolePerms) {
		return ""
	}
	var extra, missing []string
	for _, p := range got {
		if !slices.Contains(customRolePerms, p) {
			extra = append(extra, p)
		}
	}
	for _, p := range customRolePerms {
		if !slices.Contains(got, p) {
			missing = append(missing, p)
		}
	}
	switch {
	case len(extra) > 0 && len(missing) > 0:
		return "crewletOperator also holds " + words.And(extra) + " and lacks " + words.And(missing)
	case len(extra) > 0:
		return "crewletOperator also holds " + words.And(extra)
	default:
		return "crewletOperator lacks " + words.And(missing)
	}
}
