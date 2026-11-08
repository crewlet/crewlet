package capability

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A DEFAULT TOOL LEVEL'S CHANGE IS TOLD. A server's default level is every
// unnamed tool's level, so a default moving from allow to approve is a
// change of what the seat may run without asking: the digest moves and
// exactly one tool-level line says so.
//
// Mutation: rebuild a server in toolServers without its Default and the two
// snapshots are one.
func TestADefaultToolLevelChangeIsTold(t *testing.T) {
	t.Parallel()
	server := func(level string) Snapshot {
		return bound(Snapshot{V: Version, MCP: []ToolServer{{Server: "sentry",
			Allow: []string{"list_issues"}, Default: level}}})
	}
	was, is := server(string(config.MCPToolAllow)), server(string(config.MCPToolApprove))
	if was.Digest() == is.Digest() {
		t.Fatal("a default level's change leaves the digest where it was")
	}
	changes := Diff(was, is)
	if len(changes) != 1 || changes[0].Kind != ChangeToolLevel ||
		!strings.Contains(changes[0].Line, "every other tool now needs a person's approval") {
		t.Fatalf("the change is told as %+v, want one tool-level line", changes)
	}
}

// largestSnapshot is every lane granted with lists and words far past what
// any company document holds: names and notes past their bounds, a scope of a
// thousand long entries with long labels in a three-byte script, hundreds of
// tool servers with hundreds of tools at each level, and more policies and
// setup steps than a company keeps, each at its longest.
func largestSnapshot(scopeTail string) Snapshot {
	long := func(prefix string, n int) string { return prefix + strings.Repeat("வ", n) }
	quoting := strings.Repeat(`"\`, 300)
	var keys []string
	permissions, names := map[string]string{}, map[string]string{}
	for _, key := range config.GitHubPermissionKeys() {
		keys = append(keys, key)
		permissions[key] = "write"
		names[key] = long("p", 40)
	}
	pending := strings.Join(func() []string {
		out := make([]string, 0, len(keys))
		for _, key := range keys {
			out = append(out, key+":write")
		}
		return out
	}(), ",")
	s := Snapshot{V: Version}
	for _, lane := range seataccess.Lanes() {
		in := Integration{Kind: string(lane.Kind()), Lane: string(lane), On: true,
			Level: long("level", 300), LevelName: long("name", 300), LevelNote: long("note", 2000),
			Tool: string(seataccess.ToolLive), ScopeLabels: map[string]string{}}
		for i := range 1000 {
			entry := fmt.Sprintf("%s-%04d", long("scope", 200), i)
			if i == 999 {
				entry += scopeTail
			}
			in.Scope = append(in.Scope, entry)
			in.ScopeLabels[entry] = long("label", 400)
		}
		if lane == seataccess.LaneGitHub {
			in.Permissions, in.PermissionNames, in.Pending = permissions, names, pending
		}
		s.Integrations = append(s.Integrations, in)
		s.NotGranted = append(s.NotGranted, string(lane))
	}
	for i := range 300 {
		ts := ToolServer{Server: fmt.Sprintf("%s-%03d", long("server", 200), i),
			Default: string(config.MCPToolApprove)}
		for j := range 300 {
			tool := fmt.Sprintf("%s-%03d", long("tool", 200), j)
			ts.Allow, ts.Approve, ts.Blocked = append(ts.Allow, tool+"a"), append(ts.Approve, tool+"b"),
				append(ts.Blocked, tool+"c")
		}
		s.MCP = append(s.MCP, ts)
	}
	for i := range 200 {
		text := fmt.Sprintf("%d %s %s", i, quoting, long("policy", 4000))
		s.Policies = append(s.Policies, Policy{Digest: digestOf([]byte(text)), Text: excerpt(text)})
		s.Setup = append(s.Setup, SetupStep{From: SetupFromSeat, Name: long(fmt.Sprintf("step-%03d", i), 300),
			Brief: digestOf([]byte(text))})
	}
	return s
}

// THE LARGEST SNAPSHOT FITS ITS RECORD. However much a company grants a seat,
// its snapshot encodes under MaxSnapshotBytes, which is under the access log's
// own bound on one, so a told record is never refused for its size; bounding
// is idempotent, and a change past every cap still moves the digest and is
// told.
//
// Mutation: drop the compaction levels from bound and the snapshot is
// megabytes; drop the folded digests and the change in the tail is lost.
func TestTheLargestSnapshotFitsItsRecord(t *testing.T) {
	t.Parallel()
	if MaxSnapshotBytes > accessdomain.SnapshotMaxBytes {
		t.Fatalf("a snapshot may encode to %d bytes and a told record holds %d", MaxSnapshotBytes,
			accessdomain.SnapshotMaxBytes)
	}
	big := bound(largestSnapshot(""))
	if n := len(big.JSON()); n > MaxSnapshotBytes {
		t.Fatalf("the largest snapshot encodes to %d bytes, past %d", n, MaxSnapshotBytes)
	}
	if again := bound(big); string(again.JSON()) != string(big.JSON()) {
		t.Error("bounding a bounded snapshot moves it")
	}
	moved := bound(largestSnapshot("-moved"))
	if moved.Digest() == big.Digest() {
		t.Fatal("a scope entry past every cap changed and the digest did not")
	}
	if changes := Diff(big, moved); len(changes) == 0 {
		t.Error("a scope entry past every cap changed and nothing is told")
	}
	if YourAccess(big) == "" {
		t.Error("the largest snapshot renders no Your access section")
	}
}

// A SNAPSHOT SHORTENED TO FIT IS COMPARED AT ITS OWN LEVEL. A seat whose last
// told snapshot fit whole, and whose snapshot now has to be shortened, is not
// told the entries the shorter one leaves out as gone.
//
// Mutation: diff the two levels as they are and every entry past four is told
// as removed.
func TestAShortenedSnapshotIsComparedAtItsOwnLevel(t *testing.T) {
	t.Parallel()
	small := Snapshot{V: Version, Integrations: []Integration{{Kind: "github", Lane: "github", On: true,
		Scope: []string{"acme/a", "acme/b", "acme/c", "acme/d", "acme/e", "acme/f"}, Tool: "live"}}}
	whole := bound(small)
	short := whole.at(1)
	if whole.Compact != 0 || short.Compact != 1 || len(short.Integrations[0].Scope) != compactCap {
		t.Fatalf("the premise: levels %d and %d, %d entries shown", whole.Compact, short.Compact,
			len(short.Integrations[0].Scope))
	}
	if changes := Diff(whole, short); len(changes) != 0 {
		t.Errorf("one access at two levels is told as %+v", changes)
	}
}
