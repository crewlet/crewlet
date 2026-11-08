package capability_test

import (
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

func on() *bool { v := true; return &v }

// cloudflareWords are a fixed-level vendor's words, as its catalog hands them.
var cloudflareWords = capability.Words{Levels: []seataccess.Level{
	{ID: "read", Name: "Read", Note: "Reads zones."},
	{ID: "operate", Name: "Operate", Note: "Everything Read does, plus purging the cache."},
	{ID: "configure", Name: "Configure", Note: "Everything Operate does, plus zone settings."},
	{ID: "secure", Name: "Secure", Note: "Everything Configure does, plus WAF rules."},
}}

// githubWords are GitHub's permissions, as its catalog hands them.
var githubWords = capability.Words{Permissions: []seataccess.PermissionSpec{
	{Key: "contents", Name: "Contents"}, {Key: "checks", Name: "Checks"},
	{Key: "statuses", Name: "Commit statuses"}, {Key: "workflows", Name: "Workflows", WriteOnly: true},
}}

// company is one with GitHub, Cloudflare and Datadog connected and an SRE seat
// on at the first two.
func company() *config.Company {
	return &config.Company{
		Name:     "Example",
		Mission:  "Ship it.",
		Policies: []string{"Only the SRE operates production.", "Every change is reviewed."},
		Integrations: config.Integrations{
			GitHub: &config.GitHub{}, Cloudflare: &config.Cloudflare{}, Datadog: &config.Datadog{},
		},
		SeatAccessMigrated: []string{"github", "cloudflare", "datadog"},
		Roles: []config.Role{{
			Name: "Site Reliability Engineer", Handle: "sre", Kind: org.KindAgent,
			Integrations: config.RoleIntegrations{
				GitHub: &config.RoleGitHub{Enabled: on(), Repos: []string{"acme/web", "acme/api"},
					Permissions: map[string]config.GitHubAccess{
						"contents": config.GitHubWrite, "checks": config.GitHubRead, "statuses": config.GitHubRead,
					}},
				Cloudflare: &config.RoleCloudflare{Enabled: on(), Level: "operate"},
			},
		}},
	}
}

// input is the SRE's input: both accounts made, GitHub's app not yet allowed
// commit statuses.
func input(c *config.Company) capability.Input {
	return capability.Input{
		Company: c, Handle: "sre",
		Observed: map[seataccess.Lane]seataccess.Observed{
			seataccess.LaneGitHub: {Exists: true, Enabled: true,
				Ceiling: seataccess.Permissions{"contents": config.GitHubWrite, "checks": config.GitHubRead}},
			seataccess.LaneCloudflare: {Exists: true, Enabled: true,
				Credential: &seataccess.Credential{Variable: "CLOUDFLARE_SEAT_SRE", MintedAt: time.Unix(1, 0)}},
		},
		Words: map[seataccess.Lane]capability.Words{
			seataccess.LaneGitHub: githubWords, seataccess.LaneCloudflare: cloudflareWords,
		},
		Tools: []capability.ToolServer{{Server: "sentry", Allow: []string{"list_issues"},
			Approve: []string{"resolve_issue"}}},
	}
}

func TestASnapshotReadsWhatTheSeatReaches(t *testing.T) {
	t.Parallel()
	s := capability.Take(input(company()))
	if len(s.Integrations) != 2 || s.Integrations[0].Lane != "cloudflare" || s.Integrations[1].Lane != "github" {
		t.Fatalf("integrations = %+v, want cloudflare then github", s.Integrations)
	}
	cf, gh := s.Integrations[0], s.Integrations[1]
	if !cf.On || cf.Level != "operate" || cf.LevelName != "Operate" || cf.LevelNote == "" {
		t.Errorf("cloudflare = %+v", cf)
	}
	if gh.Pending != "statuses:read" || gh.Permissions["statuses"] != "" || gh.Permissions["contents"] != "write" {
		t.Errorf("github = %+v, want statuses pending and not in effect", gh)
	}
	if !slices.Equal(gh.Scope, []string{"acme/api", "acme/web"}) {
		t.Errorf("github scope = %v, want sorted", gh.Scope)
	}
	if !slices.Equal(s.NotGranted, []string{"datadog"}) {
		t.Errorf("not granted = %v", s.NotGranted)
	}
	text := capability.YourAccess(s)
	for _, want := range []string{
		"## Your access",
		"- Cloudflare: Operate. Everything Read does, plus purging the cache.",
		"- GitHub (acme/api, acme/web): Checks Read-only, Contents Read and write.",
		"Requested and not in effect yet, until an organization owner accepts the app's new permissions: Commit statuses Read-only.",
		"Not granted: Workflows.",
		"- Not granted to you: Datadog.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Your access lacks %q:\n%s", want, text)
		}
	}
}

