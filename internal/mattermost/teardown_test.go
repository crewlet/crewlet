package mattermost_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
)

// tearDown runs a disconnect against the fake with the admin credential.
func tearDown(t *testing.T, srv *chatServer, roles []*org.Role, removeSeats bool) error {
	t.Helper()
	cfg := enabledChat()
	plan, err := mattermost.PlanFor(&org.Organization{Name: "Nimbus", Roles: roles}, cfg)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	return mattermost.Teardown(context.Background(), mattermost.TeardownOptions{
		Client: chatClient(t, srv), Config: cfg, Plan: plan, RemoveSeats: removeSeats,
	})
}

// tokensOn names the descriptions of the tokens an account still holds.
func tokensOn(srv *chatServer, username string) []string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	out := []string{}
	for _, token := range srv.tokens[srv.bots[username]] {
		out = append(out, token.Description)
	}
	return out
}

// A DISCONNECT TAKES THE CREDENTIAL, NOT ONLY THE ACCOUNT.
//
// This is where disabling instead of deleting costs something. Deleting an
// account takes its tokens with it; disabling one leaves them sitting on it.
// Mattermost refuses a deactivated account's token, so the credential looks
// dead, but it is dormant rather than gone: the account keeps its username,
// so anything that re-enables it, this engine's own reconnect included, makes
// every token ever minted on it work again. A disconnect that reported the
// accounts removed while leaving a live credential in the company's secret
// store, at an instance the operator had just disconnected from, is the worst
// thing this path can get wrong, and it is what it did.
func TestADisconnectRevokesTheSeatsTokenAndDisablesItsBot(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	roles := []*org.Role{chatSeat("SRE Lead", "${MM_TOKEN_SRE}", "engineering")}
	if _, err := reconcileChat(t, srv, newChatSink(), roles); err != nil {
		t.Fatalf("provision: %v", err)
	}
	id := srv.bots["agent-sre-lead"]
	if id == "" {
		t.Fatal("the run created no bot")
	}
	// AN ADMINISTRATOR'S OWN TOKEN on the same bot, which this engine did
	// not mint and has no business revoking: a company may have been
	// running this account before the engine adopted it.
	srv.issue(id, "an-operators-own-token")

	if err := tearDown(t, srv, roles, true); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	left := tokensOn(srv, "agent-sre-lead")
	for _, description := range left {
		if description == mattermost.TokenDescription("sre-lead") {
			t.Error("the seat's own token survived the disconnect, so re-enabling " +
				"the bot restores an agent the operator disconnected")
		}
	}
	if len(left) != 1 || left[0] != "seeded" {
		t.Errorf("tokens left = %v, want the administrator's own and nothing else", left)
	}
	srv.mu.Lock()
	off := srv.off[id]
	srv.mu.Unlock()
	if !off {
		t.Error("the bot is still enabled after a disconnect that removes seats")
	}
}

// WITHOUT THE CHECKBOX NOTHING AT THE INSTANCE IS TOUCHED. Disconnecting is
// dropping the block; removing the accounts is a second, destructive thing an
// operator asks for separately.
func TestADisconnectWithoutRemoveSeatsLeavesTheBotAlone(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	roles := []*org.Role{chatSeat("SRE Lead", "${MM_TOKEN_SRE}", "engineering")}
	if _, err := reconcileChat(t, srv, newChatSink(), roles); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := tearDown(t, srv, roles, false); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if got := srv.liveTokens(); got != 1 {
		t.Errorf("live tokens = %d, want the seat's own untouched", got)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.off[srv.bots["agent-sre-lead"]] {
		t.Error("a disconnect that was not asked to remove seats disabled a bot")
	}
}

// A CREDENTIAL THAT WOULD NOT GO LEAVES THE BOT ENABLED, and says so.
//
// The alternative is the state nobody can see: an account disabled in the
// console, reported removed, quietly holding a token that works the moment
// anybody turns it back on. Failing with the bot still listed is the honest
// answer, and repeating the disconnect resumes exactly here.
func TestABotWhoseTokenSurvivesIsLeftEnabled(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	roles := []*org.Role{chatSeat("SRE Lead", "${MM_TOKEN_SRE}", "engineering")}
	if _, err := reconcileChat(t, srv, newChatSink(), roles); err != nil {
		t.Fatalf("provision: %v", err)
	}
	srv.mu.Lock()
	srv.revokeFails = true
	srv.mu.Unlock()

	err := tearDown(t, srv, roles, true)
	if err == nil {
		t.Fatal("a disconnect that could not revoke the seat's token reported success")
	}
	if !strings.Contains(err.Error(), "agent-sre-lead") {
		t.Errorf("error = %v, and it does not name the bot an operator has to go and fix", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.off[srv.bots["agent-sre-lead"]] {
		t.Error("the bot was disabled while still holding a live token, which is " +
			"the one state an operator cannot see")
	}
}

// A DEPARTED SEAT'S CREDENTIAL GOES WITH IT, on the same reasoning: a
// decommission disables the bot, and a token left live on it is a colleague
// who left the company and starts working again the moment the account is
// re-enabled.
func TestDecommissionRevokesTheDepartedSeatsToken(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	both := []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
		chatSeat("CTO", "${MM_TOKEN_CTO}", "eng"),
	}
	if _, err := reconcileChat(t, srv, newChatSink(), both); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := srv.liveTokens(); got != 2 {
		t.Fatalf("live tokens = %d, want one per seat", got)
	}

	res, err := reconcileChatWith(t, srv, newChatSink(), both[:1],
		func(o *mattermost.Options) { o.Decommission = true })
	if err != nil {
		t.Fatalf("decommission: %v", err)
	}
	if len(res.Decommissioned) != 1 {
		t.Fatalf("Decommissioned = %v, want the departed seat", res.Decommissioned)
	}
	if left := tokensOn(srv, "agent-cto"); len(left) != 0 {
		t.Errorf("the departed seat still holds %v", left)
	}
	// AND THE SEAT STILL IN THE COMPANY KEEPS WORKING: a decommission that
	// took a working colleague's credential is far worse than one that
	// left a departed one's.
	if left := tokensOn(srv, "agent-ceo"); len(left) != 1 {
		t.Errorf("the remaining seat holds %v, want its own token untouched", left)
	}
}

// THE ADMINISTRATOR'S OWN TOKEN IS NOT THIS ENGINE'S TO TAKE, whatever a
// walk is asked to remove. The description is the only thing separating the
// two, which is why one function owns the comparison.
func TestRevokeMintedTakesOnlyWhatThisToolMinted(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	roles := []*org.Role{chatSeat("SRE Lead", "${MM_TOKEN_SRE}", "engineering")}
	if _, err := reconcileChat(t, srv, newChatSink(), roles); err != nil {
		t.Fatalf("provision: %v", err)
	}
	id := srv.bots["agent-sre-lead"]
	srv.issue(id, "an-operators-own-token")

	client := chatClient(t, srv)
	// FOLDED, because a decommission recovers the handle from the
	// lowercased username Mattermost stores, and a mixed-case seat's
	// credential must not survive that.
	revoked, err := client.RevokeMinted(context.Background(), id,
		strings.ToUpper(mattermost.TokenDescription("sre-lead")), "")
	if err != nil {
		t.Fatalf("RevokeMinted: %v", err)
	}
	if revoked != 1 {
		t.Errorf("revoked %d tokens, want the one this tool minted", revoked)
	}
	if left := tokensOn(srv, "agent-sre-lead"); len(left) != 1 || left[0] != "seeded" {
		t.Errorf("tokens left = %v, want the administrator's own alone", left)
	}
}
