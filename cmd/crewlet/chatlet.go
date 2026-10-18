package main

import (
	"context"
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

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet chatlet` — the company's own chat, from a shell.
//
// # Why every verb here talks to a running node
//
// For the reason `crewlet budgets` and `crewlet worklet purge` do, and one more of
// chat's own. The rooms and the messages are the state log's: a write is a
// record the broker arbitrates and every data node applies, and on the default
// topology that broker is inside the engine's own process with no listener at
// all. A command that opened the store instead would either find a database
// locked to a live engine or — worse — write rows the log never carried, which
// is the one direction this engine's estates may not flow in.
//
// # Why no verb here names an author
//
// WHO SPEAKS IS THE CREDENTIAL'S, and there is nowhere in this CLI to write a
// handle. A chatlet verb is made AS A SEAT: the node resolves the credential in
// CREWLET_API_TOKEN to the principal it acts as, and a person — or a
// credential the identity directory binds to a human seat — reads and speaks
// as that seat. A person's own token is `crewlet iam token -login <login>`; a
// pipeline that has to post is a service account bound to a human seat with
// `crewlet iam bind`. A credential acting as itself — a Tier A token, a
// service account bound to no seat — is refused every room, reads included, by
// the authority table's own row: a message's author, a room's membership and
// every recipient rule key on a seat, and a `-as` flag here would be a second
// answer to the only question that decides what a transcript may show.
//
// So `crewlet chatlet post` says something as that seat over the act transport
// the dashboard's composer uses (`post_message`), one gesture with one record
// and one `operator_acted` row, and `channels`, `read` and `search` are the
// chat questions the dashboard asks.
//
// # Except the three said in nobody's voice
//
// `erase`, `prune` and `retention` destroy what people said, now or on a delay,
// and no seat is given them: they are an administrator's, made under the
// administrator's own name — a Tier A token holding admin included — on the
// human write surface's routes beside the tracker's purge. None of them reads
// the room it destroys from, because an administrator is in no private room
// they were not invited to, and what they confirm is what the request itself
// destroys: an erase's count of messages, a prune's cutoff.

const chatletUsage = `crewlet chatlet — the company's own chat, as the seat your credential acts as

Usage:
  crewlet chatlet channels [-archived]              The rooms you are in, and what
                                                    you have not read in each
  crewlet chatlet read ROOM-ID [-limit N] [-before CURSOR]
                                                    One room's transcript, oldest
                                                    first, threads marked
  crewlet chatlet post ROOM -body TEXT [-link URL]...
                                                    Say something; the room by its
                                                    id or its name
  crewlet chatlet search TEXT [-channel ROOM-ID] [-author HANDLE] [-limit N]
                                                    Rank every room you may read
  crewlet chatlet erase ROOM-ID -message ID... -reason TEXT -confirm N
                                                    Destroy the messages named, on
                                                    every data node; no inverse
  crewlet chatlet prune ROOM-ID -cutoff TIME -confirm TIME
                                                    Destroy everything a room said
                                                    before an instant, now; no inverse
  crewlet chatlet retention ROOM-ID -days N | -company
                                                    Set how long one room keeps what
                                                    is said in it, or return it to
                                                    the company's horizon

Every command also takes [<config.yaml>] and -api URL, the running node; every
write takes -op-id ID, the operation an earlier unknown answer named. The
credential is ` + apiTokenEnv + `.

Who you are:
  A chatlet verb is made AS A SEAT: your own token ("crewlet iam token -login
  LOGIN") reads and speaks as your seat, and a service account or a Tier A
  token row bound to a human seat ("crewlet iam bind ID SEAT") as that one. A
  credential bound to no seat is refused every room, reads included. erase,
  prune and retention are the exception: an administrator's, made under their
  own name, and none of them reads the room it acts on.
`

// runChatlet dispatches `crewlet chatlet`.
func runChatlet(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	if sub == "" || sub == "help" {
		fmt.Fprint(stdout, chatletUsage)
		return flag.ErrHelp
	}
	switch sub {
	case "channels":
		return chatletChannels(environ, rest, stdout, stderr)
	case "read":
		return chatletRead(environ, rest, stdout, stderr)
	case "post":
		return chatletPost(environ, rest, stdout, stderr)
	case "search":
		return chatletSearch(environ, rest, stdout, stderr)
	case "erase":
		return chatletErase(environ, rest, stdout, stderr)
	case "prune":
		return chatletPrune(environ, rest, stdout, stderr)
	case "retention":
		return chatletRetention(environ, rest, stdout, stderr)
	default:
		// REFUSED BEFORE ANY FLAG SET IS BUILT, which the switch gives for
		// free: `chatlet reed c-eng -limit 5` parsed against some verb's set
		// would answer "flag provided but not defined: -limit" and send the
		// operator looking at the flag rather than at the word they typed
		// wrong.
		fmt.Fprint(stderr, chatletUsage)
		return fmt.Errorf("unknown chatlet command %q", sub)
	}
}

// ---- a node that serves no chat --------------------------------------- //

// chatletUnserved names the axis when a node answers that it does not serve what a
// chatlet verb asked for, and passes every other failure through unchanged.
//
// THREE REFUSALS, ONE CAUSE. A chat question a node has no rooms for is
// `unknown_query`, a chat tool its catalogue does not carry `unknown_tool`, and
// a destruction whose half is not up `no_route`. Each is right in its own words
// and none says why: the rooms, their questions and their tools exist only
// where `chat.backend` is `chatlet`, the writes only on a node that may publish,
// and every route only on a node with the ingress role. "the node answered
// 404: unknown_tool" sends an operator looking for a typo.
//
// NOT `no_active_revision`, which is a node that has not been handed a company
// yet and says so in its own words: wrapped in this sentence it would send the
// operator to the company's `chat.backend` for a node that has no company at
// all.
func chatletUnserved(err error, what string) error {
	var refusal *nodeRefusal
	if !errors.As(err, &refusal) {
		return err
	}
	switch httpjson.Code(refusal.Code) {
	case httpjson.CodeUnknownQuery, httpjson.CodeUnknownTool, httpjson.CodeNoRoute:
		return fmt.Errorf("%s is not served by this node: the company's own chat "+
			"is served only where its `chat.backend` is `chatlet` — the default for a "+
			"company that declares no vendor chat — its writes only by a node that may "+
			"publish, not one started in a maintenance mode (`crewlet run -mode`), and "+
			"every chat route only by a node with the ingress role. Check the company "+
			"document and the node, or -api if it names another deployment (%w)",
			what, err)
	}
	return err
}

// ---- reading ------------------------------------------------------------ //

// chatletServed is the freshness every chat read carries: the position on the log
// the answering copy had applied, and whether it could present every row.
type chatletServed struct {
	ReadLevel string            `json:"read_level"`
	Complete  bool              `json:"complete"`
	Position  statelog.Position `json:"position"`
	LogLag    *uint64           `json:"log_lag"`
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
func reportServed(stdout io.Writer, s chatletServed) {
	at := ""
	if s.Position.Stream != "" {
		at = " at " + s.Position.String()
	}
	fmt.Fprintf(stdout, "\n%s%s", s.ReadLevel, at)
	if s.LogLag != nil {
		fmt.Fprintf(stdout, ", %d record(s) behind the log", *s.LogLag)
	}
	fmt.Fprintln(stdout)
	if !s.Complete {
		fmt.Fprintln(stdout, "incomplete: the copy that answered could not "+
			"present every row — a record it has deferred, or a document a "+
			"newer build wrote. Run this again once it has caught up.")
	}
}

// oneLine is text as one table cell: every run of whitespace, a line break or
// a tab included, as one space — a cell that broke its line would shift every
// column after it.
func oneLine(text string) string { return strings.Join(strings.Fields(text), " ") }

// chatletRoomRow is one room on the rail.
type chatletRoomRow struct {
	Channel struct {
		ID         string     `json:"id"`
		Kind       string     `json:"kind"`
		Name       string     `json:"name"`
		ArchivedAt *time.Time `json:"archived_at"`
	} `json:"channel"`
	Unread       int  `json:"unread"`
	UnreadCapped bool `json:"unread_capped"`
	Muted        bool `json:"muted"`
	Last         *struct {
		Author  string `json:"author"`
		Excerpt string `json:"excerpt"`
	} `json:"last"`
	Participants []string `json:"participants"`
}

// chatletRail is the rail's answer, as this command prints it.
type chatletRail struct {
	chatletServed
	Channels   []chatletRoomRow `json:"channels"`
	Truncated  bool             `json:"truncated"`
	Unreadable int              `json:"unreadable"`

	// ReadState says the UNREAD column is a measurement. False is a
	// person's read state nobody could read — the node zeroes every count
	// rather than counting each room's whole tail — which is "nobody could
	// look", never "you have read everything".
	ReadState bool `json:"read_state"`
}

// chatletChannels lists the rooms the credential's seat is in.
func chatletChannels(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var archived *bool
	client, err := nodeClientFor(environ, args, "chatlet channels", stderr,
		func(fs *flag.FlagSet) {
			archived = fs.Bool("archived", false,
				"include archived rooms, which take no new messages and stay readable")
		})
	if err != nil {
		return err
	}
	path := "/chatlet/channels"
	if *archived {
		path += "?include_archived=true"
	}
	var answer chatletRail
	if err := client.get(context.Background(), path, &answer); err != nil {
		return chatletUnserved(err, "the room list")
	}
	if len(answer.Channels) == 0 {
		fmt.Fprintln(stdout, "No rooms: this seat is in none, or the company has "+
			"none yet.")
		reportServed(stdout, answer.chatletServed)
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tKIND\tUNREAD\tLAST")
	for _, row := range answer.Channels {
		name := row.Channel.Name
		if name == "" {
			// A DIRECT CONVERSATION HAS NO NAME — its identity is its
			// participants — so the row would otherwise be a blank column
			// beside an id nobody can place.
			name = strings.Join(row.Participants, ", ")
		}
		if row.Channel.ArchivedAt != nil {
			name += " (archived)"
		}
		if row.Muted {
			name += " (muted)"
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
			last = row.Last.Author + ": " + oneLine(row.Last.Excerpt)
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
		fmt.Fprintln(stdout, "unread: unknown — the node could not read where this "+
			"seat has read to, so the counts are unknown rather than zero. The rooms "+
			"are the log's and are still true; run this again once the coordination "+
			"store answers.")
	}
	reportServed(stdout, answer.chatletServed)
	return nil
}

// chatletMessageRow is one message as the transcript carries it.
type chatletMessageRow struct {
	Message struct {
		ID         string     `json:"id"`
		ThreadRoot string     `json:"thread_root"`
		Author     string     `json:"author"`
		Body       string     `json:"body"`
		Links      []string   `json:"links"`
		CreatedAt  time.Time  `json:"created_at"`
		DeletedAt  *time.Time `json:"deleted_at"`
		EditedAt   *time.Time `json:"edited_at"`
	} `json:"message"`
	ChannelSeq int64 `json:"channel_seq"`
}

// chatletTranscript is one page of a room, as this command prints it.
type chatletTranscript struct {
	chatletServed
	Messages   []chatletMessageRow `json:"messages"`
	NextCursor string              `json:"next_cursor"`
	Unreadable int                 `json:"unreadable"`
}

// chatletRead prints one room's transcript.
func chatletRead(environ config.Source, args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var limit *int
	var before *string
	client, err := nodeClientFor(environ, rest, "chatlet read", stderr,
		func(fs *flag.FlagSet) {
			limit = fs.Int("limit", 0,
				"how many messages to print; 0 takes the node's own page size")
			before = fs.String("before", "",
				"the cursor a previous page printed; reads the page before it")
		})
	if err != nil {
		return err
	}
	room := strings.TrimSpace(channel)
	if room == "" {
		fmt.Fprintln(stderr, "usage: crewlet chatlet read ROOM-ID [-limit N] [-before CURSOR]")
		return errors.New("name the room to read: `crewlet chatlet channels` lists the " +
			"ids this seat can use")
	}
	// THE PAGE SIZE IS THE DOMAIN'S unless the operator names one, as
	// `chatlet search` leaves its own to the surface: a number held here would
	// be a second copy of [chatlet.DefaultLimit] that drifts the day it moves.
	query := url.Values{}
	if *limit > 0 {
		query.Set("limit", strconv.Itoa(*limit))
	}
	if cursor := strings.TrimSpace(*before); cursor != "" {
		query.Set("cursor", cursor)
	}
	path := "/chatlet/channels/" + url.PathEscape(room) + "/messages"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var answer chatletTranscript
	if err := client.get(context.Background(), path, &answer); err != nil {
		return chatletUnserved(err, "a room's transcript")
	}
	switch {
	case len(answer.Messages) > 0:
	case query.Has("cursor"):
		fmt.Fprintln(stdout, "No older messages: the page before that cursor is "+
			"the start of the room.")
	default:
		fmt.Fprintln(stdout, "No messages: nothing has been said in this room, or "+
			"everything said in it has been destroyed.")
	}
	// OLDEST FIRST. The route answers newest-first because that is what a
	// screen renders and pages backwards from; a terminal transcript is
	// READ, and a conversation read bottom-up is not one.
	for _, row := range slices.Backward(answer.Messages) {
		fmt.Fprintln(stdout, chatletTranscriptLine(row))
	}
	if answer.NextCursor != "" {
		fmt.Fprintf(stdout, "\nolder: -before %s\n", answer.NextCursor)
	}
	if answer.Unreadable > 0 {
		fmt.Fprintf(stdout, "%d message(s) this build could not present.\n",
			answer.Unreadable)
	}
	reportServed(stdout, answer.chatletServed)
	return nil
}

// chatletTranscriptLine renders one message: its number in the room, when, its
// id, who, the thread it was said in, what it says and the links it carries.
//
// THE ID IS PRINTED because two things need it and nothing else here shows it:
// `chatlet erase` names the messages it destroys by id — the credential pasted
// into one message is found by reading or searching, and the line that shows
// it has to say what to erase — and a reply's `↳` names the message it hangs
// off by id, which read as a reference to nothing while no line carried one.
func chatletTranscriptLine(row chatletMessageRow) string {
	body := row.Message.Body
	switch {
	case row.Message.DeletedAt != nil:
		// A TOMBSTONE KEEPS ITS ROW so a thread hung off it still has a
		// root; printing an empty line for it would read as a message with
		// nothing in it rather than as one taken back.
		body = "(deleted)"
	case row.Message.EditedAt != nil:
		body += " (edited)"
	}
	thread := ""
	if row.Message.ThreadRoot != "" {
		thread = " ↳" + row.Message.ThreadRoot
	}
	head := fmt.Sprintf("%6d  %s  %s  %-16s%s ", row.ChannelSeq,
		row.Message.CreatedAt.UTC().Format(time.RFC3339), row.Message.ID,
		row.Message.Author, thread)
	// A MESSAGE OF SEVERAL LINES stays under its own heading: its later
	// lines are indented past the column a message's number sits in, so
	// none of them reads as a message of its own.
	line := head + strings.ReplaceAll(body, "\n", "\n        ")
	// AND ITS LINKS ARE PART OF WHAT IT SAYS: a message links to a file rather
	// than carrying one, so "here it is" printed without them is a message
	// pointing at nothing. A taken-back one keeps none to show.
	if row.Message.DeletedAt == nil {
		for _, link := range row.Message.Links {
			line += "\n        link: " + link
		}
	}
	return line
}

// chatletSearch ranks every room the credential's seat may read.
func chatletSearch(environ config.Source, args []string, stdout, stderr io.Writer) error {
	text, rest := splitSubject(args)
	var channel, author *string
	var limit *int
	client, err := nodeClientFor(environ, rest, "chatlet search", stderr,
		func(fs *flag.FlagSet) {
			channel = fs.String("channel", "",
				"narrow to one room's id; empty searches every room this seat may read")
			author = fs.String("author", "", "only messages by this handle")
			limit = fs.Int("limit", 0, "how many hits; 0 takes the index's own default")
		})
	if err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		fmt.Fprintln(stderr, "usage: crewlet chatlet search TEXT [-channel ROOM-ID] "+
			"[-author HANDLE] [-limit N]")
		return errors.New("name what to search for")
	}
	query := url.Values{"q": {text}}
	if room := strings.TrimSpace(*channel); room != "" {
		query.Set("channel_id", room)
	}
	if who := strings.TrimPrefix(strings.TrimSpace(*author), "@"); who != "" {
		query.Set("author", who)
	}
	// THE PAGE IS THE INDEX'S unless the operator names one, for the reason
	// `chatlet read` leaves its own to the node.
	if *limit > 0 {
		query.Set("limit", strconv.Itoa(*limit))
	}
	var answer struct {
		Hits []struct {
			MessageID string    `json:"message_id"`
			ChannelID string    `json:"channel_id"`
			Author    string    `json:"author"`
			Snippet   string    `json:"snippet"`
			At        time.Time `json:"at"`
			Score     float64   `json:"score"`
		} `json:"hits"`
		Searched  int    `json:"searched"`
		Building  bool   `json:"building"`
		ReadLevel string `json:"read_level"`
	}
	if err := client.get(context.Background(), "/chatlet/search?"+query.Encode(),
		&answer); err != nil {
		return chatletUnserved(err, "chatlet search")
	}
	// A BUILDING INDEX IS SAID FIRST, because it changes what every line
	// below means: the hits are drawn from part of the transcript, and "nobody
	// said that" is not yet an answer this node can give.
	if answer.Building {
		fmt.Fprintln(stdout, "the index is still building on the node that "+
			"answered: these hits are drawn from part of the transcript, and a "+
			"message it has not reached yet is not among them.")
	}
	if len(answer.Hits) == 0 {
		// THE ROOM COUNT IS THE HALF THAT MATTERS on an empty answer.
		// "Nothing matched" and "this seat may read nothing" look identical
		// without it, and only one of them is about the query.
		fmt.Fprintf(stdout, "No hits across %d room(s), read %s.\n", answer.Searched,
			answer.ReadLevel)
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	// THE MESSAGE'S ID IS A COLUMN, because a search is how the one message
	// somebody pasted a credential into is found, and `chatlet erase` names what
	// it destroys by id.
	fmt.Fprintln(w, "SCORE\tROOM\tMESSAGE\tWHEN\tWHO\tSNIPPET")
	for _, hit := range answer.Hits {
		fmt.Fprintf(w, "%.3f\t%s\t%s\t%s\t%s\t%s\n", hit.Score, hit.ChannelID,
			hit.MessageID, hit.At.UTC().Format(time.RFC3339), hit.Author,
			oneLine(hit.Snippet))
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing the hits: %w", err)
	}
	fmt.Fprintf(stdout, "\n%d hit(s) across %d room(s), read %s.\n",
		len(answer.Hits), answer.Searched, answer.ReadLevel)
	return nil
}

// ---- writing ------------------------------------------------------------ //

// chatletPost says something in a room, as the seat the credential acts as.
//
// THROUGH THE ACT TRANSPORT, as `post_message` — the tool the dashboard's
// composer calls and a seat's turn calls, over the route the dashboard writes
// through (ADR-0024) — so a line said from a terminal is one gesture with one
// record and one `operator_acted` row, attributed to that seat. There is no
// second write path to drift from it.
//
// # A retry is the same message
//
// A post arbitrates nothing at the broker: two people talking in one room are
// not racing for anything, so there is no expectation a resubmission could fail
// against. What makes a retry one message is the OPERATION, which the transport
// takes from the `Idempotency-Key` this command mints before it asks and prints
// whenever the outcome is not known — and `-op-id` sends it again
// ([keyedWrite]).
func chatletPost(environ config.Source, args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var body, opID *string
	var links chatletRepeated
	client, err := nodeClientFor(environ, rest, "chatlet post", stderr,
		func(fs *flag.FlagSet) {
			body = fs.String("body", "", "what to say, in markdown; @handle reaches "+
				"one colleague and @channel the whole room")
			fs.Var(&links, "link",
				"an absolute http, https or mailto address the message points at; "+
					"repeat for more. A message links to a file rather than carrying one")
			opID = opIDFlag(fs,
				"retry a post whose outcome is unknown with the id it printed, so the "+
					"retry cannot say the same thing twice; empty starts a new one")
		})
	if err != nil {
		return err
	}
	room := strings.TrimSpace(channel)
	switch {
	case room == "":
		fmt.Fprintln(stderr, "usage: crewlet chatlet post ROOM -body TEXT [-link URL]... "+
			"[-op-id ID]")
		return errors.New("name the room to post in, by its id or its name: " +
			"`crewlet chatlet channels` lists the rooms this seat is in")
	case strings.TrimSpace(*body) == "":
		return errors.New("say something in -body: a message with nothing in it is " +
			"a notice about nothing")
	}
	operation, err := operationFor("post", *opID)
	if err != nil {
		return err
	}
	write := keyedWrite{gesture: "post", operation: operation, check: "the room"}

	toolArgs := map[string]any{"channel": room, "body": *body}
	if len(links) > 0 {
		toolArgs["links"] = []string(links)
	}
	var answer struct {
		Outcome  string  `json:"outcome"`
		Position *string `json:"position"`
		Receipt  struct {
			MessageID string   `json:"message_id"`
			Mentioned []string `json:"mentioned"`
			WholeRoom bool     `json:"addressed_whole_room"`
		} `json:"receipt"`
	}
	// PATIENTLY, for a purge's reason: a post is one record on the chat log,
	// and what it waits on is the write path's own waits ([logWriteTimeout]).
	if err := write.settle(stderr, client.patiently(logWriteTimeout).postKeyed(
		context.Background(), "/operator/act/"+url.PathEscape(chatlet.PostMessageTool),
		operation, map[string]any{"args": toolArgs}, &answer)); err != nil {
		return chatletUnserved(err, "posting")
	}
	if err := write.confirmed("post in "+room, answer.Outcome); err != nil {
		return err
	}
	at := ""
	if answer.Position != nil && *answer.Position != "" {
		at = " at " + *answer.Position
	}
	fmt.Fprintf(stdout, "post %s in %s: %s%s (operation %s)\n", answer.Receipt.MessageID,
		room, answer.Outcome, at, operation)
	if answer.Receipt.WholeRoom {
		// SAID BACK, because nobody passed a flag: `@channel` was written in
		// the body and the node read it as an address, so this is where the
		// operator learns that the room's agents are about to be woken — the
		// people in it are told, and never woken.
		fmt.Fprintln(stdout, "  @channel: this addresses the whole room, and wakes "+
			"the agents in it.")
	}
	if len(answer.Receipt.Mentioned) > 0 {
		fmt.Fprintf(stdout, "  mentions: @%s\n", strings.Join(answer.Receipt.Mentioned, ", @"))
	}
	if answer.Outcome == string(statelog.OutcomePending) {
		// THE THREE-VALUED OUTCOME, said plainly and NOT as a failure: the
		// record is on the log and every data node applies it as it reaches
		// it, so posting again would say the same thing twice.
		fmt.Fprintln(stdout, "  The record is durable and the node that took it has "+
			"not applied it yet; every data node applies it as it reaches it. Do not "+
			"run this again.")
	}
	return nil
}

// chatletRepeated collects a repeated flag: -link, -message.
type chatletRepeated []string

func (r *chatletRepeated) String() string { return strings.Join(*r, ", ") }

func (r *chatletRepeated) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("an empty value; name one or leave the flag off")
	}
	*r = append(*r, value)
	return nil
}

// chatletErase destroys the messages named in one room, now.
//
// # Why this exists beside the prune
//
// A credential pasted into ONE message is the case it is for. A prune destroys
// everything a room said before an instant — the blunt tool, and every line of
// conversation around the one that leaked goes with it, with no inverse. An
// erase destroys exactly the rows named, on every data node, and leaves the
// conversation around them standing — by the ids `chatlet read` and `chatlet search`
// print beside each message, which is how the one that leaked is found.
//
// # Why the count is typed, and the reason is required
//
// The route's confirmation is the NUMBER of messages: repeating the room id
// would confirm nothing, since it is already in the request, while the count is
// a value the operator had to establish — counted here as the route counts it,
// over [chatlet.EraseSet], so a message named twice is one message on both sides.
// And the reason is the erase record's account of rows that are gone from
// every node, so the route refuses an erase without one — refused here first,
// so a typo costs nothing.
//
// # A destruction in the purge's shape
//
// The operation is minted HERE, before the request, and `-op-id` finishes an
// unknown — or a request the node never answered — rather than appending a
// second erase ([keyedWrite]). The route binds the operation to the set it
// destroys, so the same id sent with other messages is another erase.
func chatletErase(environ config.Source, args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var messages chatletRepeated
	var reason, confirm, opID *string
	client, err := nodeClientFor(environ, rest, "chatlet erase", stderr,
		func(fs *flag.FlagSet) {
			fs.Var(&messages, "message", "a message id to destroy, as `chatlet read` and "+
				"`chatlet search` print it; repeat for each one")
			reason = fs.String("reason", "",
				"why; required, and the erase record's account of the rows it destroys")
			confirm = fs.String("confirm", "",
				"how many messages -message names — this destroys them on every data node")
			opID = opIDFlag(fs,
				"retry an erase whose outcome is unknown with the id it printed, so the "+
					"retry cannot append a second erase; empty starts a new one")
		})
	if err != nil {
		return err
	}
	room := strings.TrimSpace(channel)
	set := chatlet.EraseSet(messages)
	switch {
	case room == "" || len(set) == 0:
		fmt.Fprintln(stderr, "usage: crewlet chatlet erase ROOM-ID -message ID... "+
			"-reason TEXT -confirm N [-op-id ID]")
		return errors.New("name the room and the messages: an erase destroys those " +
			"rows on every data node, and nothing undoes it")
	case strings.TrimSpace(*reason) == "":
		return errors.New("state why in -reason: the rows are destroyed on every node, " +
			"and the reason is the erase record's account of them")
	case strings.TrimSpace(*confirm) != strconv.Itoa(len(set)):
		return fmt.Errorf("repeat the number of messages in -confirm: %d message(s) "+
			"in %s are destroyed on every data node, with no inverse", len(set), room)
	}
	operation, err := operationFor("erase", *opID)
	if err != nil {
		return err
	}
	write := keyedWrite{gesture: "erase", operation: operation,
		check: "the room's transcript"}
	var answer chatletDestroyed
	path := fmt.Sprintf("/chatlet/channels/%s/erase?confirm=%d", url.PathEscape(room),
		len(set))
	body := map[string]any{"message_ids": set, "reason": strings.TrimSpace(*reason)}
	// PATIENTLY, for the purge's reason ([logWriteTimeout]).
	if err := write.settle(stderr, client.patiently(logWriteTimeout).postKeyed(
		context.Background(), path, operation, body, &answer)); err != nil {
		return chatletUnserved(err, "an erase")
	}
	if err := write.confirmed(fmt.Sprintf("erase in %s", room), answer.Outcome); err != nil {
		return err
	}
	reportDestruction(stdout, fmt.Sprintf("erase %d message(s) in %s", len(set), room),
		answer.Outcome, answer.Position, firstNonEmpty(answer.OpID, operation))
	return nil
}

// chatletPrune destroys everything a room said before an instant.
//
// # Why this exists beside the retention duty
//
// The ordinary producer of a prune is the fleet-singleton retention duty, which
// publishes a cutoff derived from the company's horizon and a room's own. This
// is the other caller: an administrator answering an erasure request for ONE
// room, NOW, rather than at the next sweep.
//
// # Why the cutoff is typed twice
//
// The same guard the capacity window's byte ceiling takes. Everything said in
// the room before this instant is destroyed on every data node with no inverse,
// so the instant that decides how much of a conversation disappears is not a
// value to inherit from a shell history — it appears twice in one command, and
// the route refuses the pair if they name different instants. Compared here as
// INSTANTS, as the route compares them, so the same moment written in two zones
// is confirmed rather than refused.
func chatletPrune(environ config.Source, args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var cutoff, confirm, opID *string
	client, err := nodeClientFor(environ, rest, "chatlet prune", stderr,
		func(fs *flag.FlagSet) {
			cutoff = fs.String("cutoff", "",
				"RFC 3339 instant; everything the room said before it is destroyed")
			confirm = fs.String("confirm", "",
				"the cutoff again — this destroys messages on every data node")
			opID = opIDFlag(fs,
				"retry a prune whose outcome is unknown with the id it printed, so the "+
					"retry cannot append a second prune; empty starts a new one")
		})
	if err != nil {
		return err
	}
	room, when := strings.TrimSpace(channel), strings.TrimSpace(*cutoff)
	if room == "" || when == "" {
		fmt.Fprintln(stderr, "usage: crewlet chatlet prune ROOM-ID -cutoff TIME "+
			"-confirm TIME [-op-id ID]")
		return errors.New("name the room and the instant: a prune destroys everything " +
			"said before it, on every data node, and nothing undoes it")
	}
	// PARSED HERE AS WELL AS AT THE ROUTE, so a typo costs nothing — no
	// request is sent — and the message names RFC 3339 rather than repeating
	// the node's own words.
	instant, err := time.Parse(time.RFC3339Nano, when)
	if err != nil {
		return fmt.Errorf("-cutoff %q is not an RFC 3339 instant "+
			"(2031-04-02T03:14:00Z): %w", when, err)
	}
	again, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*confirm))
	if err != nil || !again.Equal(instant) {
		return fmt.Errorf("repeat the cutoff in -confirm: everything %s said before "+
			"%s is destroyed on every data node, with no inverse", room,
			instant.UTC().Format(time.RFC3339Nano))
	}
	operation, err := operationFor("prune", *opID)
	if err != nil {
		return err
	}
	write := keyedWrite{gesture: "prune", operation: operation,
		check: "the room's transcript"}
	var answer chatletDestroyed
	path := fmt.Sprintf("/chatlet/channels/%s/prune?%s", url.PathEscape(room),
		url.Values{"cutoff": {when}, "confirm": {when}}.Encode())
	// PATIENTLY, for the purge's reason ([logWriteTimeout]).
	if err := write.settle(stderr, client.patiently(logWriteTimeout).postKeyed(
		context.Background(), path, operation, nil, &answer)); err != nil {
		return chatletUnserved(err, "a prune")
	}
	if err := write.confirmed("prune "+room, answer.Outcome); err != nil {
		return err
	}
	reportDestruction(stdout, fmt.Sprintf("prune %s before %s", room,
		instant.UTC().Format(time.RFC3339Nano)), answer.Outcome, answer.Position,
		firstNonEmpty(answer.OpID, operation))
	return nil
}

// chatletDestroyed is what a chat destruction answers on a 200 or a 202 — an
// unknown is a 503 the client reads as a refusal ([keyedWrite.settle]), and a
// 2xx naming any outcome but applied or pending fails ([keyedWrite.confirmed]).
type chatletDestroyed struct {
	Outcome  string `json:"outcome"`
	Position string `json:"position"`
	OpID     string `json:"op_id"`
}

// chatletRetention sets how long one room keeps what is said in it.
//
// # Why it is here, and an administrator's
//
// A room's own horizon is the one setting a person cannot change from the
// dashboard or through a tool: a shorter one is a prune on a delay — the
// retention duty takes whatever it no longer covers on its next pass, on every
// data node — so it is decided as a prune is, on the human write surface, and
// without this verb nothing a person runs could reach it.
//
// # Exactly one of -days and -company
//
// THE ZERO IS A SETTING: `-days 0` keeps the room for ever ([chatlet.RetentionForever]),
// so a flag left off cannot also mean zero — the default a plain integer flag
// would give it — and "the company's own horizon" is a choice of its own,
// `-company`, sent as null. A command naming neither is refused rather than
// guessed at, because the guess that reads an absent value as "the company's"
// destroys the past of a room an administrator had set to keep for ever.
func chatletRetention(environ config.Source, args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var days chatletDays
	var company *bool
	var opID *string
	client, err := nodeClientFor(environ, rest, "chatlet retention", stderr,
		func(fs *flag.FlagSet) {
			fs.Var(&days, "days",
				"how many days the room keeps a message; 0 keeps them for ever")
			company = fs.Bool("company", false,
				"return the room to the company's own horizon "+
					"(chat.chatlet.message_retention_days)")
			opID = opIDFlag(fs,
				"retry a change whose outcome is unknown with the id it printed, so the "+
					"retry is this change rather than another; empty starts a new one")
		})
	if err != nil {
		return err
	}
	room := strings.TrimSpace(channel)
	if room == "" || days.set == *company {
		fmt.Fprintln(stderr, "usage: crewlet chatlet retention ROOM-ID -days N | -company "+
			"[-op-id ID]")
		return errors.New("name the room and exactly one horizon: -days N (0 keeps " +
			"it for ever) or -company for the company's own")
	}
	operation, err := operationFor("retention", *opID)
	if err != nil {
		return err
	}
	write := keyedWrite{gesture: "change", operation: operation,
		check: "the room's settings"}
	var horizon any
	said := "the company's horizon"
	if days.set {
		horizon = days.n
		said = fmt.Sprintf("%d day(s)", days.n)
		if days.n == chatlet.RetentionForever {
			said = "kept for ever"
		}
	}
	var answer chatletDestroyed
	path := "/chatlet/channels/" + url.PathEscape(room) + "/retention"
	// PATIENTLY, for the purge's reason ([logWriteTimeout]): a room's horizon
	// is one record on the chat log.
	if err := write.settle(stderr, client.patiently(logWriteTimeout).putKeyed(
		context.Background(), path, operation,
		map[string]any{"retention_days": horizon}, &answer)); err != nil {
		return chatletUnserved(err, "a room's horizon")
	}
	if err := write.confirmed("retention of "+room, answer.Outcome); err != nil {
		return err
	}
	at := ""
	if answer.Position != "" {
		at = " at " + answer.Position
	}
	fmt.Fprintf(stdout, "retention of %s: %s, %s%s (operation %s)\n", room, said,
		answer.Outcome, at, firstNonEmpty(answer.OpID, operation))
	if answer.Outcome == string(statelog.OutcomePending) {
		fmt.Fprintln(stdout, "  The record is durable and this node has not applied "+
			"it yet; every data node applies it as it reaches it. Do not run this again.")
	}
	// WHAT A SHORTER HORIZON DOES, said where it is set: nothing is destroyed
	// by this record, and the next pass of the retention duty destroys what
	// it no longer covers, with no inverse.
	fmt.Fprintln(stdout, "  The retention duty prunes what this horizon no longer "+
		"covers on its next pass, on every data node, with no inverse.")
	return nil
}

// chatletDays is -days: a whole number of days, and whether it was given at all —
// the zero is a horizon ([chatlet.RetentionForever]), so "not given" is a fact of
// its own rather than a value.
type chatletDays struct {
	n   int
	set bool
}

func (d *chatletDays) String() string {
	if d == nil || !d.set {
		return ""
	}
	return strconv.Itoa(d.n)
}

func (d *chatletDays) Set(value string) error {
	// A WHOLE NUMBER, and no more is judged here: which numbers a room may
	// keep is the store's rule, and a second copy of it here is the one that
	// drifts. The node refuses a number the store does not keep `422
	// invalid`, in its code's own sentence: nobody has worded the store's
	// refusal for a person yet (ADR-0060's floor), so it names no field.
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("a whole number of days, 0 to keep the room for ever: %w", err)
	}
	d.n, d.set = n, true
	return nil
}
