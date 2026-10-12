package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet chat` — the operator's half of the company's own chat.
//
// # Why every verb here talks to a running node
//
// For the reason `crewlet budgets` and `crewlet work purge` do, and one more
// of chat's own. The rooms and the messages are the state log's: a write is a
// record the broker arbitrates and every data node applies, and on the default
// topology that broker is inside the engine's own process with no listener at
// all. A command that opened the store instead would either find a database
// locked to a live engine or — worse — write rows the log never carried,
// which is the one direction this engine's estates may not flow in.
//
// # Why no verb here names an author
//
// THE SERVER RESOLVES THE CREDENTIAL TO A SEAT, on every call, and there is
// nowhere in this CLI to write a handle. A person in chat IS a `kind: human`
// seat whose `contact.crewlet_operator_id` names one of Tier A's
// `api.auth.tokens`; a token bound to no seat is refused everything, reads
// included. That is the rule the whole surface is built on (see
// internal/api/chat.go), and a `-as` flag here would be a second answer to the
// only question that decides what a transcript may show.
//
// So `crewlet chat post` says something as the person the token is bound to,
// over the act transport the dashboard's composer uses (`post_message`), and
// `crewlet chat erase` and `crewlet chat prune` destroy messages as that same
// person, recorded as an OPERATOR's act on the destructive routes beside the
// tracker's purge. None of them can speak as somebody else.

// chatSubcommands is every `crewlet chat` verb. It is what the guard in
// [runChat] checks a name against before any flag is registered, and the
// dispatch switch at the bottom of that function must name exactly these —
// TestEveryChatVerbIsDispatchedAndDocumented asserts both directions, because
// nothing else connects the list, the switch and the usage text.
var chatSubcommands = []string{
	"channels", "read", "post", "search", "erase", "prune",
}

const chatUsage = `usage: crewlet chat <command> [<config.yaml>] [-url] [-token]

  crewlet chat channels        The rooms this credential's seat is in
  crewlet chat read <id>       One room's transcript, oldest first
  crewlet chat post <room>     Say something, as the person your token is
                               bound to; the room by its id or its name
  crewlet chat search <text>   Rank every room this seat may read
  crewlet chat erase <id>      Destroy the messages named, on every data
                               node — a credential pasted into one line; no
                               inverse
  crewlet chat prune <id>      Destroy everything said in a room before an
                               instant, now, rather than waiting for the
                               retention duty — there is no inverse

