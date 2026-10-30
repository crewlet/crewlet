package mattermost_test

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mattermost"
)

// writes is the calls a stand-in saw that change something, in order.
func writes(s *server) []string {
	var out []string
	for _, call := range s.seen() {
		if !strings.HasPrefix(call, http.MethodGet+" ") &&
			!strings.HasPrefix(call, http.MethodPost+" /api/v4/users/usernames") {
			out = append(out, call)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, body string) bool {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
	return true
}

// A BOT IS OBSERVED WHOLE: its roles, whether THIS engine deactivated it, the
// team's channels it is in (never a private conversation) and its tokens.
func TestObserveBotReadsEveryFactAPassCompares(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/v4/users/u-1":
			return writeJSON(w, `{"id":"u-1","username":"agent-sre",`+
				`"roles":"system_user system_post_all","delete_at":1700000000000}`)
		case "/api/v4/bots/u-1":
			return writeJSON(w, `{"user_id":"u-1","description":"crewlet:disconnected"}`)
		case "/api/v4/users/u-1/teams/t-1/channels":
			return writeJSON(w, `[{"id":"c-2","name":"incidents","type":"O"},`+
				`{"id":"c-1","name":"general","type":"O"},`+
				`{"id":"d-1","name":"u-1__u-9","type":"D"}]`)
		case "/api/v4/users/u-1/tokens":
			return writeJSON(w, `[{"id":"tk-1","description":"crewlet-sre"},`+
				`{"id":"tk-2","description":"by hand"}]`)
		}
		return false
	})
	bot, found, err := client(t, s).ObserveBot(context.Background(), "u-1", "t-1")
	if err != nil || !found {
		t.Fatalf("observe: found=%v err=%v", found, err)
	}
	if !bot.Deactivated || !bot.DeactivatedHere {
		t.Errorf("bot = %+v, want deactivated by this engine", bot)
	}
	if !mattermost.SameRoles(bot.Roles, []string{"system_user", "system_post_all"}) {
		t.Errorf("roles = %q", bot.Roles)
	}
	if !slices.Equal(bot.Channels, []string{"general", "incidents"}) {
		t.Errorf("channels = %q, want the team's channels without the direct message", bot.Channels)
	}
	if minted := bot.MintedTokens("SRE"); len(minted) != 1 || minted[0].ID != "tk-1" {
		t.Errorf("minted tokens = %+v, want this engine's own alone", minted)
	}
}

// A BOT DEACTIVATED BY HAND IS NOT THIS ENGINE'S OFF (D3), and a running bot
// costs no extra read.
func TestObserveBotTellsWhoDeactivatedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		deleteAt           string
		deactivatedHere    bool
		readsTheBotsRecord bool
	}{
		{name: "by hand", deleteAt: "1700000000000", readsTheBotsRecord: true},
		{name: "running", deleteAt: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newServer(t)
			s.responds(func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/api/v4/users/u-1":
					return writeJSON(w, `{"id":"u-1","roles":"system_user","delete_at":`+tc.deleteAt+`}`)
				case "/api/v4/bots/u-1":
					return writeJSON(w, `{"user_id":"u-1","description":""}`)
				}
				return writeJSON(w, `[]`)
			})
			bot, _, err := client(t, s).ObserveBot(context.Background(), "u-1", "t-1")
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			if bot.DeactivatedHere != tc.deactivatedHere {
				t.Errorf("deactivated here = %v", bot.DeactivatedHere)
			}
			read := slices.Contains(s.seen(), "GET /api/v4/bots/u-1")
			if read != tc.readsTheBotsRecord {
				t.Errorf("read the bot's record = %v, want %v", read, tc.readsTheBotsRecord)
			}
		})
	}
}

