package chatlet_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// A CREATE THAT NAMES NO VISIBILITY TAKES THE COMPANY'S DEFAULT.
//
// # Why this is a security case and not a preference
//
// `chat.chatlet.default_channel_private` is how an operator says "rooms here are
// private unless somebody says otherwise". It had NO READER: the create path
// refused an empty kind outright, so there was no way to ask for the default
// and the setting governed nothing. An operator who set it got public rooms and
// no error — the failure mode of a privacy control is that it is silent.
//
// BOTH DIRECTIONS, because a default only means something if it can be either.
// Asserting only the private case would pass against a build that made every
// unstated room private regardless of the setting, which is a different bug
// with the same test.
func TestACreateWithNoVisibilityTakesTheCompanyDefault(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		private bool
		want    chatlet.Kind
	}{
		"a company that said nothing gets public rooms": {private: false, want: chatlet.KindPublic},
		"a company that asked for private gets private": {private: true, want: chatlet.KindPrivate},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newWriteRound(t, agents().withPeople("jane"), func(o *chatlet.Options) {
				o.DefaultPrivate = c.private
			})
			made, err := r.store.CreateChannel(t.Context(), person("jane"),
				chatlet.NewChannel{Name: "unstated"})
			if err != nil {
				t.Fatalf("a create naming no visibility was refused: %v", err)
			}
			r.drain()
			if got := made.Channel.Kind; got != c.want {
				t.Errorf("a room created with no stated kind is %q, want %q — "+
					"default_channel_private=%v governed nothing",
					got, c.want, c.private)
			}
		})
	}
}

// AND A STATED VISIBILITY STILL WINS.
//
// The default answers a question the caller did not ask. A caller who DID ask
// is not overridden by it, in either direction — otherwise the setting would be
// a lock rather than a default, and a company that turned it on could never
// make a public room again.
func TestAStatedVisibilityOutranksTheDefault(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"), func(o *chatlet.Options) {
		o.DefaultPrivate = true
	})
	made, err := r.store.CreateChannel(t.Context(), person("jane"),
		chatlet.NewChannel{Name: "deliberately-open", Kind: chatlet.KindPublic})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()
	if got := made.Channel.Kind; got != chatlet.KindPublic {
		t.Errorf("a room asked for as %q came out %q — the default overrode a "+
			"caller who stated one, which makes it a lock and not a default",
			chatlet.KindPublic, got)
	}
}