The author is never named here: the node resolves this token to the
` + "`kind: human`" + ` seat whose contact.crewlet_operator_id matches it, and a
token bound to no seat is refused every room, reads included.
`

// runChat dispatches `crewlet chat`.
func runChat(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	if sub == "" || sub == "help" {
		fmt.Fprint(stdout, chatUsage)
		return flag.ErrHelp
	}
	// REFUSED BEFORE ANY FLAG SET IS BUILT, so a misspelled verb is
	// reported as one: `chat reed c-eng -limit 5` parsed against a bare set
	// answers "flag provided but not defined: -limit" and sends the
	// operator looking at the flag rather than at the word they typed
	// wrong. It is the guard `crewlet config` already takes.
	if !slices.Contains(chatSubcommands, sub) {
		fmt.Fprint(stderr, chatUsage)
		return fmt.Errorf("unknown chat command %q", sub)
	}
	switch sub {
	case "channels":
		return chatChannels(rest, stdout, stderr)
	case "read":
		return chatRead(rest, stdout, stderr)
	case "post":
		return chatPost(rest, stdout, stderr)
	case "search":
		return chatSearch(rest, stdout, stderr)
	case "erase":
		return chatErase(rest, stdout, stderr)
	case "prune":
		return chatPrune(rest, stdout, stderr)
	}
	// UNREACHABLE while the guard above and this switch name the same set,
	// which is exactly what the test asserts. Returning rather than
	// panicking because a list that grew without a case is a missing
	// feature, not a corrupt process.
	return fmt.Errorf("chat command %q is listed but not dispatched", sub)
}

// ---- a node that serves no chat --------------------------------------- //

// chatUnserved names the axis when a node answers that it does not serve what
// a chat verb asked for, and passes every other failure through unchanged.
//
// FOUR REFUSALS, ONE CAUSE. A chat question a node has no rooms for is
// `unknown_query`; the destructive routes are unmounted (`no_route`, or
// `method_not_allowed` where a read shares the path); and a tool the
// catalogue does not carry is `unknown_tool`. Each is right in its own words
// and none says why: the rooms, their questions and their tools exist only
// where `chat.backend` is `native`, and the writes only on a node that may
// publish — not one started in a maintenance mode. "the node answered 404:
// unknown_tool" sends an operator looking for a typo.
func chatUnserved(err error, what string) error {
	var refusal *nodeRefusal
	if !errors.As(err, &refusal) {
		return err
	}
	switch refusal.Code {
	case "unknown_query", "unknown_tool", "no_route", "method_not_allowed":
		return fmt.Errorf("%s is not served by this node: chat is served only "+
			"where the company's `chat.backend` is `native`, and its writes only "+
			"by a node that may publish — not one started in a maintenance mode "+
			"(`-mode`). Check the company document and the node, or -url if it "+
			"names a different deployment (%w)", what, err)
	}
	return err
}

// ---- reading ------------------------------------------------------------ //

// chatServed is the freshness every chat read carries: which position on the
// log this node had applied, and whether it could present every row.
type chatServed struct {
	ReadLevel string `json:"read_level"`
	Complete  bool   `json:"complete"`
	Position  struct {
		Stream     string `json:"stream"`
		Generation uint32 `json:"generation"`
		Seq        uint64 `json:"seq"`
	} `json:"position"`
	LogLag *uint64 `json:"log_lag"`
}

// reportServed prints the position an answer was true as of, and says so
// loudly when the answer is INCOMPLETE.
//
// A chat read is served from the rows the answering copy has applied — this
// node's while it serves, a data node's otherwise — so "there is nothing here"
// and "that copy has not caught up" are different facts. The domain reports
// both on every answer; dropping them at the CLI would make the quieter one
// invisible, which is precisely the failure the state log's read levels exist
// to prevent.
func reportServed(stdout io.Writer, s chatServed) {
	fmt.Fprintf(stdout, "\n%s at %s %d", s.ReadLevel, s.Position.Stream,
		s.Position.Seq)
	if s.LogLag != nil {
		fmt.Fprintf(stdout, ", %d record(s) behind the log", *s.LogLag)
	}
	fmt.Fprintln(stdout)
	if !s.Complete {
		fmt.Fprintln(stdout, "incomplete: the copy that answered could not "+
			"present every row — a record it has deferred, or a document a "+
			"newer build wrote. Re-run once it has caught up.")
	}
}

// chatChannelRow is one room on the rail.
type chatChannelRow struct {
	Channel struct {
		ID         string     `json:"id"`
		Kind       string     `json:"kind"`
		Name       string     `json:"name"`
		ArchivedAt *time.Time `json:"archived_at"`
	} `json:"channel"`
	Unread       int  `json:"unread"`
	UnreadCapped bool `json:"unread_capped"`
	Last         *struct {
		Author  string `json:"author"`
		Excerpt string `json:"excerpt"`
	} `json:"last"`
	Participants []string `json:"participants"`
}

type chatChannelListing struct {
	chatServed
	Channels   []chatChannelRow `json:"channels"`
	Truncated  bool             `json:"truncated"`
	Unreadable int              `json:"unreadable"`

	// ReadState says the UNREAD column is a measurement. False is a
	// person's read state nobody could read — the node zeroes every count
	// rather than counting each room's whole tail — which is "nobody could
	// look", never "you have read everything".
	ReadState bool `json:"read_state"`
}

// chatChannels lists the rooms this credential's seat is in.
func chatChannels(args []string, stdout, stderr io.Writer) error {
	var archived *bool
	client, err := nodeClientFor(args, "chat channels", stderr, func(fs *flag.FlagSet) {
		archived = fs.Bool("archived", false,
			"include archived rooms, which take no new messages and stay readable")
	})
	if err != nil {
		return err
	}
	path := "/chat/channels"
	if *archived {
		path += "?include_archived=true"
	}
	var answer chatChannelListing
	if err := client.get(context.Background(), path, &answer); err != nil {
		return chatUnserved(err, "the room list")
	}
	if len(answer.Channels) == 0 {
		fmt.Fprintln(stdout, "No rooms: this seat is in none, or the company "+
			"has none yet.")
		reportServed(stdout, answer.chatServed)
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tKIND\tUNREAD\tLAST")
	for _, row := range answer.Channels {
		name := row.Channel.Name
		if name == "" {
			// A DIRECT CONVERSATION HAS NO NAME — its identity is its
			// participants — so the row would otherwise be a blank
			// column beside an id nobody can place.
			name = strings.Join(row.Participants, ", ")
		}
		if row.Channel.ArchivedAt != nil {
			name += " (archived)"
		}
		// AN UNMEASURED BADGE IS NOT A ZERO. Printed as the count the node
		// zeroed, a coordination store that could not be read told an
		// operator they were caught up in every room.
		unread := "?"
		if answer.ReadState {
			unread = strconv.Itoa(row.Unread)
			if row.UnreadCapped {
				unread += "+"
			}
		}
		last := "-"
		if row.Last != nil {
			last = row.Last.Author + ": " + row.Last.Excerpt
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", row.Channel.ID, name,
			row.Channel.Kind, unread, last)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing the room list: %w", err)
	}
	if answer.Truncated {
		fmt.Fprintln(stdout, "\ntruncated: this seat is in more rooms than one "+
			"rail carries.")
	}
	if answer.Unreadable > 0 {
		fmt.Fprintf(stdout, "%d room(s) this build could not present.\n",
			answer.Unreadable)
	}
	if !answer.ReadState {
		fmt.Fprintln(stdout, "unread: unknown — the node could not read where "+
			"this person has read to, so the counts are unknown rather than "+
			"zero. The rooms are the log's and are still true; run this again "+
			"once the coordination store answers.")
	}
	reportServed(stdout, answer.chatServed)
	return nil
}

// chatMessageRow is one message as the transcript carries it.
type chatMessageRow struct {
	Message struct {
		ID         string     `json:"id"`
		ThreadRoot string     `json:"thread_root"`
		Author     string     `json:"author"`
		Body       string     `json:"body"`
		CreatedAt  time.Time  `json:"created_at"`
		DeletedAt  *time.Time `json:"deleted_at"`
		EditedAt   *time.Time `json:"edited_at"`
	} `json:"message"`
	ChannelSeq int64 `json:"channel_seq"`
}

type chatTranscript struct {
	chatServed
	Messages   []chatMessageRow `json:"messages"`
	NextCursor string           `json:"next_cursor"`
	Unreadable int              `json:"unreadable"`
}

// chatRead prints one room's transcript.
func chatRead(args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var limit *int
	var before *string
	client, err := nodeClientFor(rest, "chat read", stderr, func(fs *flag.FlagSet) {
		limit = fs.Int("limit", 0, "how many messages to print; 0 takes the node's own page size")
		before = fs.String("before", "",
			"the cursor a previous page printed; reads the page before it")
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(channel) == "" {
		fmt.Fprintln(stderr, "usage: crewlet chat read <channel-id> "+
			"[-limit N] [-before CURSOR]")
		return errors.New("name the room to read: `crewlet chat channels` " +
			"lists the ids this seat can use")
	}
	// THE PAGE SIZE IS THE DOMAIN'S unless the operator names one, as
	// `chat search` leaves its own to the surface: a number held here would
	// be a second copy of [chat.DefaultLimit] that drifts the day it moves.
	query := url.Values{}
	if *limit > 0 {
		query.Set("limit", strconv.Itoa(*limit))
	}
	if cursor := strings.TrimSpace(*before); cursor != "" {
		query.Set("cursor", cursor)
	}
	path := "/chat/channels/" + url.PathEscape(strings.TrimSpace(channel)) + "/messages"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var answer chatTranscript
	if err := client.get(context.Background(), path, &answer); err != nil {
		return chatUnserved(err, "a room's transcript")
	}
	// OLDEST FIRST. The route answers newest-first because that is what a
	// screen renders and pages backwards from; a terminal transcript is
	// READ, and a conversation read bottom-up is not one.
	for _, row := range slices.Backward(answer.Messages) {
		fmt.Fprintln(stdout, chatTranscriptLine(row))
	}
	if answer.NextCursor != "" {
		fmt.Fprintf(stdout, "\nolder: -before %s\n", answer.NextCursor)
	}
	if answer.Unreadable > 0 {
		fmt.Fprintf(stdout, "%d message(s) this build could not present.\n",
			answer.Unreadable)
	}
	reportServed(stdout, answer.chatServed)
	return nil
}

// chatTranscriptLine renders one message.
func chatTranscriptLine(row chatMessageRow) string {
	body := row.Message.Body
	switch {
	case row.Message.DeletedAt != nil:
		// A TOMBSTONE KEEPS ITS ROW so a thread hung off it still has a
		// root; printing an empty line for it would read as a message
		// with nothing in it rather than as one taken back.
		body = "(deleted)"
	case row.Message.EditedAt != nil:
		body += " (edited)"
	}
	thread := ""
	if row.Message.ThreadRoot != "" {
		thread = " ↳" + row.Message.ThreadRoot
	}
	return fmt.Sprintf("%6d  %s  %-16s%s %s", row.ChannelSeq,
		row.Message.CreatedAt.UTC().Format(time.RFC3339), row.Message.Author,
		thread, body)
}

// chatSearch ranks every room this seat may read.
func chatSearch(args []string, stdout, stderr io.Writer) error {
	query, rest := splitSubject(args)
	var channel, author *string
	var limit *int
	client, err := nodeClientFor(rest, "chat search", stderr, func(fs *flag.FlagSet) {
		channel = fs.String("channel", "",
			"narrow to one room; empty searches every room this seat may read")
		author = fs.String("author", "", "only messages by this handle")
		limit = fs.Int("limit", 0, "how many hits; 0 takes the surface's default")
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(query) == "" {
		fmt.Fprintln(stderr, "usage: crewlet chat search <text> "+
			"[-channel ID] [-author HANDLE] [-limit N]")
		return errors.New("name what to search for")
	}
	path := "/chat/search?q=" + url.QueryEscape(strings.TrimSpace(query))
	if room := strings.TrimSpace(*channel); room != "" {
		path += "&channel_id=" + url.QueryEscape(room)
	}
	if who := strings.TrimSpace(*author); who != "" {
		path += "&author=" + url.QueryEscape(who)
	}
	if *limit > 0 {
		path += "&limit=" + strconv.Itoa(*limit)
	}
	var answer struct {
		Hits []struct {
			MessageID  string    `json:"message_id"`
			ChannelID  string    `json:"channel_id"`
			ThreadRoot string    `json:"thread_root"`
			Author     string    `json:"author"`
			Excerpt    string    `json:"excerpt"`
			At         time.Time `json:"at"`
			Score      float64   `json:"score"`
		} `json:"hits"`
		Searched  int    `json:"searched"`
		ReadLevel string `json:"read_level"`
	}
	if err := client.get(context.Background(), path, &answer); err != nil {
		return chatUnserved(err, "chat search")
	}
	if len(answer.Hits) == 0 {
		// THE ROOM COUNT IS THE HALF THAT MATTERS on an empty answer.
		// "Nothing matched" and "this seat may read nothing" look
		// identical without it, and only one of them is about the query.
		fmt.Fprintf(stdout, "No hits across %d room(s).\n", answer.Searched)
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCORE\tROOM\tWHEN\tWHO\tEXCERPT")
	for _, hit := range answer.Hits {
		fmt.Fprintf(w, "%.3f\t%s\t%s\t%s\t%s\n", hit.Score, hit.ChannelID,
			hit.At.UTC().Format(time.RFC3339), hit.Author, hit.Excerpt)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing the hits: %w", err)
	}
	fmt.Fprintf(stdout, "\n%d hit(s) across %d room(s), read %s.\n",
		len(answer.Hits), answer.Searched, answer.ReadLevel)
	return nil
}

// ---- writing ------------------------------------------------------------ //

// chatPost says something in a room, as the seat this token is bound to.
//
// THROUGH THE ACT TRANSPORT, as `post_message` — the tool the dashboard's
// composer calls and a seat's turn calls, over the route the dashboard writes
// through (ADR-0024) — so a line said from a terminal is one gesture with one
// record and one `operator_acted` row, attributed to the person the token is
// bound to. There is no second write path to drift from it.
//
// # A retry is the same request
//
// A post arbitrates nothing at the broker: two people talking in one room are
// not racing for anything, so there is no expectation a resubmission could
// fail against. What makes a retry one message is the OPERATION, which the
// transport derives from the request id — so the id is minted once per
// invocation, printed whenever the outcome is not known, and `-request-id`
// sends it again (see act.go).
func chatPost(args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var body, requestID *string
	var links chatLinks
	client, err := nodeClientFor(rest, "chat post", stderr, func(fs *flag.FlagSet) {
		body = fs.String("body", "", "what to say, in markdown; @handle reaches "+
			"one colleague and @channel wakes the room")
		fs.Var(&links, "link",
			"a URL the message points at; repeat for more. A message links to "+
				"a file rather than carrying one")
		requestID = fs.String("request-id", "",
			"retry an `unknown` outcome with the id it printed (a UUIDv7), so "+
				"the retry is the same request and cannot say the same thing twice")
	})
	if err != nil {
		return err
	}
	room := strings.TrimSpace(channel)
	switch {
	case room == "":
		fmt.Fprintln(stderr, "usage: crewlet chat post <channel> -body TEXT "+
			"[-link URL] [-request-id ID]")
		return errors.New("name the room to post in, by its id or its name: " +
			"`crewlet chat channels` lists the rooms this seat is in")
	case strings.TrimSpace(*body) == "":
		return errors.New("say something in -body: a message with nothing in " +
			"it is a wake for nothing")
	}
	id := strings.TrimSpace(*requestID)
	if id == "" {
		id = newRequestID()
	}
	toolArgs := map[string]any{"channel": room, "body": *body}
	if len(links) > 0 {
		toolArgs["links"] = []string(links)
	}
	answer, err := client.act(context.Background(), chat.PostMessageTool, id, toolArgs)
	if err != nil {
		actUnanswered(stderr, err, "message", id)
		return chatUnserved(err, "posting")
	}
	if answer.unknown() {
		actUnknown(stdout, "post in "+room, id, answer)
		return nil
	}
	var receipt struct {
		MessageID  string `json:"message_id"`
		ThreadRoot string `json:"thread_root"`
	}
	if err := json.Unmarshal(answer.Receipt, &receipt); err != nil {
		return fmt.Errorf("the node's receipt for %s is not the JSON it writes: %w",
			chat.PostMessageTool, err)
	}
	at := ""
	if answer.Position != nil {
		at = " at " + *answer.Position
	}
	fmt.Fprintf(stdout, "post %s in %s: %s%s\n", receipt.MessageID, room,
		answer.Outcome, at)
	if answer.Outcome == string(statelog.OutcomePending) {
		// THE THREE-VALUED OUTCOME, said plainly and NOT as a failure: the
		// record is on the log and every data node applies it as it reaches it,
		// so posting again would say the same thing twice.
		fmt.Fprintln(stdout, "  The record is durable and the node that took it "+
			"has not applied it yet; every data node applies it as it reaches it. Do "+
			"not run this again.")
	}
	return nil
}

// chatLinks collects a repeated -link flag.
type chatLinks []string

func (l *chatLinks) String() string { return strings.Join(*l, ", ") }

func (l *chatLinks) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("a -link with nothing in it")
	}
	*l = append(*l, value)
	return nil
}

// chatPrune destroys everything a room said before an instant.
//
// # Why this exists beside the retention duty
//
// The ordinary producer of a prune is the fleet-singleton retention duty,
// which publishes a cutoff derived from `chat.native.message_retention_days`
// and a room's own override. This is the other caller: a person answering an
// erasure request for ONE room, NOW, rather than at the next sweep.
//
// # Why the cutoff is typed twice
//
// The same guard the capacity window's byte ceiling takes. Everything said in
// the room before this instant is destroyed on every data node with no
// inverse, so the instant that decides how much of a conversation disappears
// is not a value to inherit from a shell history — it appears twice in one
// command, and the route refuses the pair if they differ.
//
// # A destruction in the purge's shape
//
// The route answers as `POST /work/{id}/purge` does — 200 for every outcome,
// the operation beside it, no position on `unknown` — and this command reads it
// as `crewlet work purge` does, for the same reason: an operation id minted
// HERE, before the request, is the only handle on a prune whose answer was
// lost, and `-op-id` finishes it rather than appending a second one.
func chatPrune(args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var cutoff, confirm, opID *string
	client, err := nodeClientFor(rest, "chat prune", stderr, func(fs *flag.FlagSet) {
		cutoff = fs.String("cutoff", "",
			"RFC 3339 instant; everything the room said before it is destroyed")
		confirm = fs.String("confirm", "",
			"the cutoff again — this destroys messages on every data node")
		opID = fs.String("op-id", "",
			"retry an `unknown` outcome with the id it printed, so the retry "+
				"cannot append a second prune")
	})
	if err != nil {
		return err
	}
	room, when := strings.TrimSpace(channel), strings.TrimSpace(*cutoff)
	switch {
	case room == "" || when == "":
		fmt.Fprintln(stderr, "usage: crewlet chat prune <channel-id> "+
			"-cutoff RFC3339 -confirm RFC3339 [-op-id ID]")
		return errors.New("name the room and the instant: a prune destroys " +
			"everything said before it, on every data node, and nothing undoes it")
	case strings.TrimSpace(*confirm) != when:
		return fmt.Errorf("repeat the cutoff in -confirm: everything %s said "+
			"before %s is destroyed on every data node, with no inverse", room, when)
	}
	// PARSED HERE AS WELL AS AT THE ROUTE, so a typo costs a round trip
	// rather than a refusal from the far end of the network — and so the
	// message names RFC 3339 rather than repeating the node's own words.
	instant, err := time.Parse(time.RFC3339, when)
	if err != nil {
		return fmt.Errorf("-cutoff %q is not an RFC 3339 instant "+
			"(2031-04-02T03:14:00Z): %w", when, err)
	}
	// MINTED AS THE ROUTE WOULD MINT IT ([statelog.NewOpID], named for this
	// prune by [chat.PruneOpName], which is what the route holds a
	// brought-back id to), carrying the instant a node judges the retry by;
	// one the operator brought is sent as given, because the node refuses an
	// id it did not mint or one minted for another gesture, and checking it
	// here would be a second copy of that rule. See [workPurge], whose
	// reasoning this is.
	operation := *opID
	if operation == "" {
		operation = statelog.NewOpID(time.Now(), chat.PruneOpName(room, instant))
	}
	var answer chatDestroyed
	path := fmt.Sprintf("/chat/channels/%s/prune?cutoff=%s&confirm=%s&op_id=%s",
		url.PathEscape(room), url.QueryEscape(when), url.QueryEscape(when),
		url.QueryEscape(operation))
	// PATIENTLY, on the purge's budget and for its reason: a prune is one
	// record, and what it waits on is the write path's own waits rather than
	// the network.
	if err := client.patiently(purgeRequestTimeout).post(context.Background(), path,
		&answer); err != nil {
		sayDestructionRetry(stderr, err, "prune", operation)
		return chatUnserved(err, "a prune")
	}
	answer.report(stdout, fmt.Sprintf("prune %s before %s", room, when), operation)
	return nil
}

// chatErase destroys the messages named in one room, now.
//
// # Why this exists beside the prune
//
// A credential pasted into ONE message is the case it is for. A prune
// destroys everything a room said before an instant — the blunt tool, and
// every line of conversation around the one that leaked goes with it, with no
// inverse. An erase destroys exactly the rows named, on every data node, and
// leaves the conversation around them standing.
//
// # Why the count is typed, and the reason is required
//
// The route's confirmation is the NUMBER of messages: repeating the room id
// would confirm nothing, since it is already in the request, while the count
// is a value the operator had to establish. And the reason is what the
// erase's runtime audit record says about rows that are gone from every node,
// so the route refuses an erase without one — refused here first, so a typo
// costs nothing.
//
// # A destruction in the purge's shape
//
// Exactly as [chatPrune] and `crewlet work purge`: the operation id is minted
// HERE, before the request, and `-op-id` finishes an `unknown` — or a request
// the node never answered — rather than appending a second erase.
func chatErase(args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var messages chatMessageIDs
	var reason, confirm, opID *string
	client, err := nodeClientFor(rest, "chat erase", stderr, func(fs *flag.FlagSet) {
		fs.Var(&messages, "message",
			"a message id to destroy; repeat for each one")
		reason = fs.String("reason", "",
			"why; required, and what the erase's audit record says about the rows it destroys")
		confirm = fs.String("confirm", "",
			"how many messages -message names — this destroys them on every data node")
		opID = fs.String("op-id", "",
			"retry an `unknown` outcome with the id it printed, so the retry "+
				"cannot append a second erase")
	})
	if err != nil {
		return err
	}
	room := strings.TrimSpace(channel)
	switch {
	case room == "" || len(messages) == 0:
		fmt.Fprintln(stderr, "usage: crewlet chat erase <channel-id> "+
			"-message ID... -reason TEXT -confirm N [-op-id ID]")
		return errors.New("name the room and the messages: an erase destroys " +
			"those rows on every data node, and nothing undoes it")
	case strings.TrimSpace(*reason) == "":
		return errors.New("state why in -reason: the rows are destroyed on every " +
			"node, and the reason is the erase's audit record's account of them")
	case strings.TrimSpace(*confirm) != strconv.Itoa(len(messages)):
		return fmt.Errorf("repeat the number of messages in -confirm: %d "+
			"message(s) in %s are destroyed on every data node, with no inverse",
			len(messages), room)
	}
	// MINTED AS THE ROUTE WOULD MINT IT — named for these messages in this
	// room by [chat.EraseOpName] — for [chatPrune]'s reason.
	operation := *opID
	if operation == "" {
		operation = statelog.NewOpID(time.Now(), chat.EraseOpName(room, messages))
	}
	var answer chatDestroyed
	path := fmt.Sprintf("/chat/channels/%s/erase?confirm=%d&op_id=%s",
		url.PathEscape(room), len(messages), url.QueryEscape(operation))
	body := map[string]any{"message_ids": []string(messages), "reason": *reason}
	if err := client.patiently(purgeRequestTimeout).postJSON(context.Background(), path,
		body, &answer); err != nil {
		sayDestructionRetry(stderr, err, "erase", operation)
		return chatUnserved(err, "an erase")
	}
	answer.report(stdout, fmt.Sprintf("erase %d message(s) in %s", len(messages), room),
		operation)
	return nil
}

// chatMessageIDs collects a repeated -message flag.
type chatMessageIDs []string

func (m *chatMessageIDs) String() string { return strings.Join(*m, ", ") }

func (m *chatMessageIDs) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("a -message with no id in it")
	}
	*m = append(*m, value)
	return nil
}

// chatDestroyed is what a destructive chat route answers: the purge's shape.
type chatDestroyed struct {
	Outcome  string `json:"outcome"`
	OpID     string `json:"op_id"`
	Position struct {
		Stream string `json:"stream"`
		Seq    uint64 `json:"seq"`
	} `json:"position"`
}

// report prints a destruction's outcome as `crewlet work purge` prints its own,
// what names the gesture, and operation is the id this command sent.
func (d chatDestroyed) report(stdout io.Writer, what, operation string) {
	switch d.Outcome {
	case string(statelog.OutcomeUnknown):
		// NO POSITION, which is the whole content of unknown: printed as
		// "at 0" it read as a record landed at the log's origin.
		fmt.Fprintf(stdout, "%s: unknown — the record may or may not be on the "+
			"log\n  No acknowledgement — this is the one outcome to retry, and "+
			"retrying with the same operation id is what stops a second "+
			"record: -op-id %s\n", what, firstNonEmpty(d.OpID, operation))
	default:
		fmt.Fprintf(stdout, "%s: %s at %s %d\n", what, d.Outcome,
			d.Position.Stream, d.Position.Seq)
	}
	if d.Outcome == string(statelog.OutcomePending) {
		fmt.Fprintln(stdout, "  The record is durable and this node has not "+
			"applied it yet: the rows go as each node reaches it. Do not run "+
			"this again.")
	}
	// WHAT A DESTRUCTION DOES NOT REACH, said where the operator is running
	// it rather than in a document they may not have open. The same sentence
	// `crewlet work purge` prints, for the same reason: an offline or
	// evicted node keeps its copy until it rejoins, and an operator acting
	// on an erasure request has to be told so.
	fmt.Fprintln(stdout, "  A node that is offline or evicted keeps its copy "+
		"until it replays, adopts a snapshot, is replaced or is destroyed. "+
		"`crewlet retention status` names which nodes those are.")
}