// SCOPE IS SET WITHIN THE COMPANY'S LIST AND NOWHERE ELSE: a channel the seat
// names is joined, a managed channel it no longer names is left, and a room a
// person invited the bot into is not touched. A channel the team does not
// have is reported, not fatal.
func TestSetChannelsStaysWithinTheCompanysList(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/v4/users/u-1":
			return writeJSON(w, `{"id":"u-1","roles":"system_user"}`)
		case "/api/v4/users/u-1/teams/t-1/channels":
			return writeJSON(w, `[{"id":"c-gen","name":"general","type":"O"},`+
				`{"id":"c-inc","name":"incidents","type":"O"},`+
				`{"id":"c-rnd","name":"random","type":"O"}]`)
		case "/api/v4/teams/t-1/channels/name/town-square":
			return writeJSON(w, `{"id":"c-town","name":"town-square","type":"O"}`)
		case "/api/v4/teams/t-1/channels/name/typo":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return true
		}
		return writeJSON(w, `[]`)
	})
	c := client(t, s)
	bot, _, err := c.ObserveBot(context.Background(), "u-1", "t-1")
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	change, err := c.SetChannels(context.Background(), "t-1", bot,
		[]string{"town-square", "general", "typo"},
		[]string{"general", "incidents", "town-square", "typo"})
	if err != nil {
		t.Fatalf("set channels: %v", err)
	}
	if !slices.Equal(change.Joined, []string{"town-square"}) ||
		!slices.Equal(change.Left, []string{"incidents"}) ||
		!slices.Equal(change.Missing, []string{"typo"}) {
		t.Errorf("change = %+v", change)
	}
	want := []string{
		"POST /api/v4/channels/c-town/members",
		"DELETE /api/v4/channels/c-inc/members/u-1",
	}
	if got := writes(s); !slices.Equal(got, want) {
		t.Errorf("writes = %q, want %q", got, want)
	}
}

// OFF REVOKES THIS ENGINE'S TOKENS, MARKS, THEN DEACTIVATES, in that order,
// and spares a token an administrator minted by hand. A bot already
// deactivated by hand is still marked, and not deactivated twice.
func TestTurnOffRevokesMarksThenDeactivates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		deactivated bool
		want        []string
	}{
		{name: "running", want: []string{
			"POST /api/v4/users/tokens/revoke", "PUT /api/v4/bots/u-1",
			"POST /api/v4/bots/u-1/disable",
		}},
		{name: "deactivated by hand", deactivated: true, want: []string{
			"POST /api/v4/users/tokens/revoke", "PUT /api/v4/bots/u-1",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newServer(t)
			var revoked []string
			s.responds(func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/api/v4/users/u-1/tokens":
					return writeJSON(w, `[{"id":"tk-1","description":"crewlet-sre"},`+
						`{"id":"tk-admin","description":"admin debugging"}]`)
				case "/api/v4/users/tokens/revoke":
					body, _ := io.ReadAll(r.Body)
					revoked = append(revoked, string(body))
				}
				return false
			})
			err := client(t, s).TurnOff(context.Background(),
				mattermost.BotAccount{ID: "u-1", Deactivated: tc.deactivated}, "sre")
			if err != nil {
				t.Fatalf("turn off: %v", err)
			}
			if got := writes(s); !slices.Equal(got, tc.want) {
				t.Errorf("writes = %q, want %q", got, tc.want)
			}
			if len(revoked) != 1 || !strings.Contains(revoked[0], "tk-1") {
				t.Errorf("revoked = %q, want this engine's token alone", revoked)
			}
		})
	}
}

// A TOKEN THAT CANNOT BE REVOKED STOPS THE TURN-OFF BEFORE THE MARKER, so the
// bot stays live and visible rather than deactivated holding a credential.
func TestTurnOffStopsAtATokenItCannotRevoke(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/v4/users/u-1/tokens":
			return writeJSON(w, `[{"id":"tk-1","description":"crewlet-sre"}]`)
		case "/api/v4/users/tokens/revoke":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"no permission"}`))
			return true
		}
		return false
	})
	err := client(t, s).TurnOff(context.Background(), mattermost.BotAccount{ID: "u-1"}, "sre")
	if err == nil {
		t.Fatal("a refused revoke was not reported")
	}
	if got := writes(s); len(got) != 1 {
		t.Errorf("writes = %q, want only the refused revoke", got)
	}
}

// BACK ON REACTIVATES AND CLEARS THE MARKER, or the live bot would read as
// this engine's off on the next pass.
func TestTurnOnReactivatesAndClearsTheMarker(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	var description string
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && r.URL.Path == "/api/v4/bots/u-1" {
			body, _ := io.ReadAll(r.Body)
			description = string(body)
		}
		return false
	})
	if err := client(t, s).TurnOn(context.Background(), mattermost.BotAccount{ID: "u-1"}); err != nil {
		t.Fatalf("turn on: %v", err)
	}
	want := []string{"POST /api/v4/bots/u-1/enable", "PUT /api/v4/bots/u-1"}
	if got := writes(s); !slices.Equal(got, want) {
		t.Errorf("writes = %q, want %q", got, want)
	}
	if !strings.Contains(description, `"description":""`) {
		t.Errorf("the marker was not cleared: %s", description)
	}
}