// THE SECTION IS ABSENT FOR A COMPANY THAT CONNECTS NO INTEGRATION, and a
// company still moving an integration onto per-seat access says nothing of it.
func TestYourAccessIsAbsentWithNoIntegration(t *testing.T) {
	t.Parallel()
	c := company()
	c.Integrations = config.Integrations{}
	if got := capability.YourAccess(capability.Take(input(c))); got != "" {
		t.Errorf("Your access = %q, want none", got)
	}
	c = company()
	c.SeatAccessMigrated = []string{"github"}
	s := capability.Take(input(c))
	if len(s.Integrations) != 1 || len(s.NotGranted) != 0 {
		t.Errorf("snapshot = %+v, want only github: the others are not per seat yet", s)
	}
}

// THE SYSTEM PROMPT CARRIES THE SECTION ON EVERY TURN, right after the
// policies, as YourAccess renders it.
func TestYourAccessIsInEverySystemPrompt(t *testing.T) {
	t.Parallel()
	access := capability.YourAccess(capability.Take(input(company())))
	seat := prompts.Seat{Role: &org.Role{Name: "Site Reliability Engineer"},
		Org: &org.Organization{Name: "Example", Policies: []string{"Only the SRE operates production."}}}
	for _, in := range []prompts.ExecutorInput{
		{Access: access},
		{Access: access, PersonalMemory: "- a note", ThreadContext: "- a message"},
	} {
		p := prompts.BuildExecutor(seat, in)
		if !strings.Contains(p.Text, "\n"+access) {
			t.Fatalf("the system prompt lacks Your access:\n%s", p.Text)
		}
		keys := keysOf(p)
		i := slices.Index(keys, prompts.KeyAccess)
		if i < 1 || keys[i-1] != "policies" {
			t.Errorf("outline = %v, want %q right after the policies", keys, prompts.KeyAccess)
		}
	}
	if p := prompts.BuildExecutor(seat, prompts.ExecutorInput{}); slices.Contains(keysOf(p), prompts.KeyAccess) {
		t.Errorf("a prompt with no access rendered the section: %v", keysOf(p))
	}
}

// keysOf is a prompt's outline, by key.
func keysOf(p prompts.Prompt) []string {
	out := make([]string, 0, len(p.Sections))
	for _, s := range p.Sections {
		out = append(out, s.Key)
	}
	return out
}

