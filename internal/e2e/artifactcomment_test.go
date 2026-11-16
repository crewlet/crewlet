package e2e

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A person's comment on an agent's artifact wakes the agent, whose turn publishes
// a new version and resolves the thread citing it (ADR-0154). Mutation: leave the
// artifacts parser off the inbound edge, and no turn runs.
func TestAPersonsCommentIsAnsweredByTheAgentThatOwnsTheArtifact(t *testing.T) {
	t.Parallel()
	n := startNode(t, nodeSpec{boot: withSignIn})
	waitForSeat(t, n, "ceo")
	founder := person{address: janeAddress, login: janeLogin, name: "Jane Doe", password: janePassword,
		seat: janeSeat, grants: []iam.Grant{iam.GrantArtifacts}}
	tab := signedIn(t, n, founder)

	// THE AGENT'S DOC, shared with the founder as a commenter.
	deps := engine.ArtifactToolDeps(n.engine)
	if deps.Publisher == nil {
		t.Fatal("the premise: this node takes artifact uploads")
	}
	owner := artifactsdomain.Call{OpKey: statelog.NewOpID(harnessActivation, ""),
		Authority: artifacts.Authority{Actor: iam.Actor{Name: "ceo", Kind: iam.ActorAgent},
			Role: artifacts.RoleOwner, Limits: deps.Limits(), Origin: artifacts.OriginTool}}
	made, _, err := deps.Publisher.Create(t.Context(), owner, artifactupload.CreateRequest{
		Kind: artifacts.KindDoc, Title: "Launch notes", Owner: "ceo", OwnerKind: iam.ActorAgent,
		Starter: true, Doc: &artifactupload.DocStarter{Body: "Revenue grew 40% this quarter."},
		Shares: []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: janeSeat},
			Role: artifacts.RoleCommenter}},
	})
	if err != nil {
		t.Fatalf("create the agent's doc: %v", err)
	}
	if err := n.engine.WaitReached(t.Context(), made.Result.Position); err != nil {
		t.Fatal(err)
	}
	id := made.Artifact.String()

	// THE AGENT'S TURN, scripted before the comment, whose wake can reach the
	// executor before the act answers: a new version restoring the first, the
	// thread resolved citing it, and a submission naming the resolve.
	var (
		mu     sync.Mutex
		thread string
		known  = make(chan struct{})
	)
	threadArg := func() string {
		select {
		case <-known:
		case <-t.Context().Done():
		}
		mu.Lock()
		defer mu.Unlock()
		return thread
	}
	n.model.callOnExecute(builtin.PublishArtifactTool, map[string]any{
		"artifact": id, "base_version": 1, "restore_version": 1})
	n.model.callOnExecuteLater(builtin.ResolveArtifactCommentTool, func() map[string]any {
		return map[string]any{"artifact": id, "thread": threadArg(), "version": 2}
	})
	n.model.callOnExecute(runner.SubmitWorkTool, map[string]any{
		"outcome": string(turn.OutcomeDelivered), "summary": "Published version 2 and resolved the thread.",
		"deliveries": []any{builtin.ResolveArtifactCommentTool},
	})

	const remark = "The quarter's total is wrong: say 40%, and cite the source."
	answer := tab.acted(n, builtin.CommentOnArtifactTool, map[string]any{"artifact": id, "body": remark})
	var receipt struct {
		Thread string `json:"thread"`
	}
	if err := json.Unmarshal(answer.Receipt, &receipt); err != nil || receipt.Thread == "" {
		t.Fatalf("the comment answered %s, naming no thread (%v)", answer.Receipt, err)
	}
	mu.Lock()
	thread = receipt.Thread
	mu.Unlock()
	close(known)
	threadID := uuid.MustParse(receipt.Thread)

	arts, _ := engine.ArtifactsEstate(n.engine)
	var got artifactsdomain.CommentThread
	waitFor(t, "the thread resolved by the agent's turn", func() bool {
		th, err := arts.Thread(t.Context(), made.Artifact, threadID)
		got = th
		return err == nil && th.Status == artifactsdomain.ThreadResolved
	}, func() string {
		return "the model saw " + strings.Join(n.model.seen(), ", ") + "; the tools answered " +
			strings.Join(n.model.toolResults(), " | ")
	})
	if got.ResolvedVersion != 2 || got.ResolvedBy.Name != "ceo" || got.ResolvedBy.Kind != iam.ActorAgent {
		t.Errorf("the thread was resolved by %+v citing version %d, want the agent citing version 2",
			got.ResolvedBy, got.ResolvedVersion)
	}
	if head, _, err := arts.Artifact(t.Context(), made.Artifact,
		statelog.Freshness{Level: statelog.ReadLinearizable}); err != nil ||
		head.HeadNumber != 2 {
		t.Errorf("the doc's head is %d (%v), want the version the turn published", head.HeadNumber, err)
	}
	if !slicesHold(n.model.promptsOf("execute"), remark) {
		t.Errorf("the founder's comment did not reach the agent's turn: %q", n.model.promptsOf("execute"))
	}
	deliveredThrough(t, n, builtin.ResolveArtifactCommentTool)
}

// slicesHold reports whether any of texts holds want.
func slicesHold(texts []string, want string) bool {
	for _, text := range texts {
		if strings.Contains(text, want) {
			return true
		}
	}
	return false
}
