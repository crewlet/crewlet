package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/engine"
)

// fakeFeedsNode answers the dead-letter routes with the ENGINE'S OWN types —
// an engine.FeedDeadLetterListing and an engine.FeedDeadLetter, encoded as
// the route encodes them — so a fixture spelling this command's idea of the
// shape cannot agree with the command whatever a node sends.
type fakeFeedsNode struct {
	server  *httptest.Server
	listing engine.FeedDeadLetterListing

	mu     sync.Mutex
	posted []string // the path and query of every discard
}

// feedsAt is the instant the fake node lists its dead letters at.
var feedsAt = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func newFakeFeedsNode(t *testing.T) *fakeFeedsNode {
	t.Helper()
	month := 30 * 24 * time.Hour
	n := &fakeFeedsNode{listing: engine.FeedDeadLetterListing{At: feedsAt,
		Feeds: []engine.FeedDeadLetterFeed{
			{Source: "work", Group: "crewlet-tracker-feed", Subject: "dlq.changefeed.work.crewlet-tracker-feed.x",
				Entries: []engine.FeedDeadLetter{
					{ID: 41, Source: "work", Stream: "CREWLET_TRACKER_LOG", Position: 1234,
						Generation: 1, Key: "crewlet.tracker.task.T-1", OpID: "op-41",
						Reason:   "unknown_key",
						Detail:   "the frame is sealed under a key this node does not hold (it holds [k1])",
						StoredAt: feedsAt.Add(-3 * time.Hour), Delivered: 312,
						DeadLetteredAt: feedsAt.Add(-2 * time.Hour), KeptAt: feedsAt.Add(-2 * time.Hour),
						ExpiresAt: feedsAt.Add(month - 2*time.Hour)},
					{ID: 57, Source: "work", KeptAt: feedsAt.Add(-time.Hour),
						ExpiresAt: feedsAt.Add(month - time.Hour),
						DecodeError: "changefeed: the dead letter is version 2, and this build " +
							"reads up to 1: a newer build kept it"},
				}},
			{Source: "page", Group: "crewlet-pages-feed", Subject: "dlq.changefeed.page.crewlet-pages-feed.y",
				ListError: "nats: no responders available for request"},
		}}}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/feeds/dead-letters":
			n.mu.Lock()
			listing := n.listing
			n.mu.Unlock()
			_ = json.NewEncoder(w).Encode(listing)
		case r.Method == http.MethodPost && r.URL.Path == "/feeds/dead-letters/41/discard":
			n.mu.Lock()
			n.posted = append(n.posted, r.URL.RequestURI())
			n.mu.Unlock()
			if r.URL.Query().Get("confirm") != "41" {
				httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeConfirmRequired,
					httpjson.Detail{"detail": "the id was not repeated"})
				return
			}
			_ = json.NewEncoder(w).Encode(n.listing.Feeds[0].Entries[0])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(n.server.Close)
	return n
}

// discards is every discard the node was sent.
func (n *fakeFeedsNode) discards() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.posted...)
}