// NOTHING BUT ACCESS MOVES THE DIGEST: a rotation, the mission, a profile,
// timestamps and the order lists were written in leave the section's bytes
// as they were, and a level moves both.
func TestYourAccessIsByteStableWhileAccessIs(t *testing.T) {
	t.Parallel()
	base := capability.Take(input(company()))
	same := []func(*config.Company, *capability.Input){
		func(c *config.Company, _ *capability.Input) { c.Mission, c.Vision = "Ship more.", "Everywhere." },
		func(c *config.Company, _ *capability.Input) {
			c.Roles[0].Goal, c.Roles[0].Backstory = "Keep it up.", "Ran a pager."
		},
		func(c *config.Company, _ *capability.Input) {
			c.Policies = []string{c.Policies[1], c.Policies[0]}
			c.Roles[0].Integrations.GitHub.Repos = []string{"acme/api", "acme/web", "acme/api"}
		},
		func(_ *config.Company, in *capability.Input) {
			cf := in.Observed[seataccess.LaneCloudflare]
			cf.Credential = &seataccess.Credential{Variable: "CLOUDFLARE_SEAT_SRE", MintedAt: time.Unix(9_999, 0)}
			cf.CreatedAt, cf.ChangedByHandAt = time.Unix(5, 0), time.Unix(6, 0)
			cf.Retired = []seataccess.Retired{{Label: "old-token", Since: time.Unix(2, 0)}}
			in.Observed[seataccess.LaneCloudflare] = cf
		},
	}
	for i, change := range same {
		c := company()
		in := input(c)
		change(c, &in)
		got := capability.Take(in)
		if got.Digest() != base.Digest() || capability.YourAccess(got) != capability.YourAccess(base) {
			t.Errorf("variant %d moved the digest:\n%s\n%s", i, base.JSON(), got.JSON())
		}
	}
	c := company()
	c.Roles[0].Integrations.Cloudflare.Level = "secure"
	moved := capability.Take(input(c))
	if moved.Digest() == base.Digest() || capability.YourAccess(moved) == capability.YourAccess(base) {
		t.Errorf("a level change left the digest and the section as they were")
	}
	parsed, err := capability.Parse(base.JSON())
	if err != nil || parsed.Digest() != base.Digest() {
		t.Errorf("a parsed snapshot's digest moved: %v", err)
	}
}

// secret is a value no snapshot may ever hold.
func secret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "sealed" + hex.EncodeToString(b)
}

// EVERY PLACE A CREDENTIAL LIVES IS FED ONE, and none reaches the snapshot,
// either section, or a change line.
func TestASnapshotHoldsNoCredential(t *testing.T) {
	t.Parallel()
	for range 32 {
		var planted []string
		plant := func() string {
			s := secret(t)
			planted = append(planted, s)
			return s
		}
		ref := func() string {
			s := strings.ToUpper(secret(t))
			planted = append(planted, s)
			return "${" + s + "}"
		}
		c := company()
		c.Integrations.Cloudflare.Provisioning = &config.CloudflareProvisioning{APIToken: plant()}
		c.Integrations.GitHub.WebhookSecret = ref()
		c.SkillVariables = map[string]string{"TOKEN": plant()}
		c.MCPServers = []config.MCPServer{{Name: "sentry", Env: map[string]string{"KEY": plant()},
			Headers: map[string]string{"Authorization": ref()}}}
		c.Providers.Sandbox = &config.SandboxProvider{Setup: []config.SandboxSetupStep{{
			Name: "git-auth", Brief: "Git pushes with your own token.",
			Files:    map[string]string{"/home/.git-credentials": plant()},
			Env:      map[string]string{"GIT_TOKEN": ref()},
			Commands: []string{"git config --global credential.helper " + plant()},
		}}}
		c.Roles[0].Sandbox = &config.RoleSandbox{Env: map[string]string{"X": plant()},
			Setup: []config.SandboxSetupStep{{Name: "sre-tools", Files: map[string]string{"/a": plant()}}}}
		in := input(c)
		cf := in.Observed[seataccess.LaneCloudflare]
		cf.Credential = &seataccess.Credential{Variable: plant(), Others: []string{plant()}, By: plant()}
		cf.Retired = []seataccess.Retired{{Label: plant()}}
		in.Observed[seataccess.LaneCloudflare] = cf

		s := capability.Take(in)
		rendered := string(s.JSON()) + capability.YourAccess(s) +
			capability.ChangesBlock(capability.Diff(capability.Snapshot{}, s)) +
			capability.ChangesBlock(capability.Diff(s, capability.Snapshot{}))
		for _, p := range planted {
			if strings.Contains(rendered, p) {
				t.Fatalf("a credential reached the snapshot or its text: %s\n%s", p, rendered)
			}
		}
		if !strings.Contains(string(s.JSON()), `"name":"git-auth"`) {
			t.Fatalf("the setup step's name is missing, so the property tested nothing: %s", s.JSON())
		}
	}
}

