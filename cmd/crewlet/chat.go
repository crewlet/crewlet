package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
)

// `crewlet chat` — the operator's half of the company's own chat.
//
// # Why every verb here talks to a running node
//
// For the reason `crewlet budgets` and `crewlet work purge` do, and one more
// of chat's own. The rooms and the messages are the state log's: a write is a
// record the broker arbitrates and every node applies, and on the default
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
// So `crewlet chat post` says something as whoever holds the token, and
// `crewlet chat prune` destroys messages as that same person recorded as an
// OPERATOR. Neither can speak as somebody else.

// chatSubcommands is every `crewlet chat` verb. It is what the guard in
// [runChat] checks a name against before any flag is registered, and the
// dispatch switch at the bottom of that function must name exactly these —
// TestEveryChatVerbIsDispatchedAndDocumented asserts both directions, because
// nothing else connects the list, the switch and the usage text.
var chatSubcommands = []string{
	"channels", "read", "post", "search", "prune",
}

const chatUsage = `usage: crewlet chat <command> [<config.yaml>] [-url] [-token]

  crewlet chat channels        The rooms this credential's seat is in
  crewlet chat read <id>       One room's transcript, oldest first
  crewlet chat post <id>       Say something, as the seat this token is bound to
  crewlet chat search <text>   Rank every room this seat may read
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
	case "prune":
		return chatPrune(rest, stdout, stderr)
	}
	// UNREACHABLE while the guard above and this switch name the same set,
	// which is exactly what the test asserts. Returning rather than
	// panicking because a list that grew without a case is a missing
	// feature, not a corrupt process.
	return fmt.Errorf("chat command %q is listed but not dispatched", sub)
}

// ---- the three-valued answer every chat write gives -------------------- //

// chatWriteAnswer is what a chat gesture answers with, on any status.
//
// The outcome is the write authority's own three values and NOT a bool:
// `applied` is on this node's rows, `pending` is durable on the log and not
// yet applied here, and `unknown` is no acknowledgement at all — which is the
// one to retry, and the only one where retrying under a FRESH operation id
// would say the same thing twice.
//
// IT DECODES WHAT THIS CLI PRINTS AND NO MORE. The route's answer also
// carries the room, the revision and each destructive gesture's own echo, and
// a field here for each of them would be a field nothing reads — which the
// next reader cannot tell from one whose reader they failed to find.
type chatWriteAnswer struct {
	Outcome  string `json:"outcome"`
	OpID     string `json:"op_id"`
	Position struct {
		Stream string `json:"stream"`
		Seq    uint64 `json:"seq"`
	} `json:"position"`
	Message struct {
		ID string `json:"id"`
	} `json:"message"`
}

// chatGesture performs one chat write and reads its answer.
//
// # Why this is not [nodeClient.do]
//
// Because a chat write is THREE-VALUED ON THE WIRE and every other operator
// route here is not. `do` accepts 200 and turns everything else into an
// error — which is right for a budget reset or a purge, both of which answer
// 200 or nothing. A chat gesture answers 200 for `applied`, **202 Accepted**
// for `pending` and **504** for `unknown`, and two of those three are
// successful writes: the record is on the log, or may be, and an operator
// told "the node answered 202" has been handed a failure for a message that
// was in fact published.
//
// So the answer is identified by what it CARRIES rather than by its status: a
// body with an `outcome` in it is this surface answering, whatever the code,
// and a body without one is a refusal and goes to [nodeError] — which is what
// keeps a 403 from an unbound token, a 404 from a node running no native chat
// and a proxy's error page reading the way they do everywhere else.
func (c *nodeClient) chatGesture(ctx context.Context, path string, body []byte) (
	chatWriteAnswer, error) {

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, reader)
	if err != nil {
		return chatWriteAnswer{}, fmt.Errorf("build the request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return chatWriteAnswer{}, fmt.Errorf("reach the node at %s: %w\n\n"+
			"Chat lives on the state log, which the RUNNING engine holds. "+
			"Start the node, or pass -url to name another one", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The same cap and the same refusal-at-the-cap as [nodeClient.do]: a
	// clipped body reaches json.Unmarshal as malformed JSON and sends the
	// reader looking for a protocol fault that is not there.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxNodeResponseBytes+1))
	if err != nil {
		return chatWriteAnswer{}, fmt.Errorf("reading the node's answer to %s: %w",
			path, err)
	}
	if len(raw) > maxNodeResponseBytes {
		return chatWriteAnswer{}, fmt.Errorf(
			"the node's answer to %s exceeded %d bytes, so it was not read: "+
				"check that -url names the engine rather than something in "+
				"front of it", path, maxNodeResponseBytes)
	}
	var answer chatWriteAnswer
	if err := json.Unmarshal(raw, &answer); err == nil && answer.Outcome != "" {
		return answer, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		// THE ONE REFUSAL WORTH NAMING HERE, because its body is empty:
		// net/http's mux answers a bare 404 for a route it does not
		// have, and every chat route is mounted only where the node
		// runs NATIVE chat. "the node answered 404: " tells an operator
		// nothing; the axis does.
		return chatWriteAnswer{}, fmt.Errorf(
			"%s is not a route this node serves: chat's gestures are mounted "+
				"only where `chat.backend` is `native`. Check the company "+
				"document, or -url if it names a different deployment", path)
	}
	return chatWriteAnswer{}, nodeError(resp.StatusCode, raw, c.token != "")
}

// reportChatOutcome prints a write's outcome, and says what to do about the
// two that are not `applied`.
//
// SAID EVERY TIME rather than only on a failure. `pending` is a durable write
// this node has not applied yet and looks exactly like nothing having
// happened if the operator then reads the room back; `unknown` is the one
// outcome to retry, and a retry under a fresh operation id is how one gesture
// becomes two records.
func reportChatOutcome(stdout io.Writer, what string, answer chatWriteAnswer) {
	fmt.Fprintf(stdout, "%s: %s at %s %d\n", what, answer.Outcome,
		answer.Position.Stream, answer.Position.Seq)
	switch answer.Outcome {
	case "pending":
		fmt.Fprintln(stdout, "  The record is durable and this node has not "+
			"applied it yet; every node applies it as it reaches it. Do not "+
			"run this again.")
	case "unknown":
		fmt.Fprintf(stdout, "  No acknowledgement — this is the one outcome "+
			"to retry, and retrying under the same operation id is what "+
			"stops a second record: -op-id %s\n", answer.OpID)
	}
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
// A chat read is served from the rows this node has applied, so "there is
// nothing here" and "this node has not caught up" are different facts. The
// domain reports both on every answer; dropping them at the CLI would make
// the quieter one invisible, which is precisely the failure the state log's
// read levels exist to prevent.
func reportServed(stdout io.Writer, s chatServed) {
	fmt.Fprintf(stdout, "\n%s at %s %d", s.ReadLevel, s.Position.Stream,
		s.Position.Seq)
	if s.LogLag != nil {
		fmt.Fprintf(stdout, ", %d record(s) behind the log", *s.LogLag)
	}
	fmt.Fprintln(stdout)
	if !s.Complete {
		fmt.Fprintln(stdout, "incomplete: this node could not present every "+
			"row — a record it has deferred, or a document a newer build "+
			"wrote. Re-run once it has caught up.")
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
		return err
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
		unread := strconv.Itoa(row.Unread)
		if row.UnreadCapped {
			unread += "+"
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

// defaultChatReadLimit is how many messages one `crewlet chat read` prints.
//
// FIFTY, against the domain's own [chat.DefaultLimit] of a page and
// [chat.MaxLimit] of five hundred. This output is a terminal transcript a
// person scrolls, not a screen that pages on scroll: fifty is about two
// screenfuls at a standard window, which is enough to see a conversation's
// shape and few enough that the position footer is still reachable. An
// operator reading further says -limit, or follows -before with the cursor
// the last page printed.
const defaultChatReadLimit = 50

// chatRead prints one room's transcript.
func chatRead(args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var limit *int
	var before *string
	client, err := nodeClientFor(rest, "chat read", stderr, func(fs *flag.FlagSet) {
		limit = fs.Int("limit", defaultChatReadLimit, "how many messages to print")
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
	path := fmt.Sprintf("/chat/channels/%s/messages?limit=%d",
		url.PathEscape(strings.TrimSpace(channel)), *limit)
	if cursor := strings.TrimSpace(*before); cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	var answer chatTranscript
	if err := client.get(context.Background(), path, &answer); err != nil {
		return err
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
	at, marker := row.Message.CreatedAt, ""
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
	return fmt.Sprintf("%6d  %s  %-16s%s %s%s", row.ChannelSeq,
		at.UTC().Format(time.RFC3339), row.Message.Author, thread, body, marker)
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
		return err
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
func chatPost(args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var body, opID *string
	var links chatLinks
	client, err := nodeClientFor(rest, "chat post", stderr, func(fs *flag.FlagSet) {
		body = fs.String("body", "", "what to say")
		fs.Var(&links, "link",
			"a URL the message points at; repeat for more (there are no "+
				"attachments — the engine stores no file bytes)")
		opID = fs.String("op-id", "",
			"retry an `unknown` outcome with the id it printed, so the retry "+
				"cannot say the same thing twice")
	})
	if err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(channel) == "":
		fmt.Fprintln(stderr, "usage: crewlet chat post <channel-id> -body TEXT "+
			"[-link URL] [-op-id ID]")
		return errors.New("name the room to post in: `crewlet chat channels` " +
			"lists the ids this seat can use")
	case strings.TrimSpace(*body) == "" && len(links) == 0:
		return errors.New("say something in -body, or point at something with " +
			"-link: a message carrying neither is a wake for nothing")
	}
	// THE IDEMPOTENCY KEY IS MINTED HERE AND CARRIED, because a post
	// arbitrates nothing at the broker: there is no expectation for a
	// retry to fail against, so the operation id is the only thing that
	// can tell a resubmission from a second remark. An `unknown` outcome
	// is retried with -op-id for exactly that reason.
	operation := strings.TrimSpace(*opID)
	if operation == "" {
		operation = uuid.NewString()
	}
	payload, err := json.Marshal(map[string]any{
		"body": *body, "links": []string(links), "operation_id": operation,
	})
	if err != nil {
		return fmt.Errorf("encoding the message: %w", err)
	}
	answer, err := client.chatGesture(context.Background(),
		"/chat/channels/"+url.PathEscape(strings.TrimSpace(channel))+"/messages",
		payload)
	if err != nil {
		return err
	}
	reportChatOutcome(stdout, "post "+answer.Message.ID, answer)
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
// the room before this instant is destroyed on every node with no inverse, so
// the instant that decides how much of a conversation disappears is not a
// value to inherit from a shell history — it appears twice in one command, and
// the route refuses the pair if they differ.
func chatPrune(args []string, stdout, stderr io.Writer) error {
	channel, rest := splitSubject(args)
	var cutoff, confirm *string
	client, err := nodeClientFor(rest, "chat prune", stderr, func(fs *flag.FlagSet) {
		cutoff = fs.String("cutoff", "",
			"RFC 3339 instant; everything the room said before it is destroyed")
		confirm = fs.String("confirm", "",
			"the cutoff again — this destroys messages on every node")
	})
	if err != nil {
		return err
	}
	when := strings.TrimSpace(*cutoff)
	switch {
	case strings.TrimSpace(channel) == "" || when == "":
		fmt.Fprintln(stderr, "usage: crewlet chat prune <channel-id> "+
			"-cutoff RFC3339 -confirm RFC3339")
		return errors.New("name the room and the instant: a prune destroys " +
			"everything said before it, on every node, and nothing undoes it")
	case strings.TrimSpace(*confirm) != when:
		return fmt.Errorf("repeat the cutoff in -confirm: everything %s said "+
			"before %s is destroyed on every node, with no inverse",
			strings.TrimSpace(channel), when)
	}
	// PARSED HERE AS WELL AS AT THE ROUTE, so a typo costs a round trip
	// rather than a refusal from the far end of the network — and so the
	// message names RFC 3339 rather than repeating the node's own words.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := time.Parse(time.RFC3339, when); err != nil {
		return fmt.Errorf("-cutoff %q is not an RFC 3339 instant "+
			"(2031-04-02T03:14:00Z): %w", when, err)
	}
	path := fmt.Sprintf("/chat/channels/%s/prune?cutoff=%s&confirm=%s",
		url.PathEscape(strings.TrimSpace(channel)),
		url.QueryEscape(when), url.QueryEscape(when))
	answer, err := client.chatGesture(context.Background(), path, nil)
	if err != nil {
		return err
	}
	reportChatOutcome(stdout,
		fmt.Sprintf("prune %s before %s", strings.TrimSpace(channel), when), answer)
	// WHAT A PRUNE DOES NOT REACH, said where the operator is running it
	// rather than in a document they may not have open. The same sentence
	// `crewlet work purge` prints, for the same reason: an offline or
	// evicted node keeps its copy until it rejoins, and an operator acting
	// on an erasure request has to be told so.
	fmt.Fprintln(stdout, "  A node that is offline or evicted keeps its copy "+
		"until it replays, adopts a snapshot, is replaced or is destroyed. "+
		"`crewlet retention status` names which nodes those are.")
	return nil
}