// THE LISTING PRINTS EVERY ENTRY BY THE ID A DISCARD NAMES, AND AN UNREAD FEED
// IS NEVER AN EMPTY ONE: each row carries the entry's feed, why no node read
// it, where its record was delivered from, and its age and time left by the
// node's clock; an entry this build cannot read is still a row, saying why;
// and a feed the node could not list is named on stderr with its error and
// exits non-zero, in the table's form and the JSON's alike. Feeds the node
// listed whole and found empty say so and exit cleanly.
//
// Mutation: skip the unread feed, or print the JSON without its exit status,
// and a case goes red.
func TestFeedsDeadLettersPrintsEveryEntryAndNamesAnUnreadFeed(t *testing.T) {
	t.Parallel()
	node := newFakeFeedsNode(t)
	config := bootstrapForURL(t, node.server.URL)

	out, stderr, err := cli(t, "feeds", "dead-letters", config)
	if err == nil || !strings.Contains(err.Error(), "page") {
		t.Fatalf("a listing with a feed the node could not read exited %v, want an error naming it", err)
	}
	if !strings.Contains(stderr, "nats: no responders available for request") {
		t.Errorf("stderr does not name why the page feed could not be listed:\n%s", stderr)
	}
	for _, want := range []string{
		"ID", "FEED", "REASON", "RECORD", "KEY", "KEPT", "EXPIRES",
		"41", "work", "unknown_key", "CREWLET_TRACKER_LOG@1:1234", "crewlet.tracker.task.T-1",
		"2h ago", "in 29d 22h",
		"57", "(unreadable)", "1h ago", "in 29d 23h",
		"41: the frame is sealed under a key this node does not hold",
		"57: this build cannot read the entry: changefeed: the dead letter is version 2",
		"crewlet feeds discard <id> -confirm <id>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not say %q:\n%s", want, out)
		}
	}

	out, _, err = cli(t, "feeds", "dead-letters", "-json", config)
	if err == nil {
		t.Error("the JSON form of a listing with an unread feed exited 0")
	}
	var answer map[string]any
	if jerr := json.Unmarshal([]byte(out), &answer); jerr != nil || len(answer["feeds"].([]any)) != 2 {
		t.Errorf("-json printed %q (%v), want the node's answer", out, jerr)
	}

	node.mu.Lock()
	node.listing.Feeds = []engine.FeedDeadLetterFeed{{Source: "work", Group: "crewlet-tracker-feed",
		Subject: "dlq.changefeed.work.crewlet-tracker-feed.x", Entries: []engine.FeedDeadLetter{}}}
	node.mu.Unlock()
	out, stderr, err = cli(t, "feeds", "dead-letters", config)
	if err != nil || !strings.Contains(out, "No wake feed holds a dead letter.") {
		t.Errorf("feeds listed whole and empty printed %q (%v, %s), want that none holds one",
			out, err, stderr)
	}
}

// A DISCARD IS REFUSED WITHOUT A CONFIRMATION REPEATING ITS ID, AND POSTS ONE
// THAT DOES: nothing reaches the node from an invocation that does not repeat
// the id — the wake the entry holds is lost for good — and a confirmed one
// posts the id in the path and the confirmation beside it, and prints what was
// discarded.
//
// Mutation: post without checking -confirm, or send no confirm to the node,
// and a case goes red.
func TestFeedsDiscardIsRefusedWithoutAConfirmationRepeatingItsID(t *testing.T) {
	t.Parallel()
	node := newFakeFeedsNode(t)
	config := bootstrapForURL(t, node.server.URL)

	for _, args := range [][]string{
		{"feeds", "discard", "41", config},
		{"feeds", "discard", "41", "-confirm", "42", config},
		{"feeds", "discard", "-confirm", "41", config},
	} {
		if _, _, err := cli(t, args...); err == nil || !strings.Contains(err.Error(), "-confirm") {
			t.Errorf("%v exited %v, want a refusal naming -confirm", args, err)
		}
	}
	if posted := node.discards(); len(posted) != 0 {
		t.Fatalf("a refused discard reached the node: %v", posted)
	}

	out, stderr, err := cli(t, "feeds", "discard", "41", "-confirm", "41", config)
	if err != nil {
		t.Fatalf("a confirmed discard: %v\n%s", err, stderr)
	}
	if posted := node.discards(); len(posted) != 1 ||
		posted[0] != "/feeds/dead-letters/41/discard?confirm=41" {
		t.Errorf("the node was sent %v, want one discard of 41 confirming it", posted)
	}
	for _, want := range []string{"Discarded dead letter 41 of the work feed",
		"CREWLET_TRACKER_LOG@1:1234", "lost for good"} {
		if !strings.Contains(out, want) {
			t.Errorf("the discard does not say %q:\n%s", want, out)
		}
	}
}