// told is the snapshot the SRE was last told, with cloudflare at level.
func told(level string) capability.Snapshot {
	c := company()
	c.Roles[0].Integrations.Cloudflare.Level = level
	return capability.Take(input(c))
}

// A REVERT IS A CHANGE BACK, against what the seat was told; and two
// revisions that cancel out before the seat's next turn are no change.
func TestARevertIsToldAsAChangeBack(t *testing.T) {
	t.Parallel()
	changes := capability.Diff(told("secure"), told("operate"))
	if len(changes) != 1 || changes[0].Kind != capability.ChangeLevel ||
		changes[0].Line != "Cloudflare: your level is now Operate (was Secure). Everything Read does, plus purging the cache." {
		t.Fatalf("changes = %+v, want the level back to Operate", changes)
	}
	if changes := capability.Diff(told("operate"), told("operate")); len(changes) != 0 {
		t.Errorf("revisions that cancel out were told: %+v", changes)
	}
}

// A ROTATION IS NO CHANGE: a new credential at the same access.
func TestARotationIsNoChange(t *testing.T) {
	t.Parallel()
	before := capability.Take(input(company()))
	in := input(company())
	cf := in.Observed[seataccess.LaneCloudflare]
	cf.Credential = &seataccess.Credential{Variable: "CLOUDFLARE_SEAT_SRE", MintedAt: time.Unix(86_400, 0)}
	in.Observed[seataccess.LaneCloudflare] = cf
	after := capability.Take(in)
	if changes := capability.Diff(before, after); len(changes) != 0 || before.Digest() != after.Digest() {
		t.Errorf("a rotation was a change: %+v", changes)
	}
}

func TestEveryKindOfChangeIsTold(t *testing.T) {
	t.Parallel()
	before := capability.Take(input(company()))
	c := company()
	c.Roles[0].Integrations.Cloudflare.Enabled = nil
	c.Roles[0].Integrations.Datadog = &config.RoleDatadog{Enabled: on(), Role: "r1"}
	c.Roles[0].Integrations.GitHub.Repos = []string{"acme/api", "acme/docs"}
	c.Roles[0].Integrations.GitHub.Permissions["contents"] = config.GitHubRead
	c.Policies = []string{"Only the SRE operates production.", "Deploys need a person's approval."}
	c.Providers.Sandbox = &config.SandboxProvider{Setup: []config.SandboxSetupStep{{Name: "sre-tools"}}}
	in := input(c)
	gh := in.Observed[seataccess.LaneGitHub]
	gh.Ceiling = seataccess.Permissions{"contents": config.GitHubWrite, "checks": config.GitHubRead,
		"statuses": config.GitHubRead}
	in.Observed[seataccess.LaneGitHub] = gh
	in.Observed[seataccess.LaneDatadog] = seataccess.Observed{Exists: true, Enabled: true}
	in.Tools = []capability.ToolServer{{Server: "sentry", Approve: []string{"list_issues", "resolve_issue"}},
		{Server: "linear", Allow: []string{"search"}}}
	changes := capability.Diff(before, capability.Take(in))
	var lines []string
	for _, ch := range changes {
		if !ch.Kind.Valid() {
			t.Errorf("change kind %q is not in the closed list", ch.Kind)
		}
		lines = append(lines, ch.Line)
	}
	want := []string{
		"Cloudflare: no longer granted to you.",
		"Datadog: you now have your own account at r1.",
		"GitHub: Contents is now Read-only (was Read and write).",
		"GitHub: you can now work in acme/docs.",
		"GitHub: you can no longer work in acme/web.",
		"GitHub: Commit statuses Read-only is now in effect.",
		"New tool server: linear.",
		"sentry: 1 tool now needs a person's approval before each call (list_issues).",
		`Policy added: "Deploys need a person's approval."`,
		`Policy removed: "Every change is reviewed."`,
		"Box setup added: sre-tools.",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// THE BLOCK SAYS WHAT THE LINES REPLACE, and shows at most twenty of them.
func TestTheChangesBlockIsBounded(t *testing.T) {
	t.Parallel()
	if capability.ChangesBlock(nil) != "" {
		t.Error("no change rendered a block")
	}
	var changes []capability.Change
	for i := range 23 {
		changes = append(changes, capability.Change{Kind: capability.ChangePolicyAdded,
			Line: strings.Repeat("x", i)})
	}
	block := capability.ChangesBlock(changes)
	lines := strings.Split(block, "\n")
	if lines[0] != capability.ChangesHeading || len(lines) != 2+capability.MaxLines+1 {
		t.Fatalf("block has %d lines:\n%s", len(lines), block)
	}
	if got := lines[len(lines)-1]; got != `- (and 3 more changes: the rest is under "Your access")` {
		t.Errorf("tail = %q", got)
	}
	long := capability.Diff(capability.Snapshot{}, capability.Snapshot{V: 1,
		Policies: []capability.Policy{{Digest: "sha256:1", Text: strings.Repeat("policy words ", 60)}}})
	if len(long) != 1 || len(long[0].Line) > capability.MaxLineBytes {
		t.Errorf("a change line is past %d bytes: %d", capability.MaxLineBytes, len(long[0].Line))
	}
}

func TestATermOfAChangeIsWhatANoteNamesItBy(t *testing.T) {
	t.Parallel()
	level := capability.Change{Kind: capability.ChangeLevel, Vendor: "Cloudflare", Lane: "cloudflare",
		From: "Operate", To: "Secure"}
	if got := level.Terms(); !slices.Equal(got, []string{"Cloudflare", "Operate", "Secure"}) {
		t.Errorf("level terms = %v", got)
	}
	scope := capability.Change{Kind: capability.ChangeScopeRemoved, Vendor: "Google Cloud",
		Lane: "google_cloud", Items: []string{"prod-1"}}
	if got := scope.Terms(); !slices.Equal(got, []string{"Google Cloud", "google_cloud", "prod-1"}) {
		t.Errorf("scope terms = %v", got)
	}
	if got := (capability.Change{Kind: capability.ChangePolicyAdded, Vendor: "Policy"}).Terms(); got != nil {
		t.Errorf("a policy change has terms %v: no note is about a policy's access", got)
	}
}

// sshCompany is one whose SRE seat reads api-1, which runs containers, and
// not db-1.
func sshCompany() *config.Company {
	c := company()
	c.Integrations.SSH = &config.SSH{Enabled: true, Hosts: []config.SSHHost{
		{ID: "api-1", Address: "api-1.example.com", Containers: true},
		{ID: "db-1", Address: "db-1.example.com", ReadAllowlist: []string{"/usr/bin/psql --version"}},
	}}
	c.SeatAccessMigrated = append(c.SeatAccessMigrated, "ssh")
	c.Roles[0].Integrations.SSH = &config.RoleSSH{Enabled: on(), Hosts: []string{"api-1"}}
	return c
}

// A SEAT IS TOLD THE READ COMMANDS EACH OF ITS HOSTS ADMITS, as it types them:
// bare program names, placeholders for the words a rule leaves open, and no
// host it was not granted.
//
// Mutation: drop the commands from the SSH lane and the section names hosts
// only, as before the seat could tell a refused spelling from a refused
// command.
func TestYourAccessListsEachHostsReadCommands(t *testing.T) {
	t.Parallel()
	in := input(sshCompany())
	in.Observed[seataccess.LaneSSH] = seataccess.Observed{Exists: true, Enabled: true}
	got := capability.YourAccess(capability.Take(in))
	for _, want := range []string{"Read commands on api-1: `uptime`", "`sudo -n docker ps`",
		"`sudo -n docker logs --tail <n> <container>`", "`systemctl status <unit>`"} {
		if !strings.Contains(got, want) {
			t.Errorf("Your access does not say %q:\n%s", want, got)
		}
	}
	for _, never := range []string{"db-1", "psql", "/usr/bin/"} {
		if strings.Contains(got, never) {
			t.Errorf("Your access says %q:\n%s", never, got)
		}
	}
}

// A CHANGED ALLOWLIST MOVES THE DIGEST AND IS TOLD BY THE NEXT SECTION: the
// commands are part of the snapshot, so the seat is never left with a list
// its host no longer admits, and no change line is invented for it.
//
// Mutation: leave the commands out of the snapshot's encoding and the digest
// stays put.
func TestAChangedAllowlistMovesTheDigest(t *testing.T) {
	t.Parallel()
	in := input(sshCompany())
	in.Observed[seataccess.LaneSSH] = seataccess.Observed{Exists: true, Enabled: true}
	was := capability.Take(in)
	changed := sshCompany()
	changed.Integrations.SSH.Hosts[0].Containers = false
	in.Company = changed
	now := capability.Take(in)
	if was.Digest() == now.Digest() {
		t.Fatal("the host stopped admitting docker and the digest did not move")
	}
	if strings.Contains(capability.YourAccess(now), "docker") {
		t.Error("Your access still lists docker after the host stopped admitting it")
	}
	if changes := capability.Diff(was, now); len(changes) != 0 {
		t.Errorf("an allowlist change is told as %+v", changes)
	}
	if again := capability.Take(in); again.Digest() != now.Digest() {
		t.Error("one access takes two digests")
	}
}

// AN AUDIENCE IS ONE OF THE TWO THIS BUILD WORDS LINES FOR: the seat and a
// person are valid, anything else is not, and a line is never worded for it.
//
// Mutation: word an unknown audience as the seat and a person reads "your".
func TestAnAudienceIsOneThisBuildWordsLinesFor(t *testing.T) {
	t.Parallel()
	for _, a := range []capability.Audience{capability.ForSeat, capability.ForPerson} {
		if !a.Valid() {
			t.Errorf("audience %q reads as not valid", a)
		}
	}
	for _, a := range []capability.Audience{"", "admin"} {
		if a.Valid() {
			t.Errorf("audience %q reads as valid", a)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("a change was worded for audience %q", a)
				}
			}()
			capability.DiffFor(told("secure"), told("operate"), a)
		}()
	}
}

// A PERSON READS A CHANGE ABOUT THE SEAT, NOT ABOUT THEMSELVES: the same
// change worded for a person names the seat in the third person, from one
// formatter, and says the same facts.
//
// Mutation: hand a person the seat's own line and they read "your level".
func TestAPersonReadsAChangeAboutTheSeat(t *testing.T) {
	t.Parallel()
	seat := capability.DiffFor(told("secure"), told("operate"), capability.ForSeat)
	person := capability.DiffFor(told("secure"), told("operate"), capability.ForPerson)
	if len(seat) != 1 || len(person) != 1 || seat[0].Kind != person[0].Kind {
		t.Fatalf("the two audiences were told %+v and %+v, want one change each", seat, person)
	}
	if !strings.Contains(seat[0].Line, "your level is now Operate (was Secure)") ||
		!strings.Contains(person[0].Line, "its level is now Operate (was Secure)") {
		t.Errorf("the seat reads %q and a person %q", seat[0].Line, person[0].Line)
	}
	for _, c := range capability.DiffFor(capability.Take(input(company())), told("read"), capability.ForPerson) {
		for _, word := range []string{" you ", " your ", "you are", "you now", "to you"} {
			if strings.Contains(" "+c.Line+" ", word) {
				t.Errorf("a person's line %q says %q", c.Line, strings.TrimSpace(word))
			}
		}
	}
}
