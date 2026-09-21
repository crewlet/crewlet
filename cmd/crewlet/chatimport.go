package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/integration"
)

// `crewlet chat import` — a Slack or Mattermost export, replayed into rooms
// this company already has.
//
// # The four properties this command exists to hold
//
// THREADS SURVIVE. A reply is filed under the native id of its root, which is
// derived — [chat.ImportedMessageID] over (source, the root's vendor id) — so
// the root never has to be read back and a resumed run computes the same
// answer as the first one. Native threads are one level deep and so are both
// vendors': a Slack `thread_ts` and a Mattermost nested reply both name a
// root rather than a parent.
//
// THE ORIGINAL INSTANT SURVIVES. Every message carries `authored_at`, the one
// instant on this log that is not the broker's, and it is DISPLAY only: the
// order is still the log's, because the per-channel sequence is what every
// node agrees on. A transcript therefore reads in the order it was said while
// the log stays in the order it was published.
//
// IT WAKES NOBODY. An imported message carries provenance and NO ROUTING
// SNAPSHOT, which is the shape the wake filter asks about — and this command
// sends no mentions and no `@channel` flag at all, so there is nothing for a
// routing snapshot to be built from even if one were asked for. A year of
// mentions arriving as live wakes would be tens of thousands of two-phase
// turns against a node's concurrency budget, at once, on the first day the
// company is on native chat. It is also why an imported message contributes
// nothing to anybody's mention feed: a feed ranked by recency would bury a
// year of history on top of what somebody was actually asked this morning.
//
// IT IS IDEMPOTENT, AND THAT IS WHAT MAKES IT RESUMABLE. A message's id is
// derived from (source, vendor id), so a second pass over one archive derives
// the same ids and the applier's own insert declines every one of them. There
// is no cursor to write down and nothing to remember: a run interrupted
// halfway — a laptop closing, a node restarting, a `ctrl-C` — is re-run from
// the top, and what already landed lands nowhere a second time. Each message
// is ONE record and therefore ONE apply transaction on every node, so an
// archive of any size is never a single transaction and never a single
// rollback.
//
// # Why it never creates a room
//
// Because a room's KIND is a disclosure decision and its MEMBERSHIP is what a
// private room's readability IS. An import that created what it did not find
// would have to guess a membership — and a private Slack channel arriving as
// a native room nobody is in is a room the importer cannot even post into,
// while one arriving public is the whole conversation published to the
// company. So the archive's channels are resolved against the rooms this
// credential's seat is already in, and an archive channel with no room is
// REFUSED by name, before a single message is written.
//
// # Why an author is never guessed
//
// A vendor user id resolves to a seat handle through a map the operator
// writes, and an author with no mapping stops the run. See [importPlan] for
// what the three rejected alternatives cost.
//
// # The route this drives
//
// One message is one `POST /chat/channels/{channel_id}/import`, whose body is
// [importSubmission]: the provenance, the body and the native id of the
// thread root. The server builds a [chat.Imported] from the first four fields
// and calls [chat.Store.Post] or [chat.Store.Reply] with it — which is the
// only write path in this engine that produces a record with no routing
// snapshot. It is deliberately NOT the ordinary message route: that one
// attributes the message to the credential presenting it and routes what it
// says, which for an import would make the transcript say the person running
// the migration said everything in it.

// importSubmission is one imported message on the wire.
//
// THE AUTHOR IS RESOLVED HERE AND CARRIED, which is the one place in this
// whole surface where a caller names one — and it is not an exception to the
// rule that a caller may never name a seat, it is what the rule is FOR. The
// operator's own credential is still what authorises the write and still what
// the record's envelope records as the actor; `author` says who said the
// words a year ago, which is a fact about the archive rather than a claim
// about who is calling. A record carrying the vendor's user id instead would
// need the operator's map again on every node, for ever, including after the
// person has left.
//
// THERE IS NO `mentions` AND NO `collective` FIELD, deliberately. See the
// package comment above: an import wakes nobody, and a field for the two
// things wakes are derived from is a field somebody eventually fills.
type importSubmission struct {
	Source     string    `json:"source"`
	VendorID   string    `json:"vendor_id"`
	Author     string    `json:"author"`
	AuthorKind string    `json:"author_kind"`
	AuthoredAt time.Time `json:"authored_at"`
	Body       string    `json:"body"`

	// ReplyTo is the NATIVE id of the thread's root, empty for a room
	// post. Derived rather than read back, so a resumed run files a reply
	// under the same root the first pass did without having to find it.
	ReplyTo string `json:"reply_to,omitempty"`
}

// importWriter is where one message goes, declared by the caller and kept to
// the one thing this command asks of a node.
type importWriter interface {
	Import(ctx context.Context, channelID string, in importSubmission) (
		chatWriteAnswer, error)
}

// nodeImporter writes through a running node.
type nodeImporter struct{ client *nodeClient }

func (n nodeImporter) Import(ctx context.Context, channelID string,
	in importSubmission) (chatWriteAnswer, error) {

	body, err := json.Marshal(in)
	if err != nil {
		return chatWriteAnswer{}, fmt.Errorf(
			"encoding %s message %s: %w", in.Source, in.VendorID, err)
	}
	return n.client.chatGesture(ctx,
		"/chat/channels/"+url.PathEscape(channelID)+"/import", body)
}

// importProgressEvery is how often a run says where it has got to.
//
// TWO HUNDRED AND FIFTY messages. Each one is a round trip to a node and an
// arbitration at the broker, so a quarter of a thousand is a few seconds of
// silence at worst — short enough that an operator watching a large archive
// does not reach for ctrl-C, and long enough that the output of a twenty
// thousand message import is eighty lines rather than twenty thousand.
const importProgressEvery = 250

// chatImport is `crewlet chat import`.
func chatImport(args []string, stdout, stderr io.Writer) error {
	dir, rest := splitSubject(args)
	var mapPath *string
	var limit *int
	var check *bool
	client, err := nodeClientFor(rest, "chat import", stderr, func(fs *flag.FlagSet) {
		mapPath = fs.String("map", "",
			"a YAML document mapping the archive's channels to this company's "+
				"rooms and its users to seat handles; see the refusals, which "+
				"print the lines to add")
		limit = fs.Int("limit", 0,
			"stop after this many messages; 0 imports the whole archive. A "+
				"slice is re-runnable like any other — nothing already "+
				"written is written again")
		check = fs.Bool("check", false,
			"read the archive, resolve everything and report; write nothing")
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(dir) == "" {
		fmt.Fprintln(stderr, "usage: crewlet chat import <export-dir> "+
			"[-map map.yaml] [-limit N] [-check] [<config.yaml>] [-url] [-token]")
		return errors.New("name the export directory to read")
	}
	archive, err := readChatArchive(strings.TrimSpace(dir))
	if err != nil {
		return err
	}
	mapping, err := loadImportMap(strings.TrimSpace(*mapPath))
	if err != nil {
		return err
	}
	ctx := context.Background()
	rooms, err := importRooms(ctx, client)
	if err != nil {
		return err
	}
	agents, err := importAgentHandles(ctx, client)
	if err != nil {
		return err
	}
	plan, err := buildImportPlan(archive, mapping, rooms, agents)
	if err != nil {
		return err
	}
	plan.report(stdout, archive)
	if *check {
		fmt.Fprintln(stdout, "\n-check: nothing was written.")
		return nil
	}
	return runImport(ctx, stdout, nodeImporter{client: client}, plan, *limit)
}

// ---- what an archive is, once this build has read it -------------------- //

// importMessage is one message, in the only vocabulary both vendors share.
type importMessage struct {
	// VendorID is what makes this message THIS message across two passes.
	// It is the archive's own id where the archive has one and a derived
	// tuple where it does not — see [mattermostVendorID].
	VendorID string

	// RootVendorID is the thread root's vendor id, empty on a room post.
	RootVendorID string

	// Author is the VENDOR's user id, unresolved. Resolution happens once,
	// in [buildImportPlan], where the operator's map is in hand.
	Author string

	AuthoredAt time.Time
	Body       string

	// Where is the file and line this message was read from, so a refusal
	// names something the operator can open.
	Where string
}

// importChannel is one room's worth of an archive.
type importChannel struct {
	Name string

	// Kind is what the ARCHIVE said, never a default. A room whose kind
	// this build cannot read from the archive is refused rather than
	// assumed, because the assumption that costs is the safe-looking one:
	// a private conversation arriving as a public room is a disclosure
	// nothing undoes.
	Kind chat.Kind

	Messages []importMessage
}

// importArchive is an export directory this build has understood.
type importArchive struct {
	// Source is `slack` or `mattermost`, in [integration.Kinds]'
	// vocabulary, because it is what the provenance on every row carries
	// and an operator auditing a room tells one migration from another by
	// it.
	Source string

	// Root is the path the operator named, for the lines a refusal prints.
	Root string

	Channels []importChannel

	// Users maps a vendor user id to the name that vendor displayed for
	// it. Nothing is imported from it: it exists so the refusal for an
	// unmapped author can say WHO the operator is being asked about,
	// rather than handing them a list of opaque ids.
	Users map[string]string

	// Skipped is what this build read and did not import, by reason, for
	// the summary. Counted rather than dropped: a message that is not in
	// the company's rooms afterwards has to be accounted for somewhere.
	Skipped map[string]int
}

func (a *importArchive) skip(reason string) {
	if a.Skipped == nil {
		a.Skipped = map[string]int{}
	}
	a.Skipped[reason]++
}

func (a *importArchive) messageCount() int {
	var n int
	for _, room := range a.Channels {
		n += len(room.Messages)
	}
	return n
}

// readChatArchive reads an export directory, whichever vendor wrote it.
//
// THE VENDOR IS DETECTED RATHER THAN DECLARED, and the refusal names what was
// looked for. A `-source` flag would be a second opinion about a directory
// that already says what it is, and the failure of getting it wrong is silent
// in the worst way: the vendor id it derives is half of every message's
// identity, so an archive imported under the wrong source would import
// cleanly a second time as a second copy of the conversation.
func readChatArchive(dir string) (*importArchive, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the export at %s: %w\n\n"+
			"Name the directory a Slack export was unzipped into, or the "+
			"directory holding a Mattermost bulk export's .jsonl file", dir, err)
	}
	if !info.IsDir() {
		// A BULK EXPORT NAMED DIRECTLY is the one file form, and it is
		// accepted because `mmctl export` produces exactly one file and
		// an operator will point at it.
		if strings.EqualFold(filepath.Ext(dir), ".jsonl") {
			return readMattermostArchive(dir, dir)
		}
		return nil, fmt.Errorf("%s is a file rather than an export directory, "+
			"and it is not a .jsonl bulk export either", dir)
	}
	if exists(filepath.Join(dir, "users.json")) {
		return readSlackArchive(dir)
	}
	jsonl, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("looking for a bulk export in %s: %w", dir, err)
	}
	switch len(jsonl) {
	case 1:
		return readMattermostArchive(dir, jsonl[0])
	case 0:
	default:
		slices.Sort(jsonl)
		return nil, fmt.Errorf("%s holds %d .jsonl files (%s), so this build "+
			"cannot tell which is the bulk export: name the one to import",
			dir, len(jsonl), strings.Join(jsonl, ", "))
	}
	return nil, fmt.Errorf("%s is not an export this build reads: a Slack "+
		"export has users.json and channels.json at its root, and a Mattermost "+
		"bulk export is a single .jsonl file", dir)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---- Slack --------------------------------------------------------------//

// slackChannelLists is where Slack puts each kind of room, and the kind each
// one IS.
//
// PUBLIC AND PRIVATE COME FROM DIFFERENT FILES, which is the whole reason
// this is a table rather than one filename: a corporate export writes public
// channels to channels.json and private ones to groups.json, and a build that
// read only the first would import a workspace's private conversations
// nowhere — while one that read both into a single kind would publish them.
//
// mpims.json and dms.json are deliberately absent. A direct conversation has
// no name to resolve against a native room, and its identity is its
// participant set — so importing one means opening the conversation first,
// which is a gesture with its own membership consequences and not something
// to do on an operator's behalf while replaying a year of history.
var slackChannelLists = []struct {
	file string
	kind chat.Kind
}{
	{"channels.json", chat.KindPublic},
	{"groups.json", chat.KindPrivate},
}

// slackNarrationSubtypes are the messages that are SLACK narrating a room
// rather than anybody speaking in it.
//
// Not imported, and counted rather than dropped. Native chat narrates its own
// rooms — a join, a topic change and an archive are each a record the applier
// writes as [chat.AuthorSystem] from the room's own history — so importing
// Slack's copies would give one room two contradictory accounts of what
// happened to it, in which only one set of events ever actually occurred here.
var slackNarrationSubtypes = []string{
	"channel_join", "channel_leave", "channel_topic", "channel_purpose",
	"channel_name", "channel_archive", "channel_unarchive", "channel_convert_to_private",
	"channel_convert_to_public", "group_join", "group_leave", "group_topic",
	"group_purpose", "group_name", "group_archive", "group_unarchive",
	"pinned_item", "unpinned_item", "bot_add", "bot_remove", "reminder_add",
	"tombstone",
}

type slackUser struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
	Profile struct {
		RealName    string `json:"real_name"`
		DisplayName string `json:"display_name"`
	} `json:"profile"`
}

// slackWho is how an unmapped author is described to the operator who has to
// map it.
//
// BOTH NAMES WHERE THERE ARE TWO. The display name is what they saw in the
// workspace and the real name is what identifies the person a year later, and
// which of the two a reader recognises depends on the workspace. Offering
// only one is how a refusal that exists to be acted on stops being actionable.
func slackWho(u slackUser) string {
	shown := firstNonEmpty(u.Profile.DisplayName, u.Name, u.ID)
	if real := u.Profile.RealName; real != "" && real != shown {
		return shown + " (" + real + ")"
	}
	return shown
}

type slackChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type slackMessage struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Username string `json:"username"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
}

// readSlackArchive reads an unzipped Slack export.
func readSlackArchive(dir string) (*importArchive, error) {
	out := &importArchive{Source: string(integration.KindSlack), Root: dir,
		Users: map[string]string{}}

	var users []slackUser
	if err := readJSONArray(filepath.Join(dir, "users.json"),
		func(raw json.RawMessage, where string) error {
			var u slackUser
			if err := json.Unmarshal(raw, &u); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			if u.ID == "" {
				return fmt.Errorf("%s: a user with no id, so nothing in this "+
					"export can be attributed to it", where)
			}
			users = append(users, u)
			return nil
		}); err != nil {
		return nil, err
	}
	for _, u := range users {
		out.Users[u.ID] = slackWho(u)
	}

	var named int
	for _, list := range slackChannelLists {
		path := filepath.Join(dir, list.file)
		if !exists(path) {
			continue
		}
		var rooms []slackChannel
		if err := readJSONArray(path, func(raw json.RawMessage, where string) error {
			var c slackChannel
			if err := json.Unmarshal(raw, &c); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			if c.Name == "" {
				return fmt.Errorf("%s: a channel with no name, which is what "+
					"this import resolves a native room by", where)
			}
			rooms = append(rooms, c)
			return nil
		}); err != nil {
			return nil, err
		}
		named += len(rooms)
		for _, room := range rooms {
			read, err := readSlackChannel(dir, room, list.kind, out)
			if err != nil {
				return nil, err
			}
			if read != nil {
				out.Channels = append(out.Channels, *read)
			}
		}
	}
	if named == 0 {
		return nil, fmt.Errorf("%s holds no channels.json and no groups.json, "+
			"so nothing in it says which rooms these messages were said in", dir)
	}
	// A DIRECTORY NO LIST NAMES IS REFUSED rather than imported with a
	// guessed kind. Slack writes one directory per room and states the
	// room's kind only in the list files, so a directory that appears in
	// neither is a room whose privacy nothing in this archive declares.
	if err := refuseUnlistedSlackDirs(dir, out.Channels); err != nil {
		return nil, err
	}
	return out, nil
}

func readSlackChannel(dir string, room slackChannel, kind chat.Kind,
	out *importArchive) (*importChannel, error) {

	days, err := filepath.Glob(filepath.Join(dir, room.Name, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("looking for %s's messages: %w", room.Name, err)
	}
	if len(days) == 0 {
		// A LISTED ROOM WITH NO DIRECTORY is an ordinary export of a
		// room nobody ever said anything in, and refusing it would
		// refuse the archive over an empty room.
		out.skip("a listed room with no messages")
		return nil, nil
	}
	slices.Sort(days)
	read := &importChannel{Name: room.Name, Kind: kind}
	for _, day := range days {
		if err := readJSONArray(day, func(raw json.RawMessage, where string) error {
			var m slackMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			msg, keep, err := slackMessageOf(m, room, where, out)
			if err != nil {
				return err
			}
			if keep {
				read.Messages = append(read.Messages, msg)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return read, nil
}

// slackMessageOf turns one exported message into this build's vocabulary,
// reporting whether it is one to import at all.
func slackMessageOf(m slackMessage, room slackChannel, where string,
	out *importArchive) (importMessage, bool, error) {

	if m.Type != "" && m.Type != "message" {
		out.skip("an event that is not a message")
		return importMessage{}, false, nil
	}
	if slices.Contains(slackNarrationSubtypes, m.Subtype) {
		out.skip("Slack narrating a room (" + m.Subtype + ")")
		return importMessage{}, false, nil
	}
	if m.TS == "" {
		// THE ONE FIELD WITH NO SUBSTITUTE. A Slack message's `ts` is
		// both its id within the channel and when it was said, so a
		// message without one can neither be identified across two
		// passes nor rendered at the instant it happened.
		return importMessage{}, false, fmt.Errorf("%s: a message with no `ts`, "+
			"which is both its identity across two passes and when it was said",
			where)
	}
	at, err := slackInstant(m.TS)
	if err != nil {
		return importMessage{}, false, fmt.Errorf("%s: %w", where, err)
	}
	author := firstNonEmpty(m.User, m.BotID)
	if author == "" {
		return importMessage{}, false, fmt.Errorf("%s: a message at %s names "+
			"neither a `user` nor a `bot_id`, so nothing in this archive says "+
			"who said it — and this import never guesses an author", where, m.TS)
	}
	if m.Username != "" && out.Users[author] == "" {
		// A BOT'S DISPLAY NAME lives on the message rather than in
		// users.json, and it is the only thing that tells an operator
		// what they are being asked to map.
		out.Users[author] = m.Username
	}
	body := slackText(m.Text, out.Users)
	if body == "" {
		// THE ENGINE STORES NO FILE BYTES, so a message whose whole
		// content was an upload has nothing to import — and the write
		// path refuses an empty body by name. Counted, so the summary
		// accounts for it.
		out.skip("a message carrying no text (an upload, which this engine " +
			"stores no bytes for)")
		return importMessage{}, false, nil
	}
	// THE CHANNEL IS HALF THE VENDOR ID. A Slack `ts` is unique within a
	// channel and not across a workspace, so two rooms can carry one `ts`
	// — and an id that collided would make one room's message decline as
	// a duplicate of another's, losing it with no error anywhere.
	roomID := firstNonEmpty(room.ID, room.Name)
	msg := importMessage{
		VendorID: roomID + "/" + m.TS, Author: author,
		AuthoredAt: at, Body: body, Where: where,
	}
	if m.ThreadTS != "" && m.ThreadTS != m.TS {
		msg.RootVendorID = roomID + "/" + m.ThreadTS
	}
	return msg, true, nil
}

// slackInstant reads Slack's `ts`: whole seconds, a dot, and a per-channel
// counter.
func slackInstant(ts string) (time.Time, error) {
	// THE FRACTION AFTER THE DOT IS A COUNTER, NOT A SUB-SECOND TIME.
	// Slack's six digits disambiguate messages within one second; reading
	// them as microseconds would put a message said at 12:00:00 up to a
	// second late for no reason anybody could see. Only the seconds reach
	// the instant — the whole `ts`, counter included, is what identifies
	// the message.
	seconds, _, _ := strings.Cut(ts, ".")
	whole, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("`ts` %q is not a Slack timestamp "+
			"(seconds, a dot, then a counter): %w", ts, err)
	}
	if whole <= 0 {
		return time.Time{}, fmt.Errorf("`ts` %q is at or before the epoch, so "+
			"the message states no instant it was said at", ts)
	}
	return time.Unix(whole, 0).UTC(), nil
}

// slackText turns Slack's markup into what somebody reads.
//
// # Why this rewrites at all
//
// Slack stores `<@U0FOUNDER>` where a person typed `@ali`, `<#C0ENG|eng>`
// where they typed `#eng`, and `<https://example.com|the doc>` for a link with
// a label — and it escapes `&`, `<` and `>`. A transcript imported verbatim is
// a year of opaque ids, and the keyword index would rank on the vendor's ids
// rather than on the people named.
//
// # Why the resolved names are NOT sent as mentions
//
// Because a mention is what a wake is derived from and what a mention feed is
// filled from. The prose says who was named; the record says nobody was
// routed. See the package comment.
//
// # Why a link is rendered into the body rather than carried as a link
//
// A message carries at most [chat.MaxLinks] links, and a message with nine
// URLs in it would be REFUSED — losing what somebody said over a field that
// is an affordance rather than the content. The URL is part of the prose it
// was said in, so that is where it goes.
func slackText(text string, users map[string]string) string {
	var out strings.Builder
	for len(text) > 0 {
		open := strings.IndexByte(text, '<')
		if open < 0 {
			out.WriteString(slackUnescape(text))
			break
		}
		shut := strings.IndexByte(text[open:], '>')
		if shut < 0 {
			out.WriteString(slackUnescape(text))
			break
		}
		out.WriteString(slackUnescape(text[:open]))
		out.WriteString(slackEntity(text[open+1:open+shut], users))
		text = text[open+shut+1:]
	}
	return strings.TrimSpace(out.String())
}

// slackEntity renders one `<...>` from a Slack body.
func slackEntity(body string, users map[string]string) string {
	target, label, labelled := strings.Cut(body, "|")
	switch {
	case strings.HasPrefix(target, "@"):
		id := strings.TrimPrefix(target, "@")
		if name := users[id]; name != "" {
			return "@" + name
		}
		if labelled && label != "" {
			return "@" + label
		}
		// THE ID SURVIVES rather than becoming an empty space. A user
		// this export did not list is somebody the operator still has
		// to recognise, and a mention that vanished is a sentence that
		// no longer says who it was about.
		return "@" + id
	case strings.HasPrefix(target, "#"):
		if labelled && label != "" {
			return "#" + label
		}
		return "#" + strings.TrimPrefix(target, "#")
	case strings.HasPrefix(target, "!"):
		// `<!here>`, `<!channel>`, `<!everyone>` — a broadcast, rendered
		// as the word somebody typed and carried as PROSE. It is
		// emphatically not this import's `collective` flag, which this
		// build does not send at all: a year of `@channel` replayed as
		// collective wakes is the failure this whole command is shaped
		// around avoiding.
		return "@" + strings.TrimPrefix(target, "!")
	case labelled && label != "":
		return label + " (" + target + ")"
	}
	return target
}

// slackUnescape reverses the three entities Slack writes. Only three, and in
// this order: `&amp;` last would turn an escaped `&lt;` into a literal `<`
// somebody never typed.
func slackUnescape(s string) string {
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.ReplaceAll(s, "&amp;", "&")
}

// refuseUnlistedSlackDirs refuses an export holding messages for a room
// neither list names.
func refuseUnlistedSlackDirs(dir string, read []importChannel) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", dir, err)
	}
	var unlisted []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if slices.ContainsFunc(read, func(c importChannel) bool {
			return c.Name == entry.Name()
		}) {
			continue
		}
		unlisted = append(unlisted, entry.Name())
	}
	if len(unlisted) == 0 {
		return nil
	}
	slices.Sort(unlisted)
	return fmt.Errorf("%s holds message directories for rooms that neither "+
		"channels.json nor groups.json names: %s\n\n"+
		"Nothing in this archive says whether those rooms were public or "+
		"private, and a private conversation imported as a public room is a "+
		"disclosure nothing undoes. Add them to the right list file, or "+
		"remove the directories",
		dir, strings.Join(unlisted, ", "))
}

// ---- Mattermost ---------------------------------------------------------//

// mattermostLine is one line of a bulk export, read in two passes: the
// envelope every line has, then the payload its type names.
type mattermostLine struct {
	Type    string          `json:"type"`
	Channel json.RawMessage `json:"channel"`
	Post    json.RawMessage `json:"post"`
}

type mattermostChannel struct {
	Team        string `json:"team"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
}

type mattermostPost struct {
	Team     string               `json:"team"`
	Channel  string               `json:"channel"`
	User     string               `json:"user"`
	Message  string               `json:"message"`
	CreateAt int64                `json:"create_at"`
	Replies  []mattermostPostBase `json:"replies"`
}

type mattermostPostBase struct {
	User     string `json:"user"`
	Message  string `json:"message"`
	CreateAt int64  `json:"create_at"`
}

// readMattermostArchive reads a bulk export's JSONL.
func readMattermostArchive(root, path string) (*importArchive, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the operator named this path
	if err != nil {
		return nil, fmt.Errorf("reading the bulk export %s: %w", path, err)
	}
	out := &importArchive{Source: string(integration.KindMattermost), Root: root,
		Users: map[string]string{}}
	rooms := map[string]*importChannel{}
	var order []string
	// THE VENDOR ID IS DERIVED AND THEREFORE HAS TO BE DISAMBIGUATED. A
	// bulk export carries no post id at all — see [mattermostVendorID] —
	// so two posts by one user in one channel at one millisecond would
	// otherwise share an identity, and the second would decline as a
	// duplicate of the first.
	seen := map[string]int{}

	for index, text := range strings.Split(string(raw), "\n") {
		where := fmt.Sprintf("%s:%d", path, index+1)
		if strings.TrimSpace(text) == "" {
			continue
		}
		var envelope mattermostLine
		if err := json.Unmarshal([]byte(text), &envelope); err != nil {
			return nil, fmt.Errorf("%s: this line is not the JSON a bulk "+
				"export is made of: %w", where, err)
		}
		switch envelope.Type {
		case "channel":
			if err := readMattermostChannel(envelope, where, rooms, &order); err != nil {
				return nil, err
			}
		case "post":
			if err := readMattermostPost(envelope, where, rooms, seen, out); err != nil {
				return nil, err
			}
		case "":
			return nil, fmt.Errorf("%s: this line declares no `type`, so "+
				"nothing says what it is", where)
		default:
			// EVERY OTHER DECLARED LINE IS COUNTED, not refused: a bulk
			// export carries teams, users, emoji, schemes and direct
			// conversations, and an import that refused the file over a
			// line it does not need would refuse every real export.
			// Direct conversations are among them deliberately — see
			// [slackChannelLists].
			out.skip("a `" + envelope.Type + "` line, which this import does not read")
		}
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("%s declares no channels, so nothing in it "+
			"says which rooms these messages were said in", path)
	}
	for _, key := range order {
		out.Channels = append(out.Channels, *rooms[key])
	}
	return out, nil
}

func readMattermostChannel(line mattermostLine, where string,
	rooms map[string]*importChannel, order *[]string) error {

	var room mattermostChannel
	if err := json.Unmarshal(line.Channel, &room); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	if room.Name == "" {
		return fmt.Errorf("%s: a channel with no name, which is what this "+
			"import resolves a native room by", where)
	}
	kind, err := mattermostKind(room.Type)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	key := room.Team + "/" + room.Name
	if _, already := rooms[key]; already {
		return fmt.Errorf("%s: %s is declared twice, so two sets of messages "+
			"would import into one room under one identity", where, key)
	}
	rooms[key] = &importChannel{Name: room.Name, Kind: kind}
	*order = append(*order, key)
	return nil
}

// mattermostKind reads a channel's type.
//
// THE OPEN ENUM IS REFUSED rather than defaulted, for the reason
// [importChannel.Kind] gives: `O` and `P` are what this build knows, and a
// type it does not classify becoming public is the failure that cannot be
// taken back.
func mattermostKind(kind string) (chat.Kind, error) {
	switch kind {
	case "O":
		return chat.KindPublic, nil
	case "P":
		return chat.KindPrivate, nil
	case "":
		return "", errors.New("a channel with no `type`, so nothing says " +
			"whether it was public or private")
	}
	return "", fmt.Errorf("`type: %s` is a channel kind this build does not "+
		"read (it knows `O` for open and `P` for private), and a room whose "+
		"privacy nothing states is not one to guess at", kind)
}

func readMattermostPost(line mattermostLine, where string,
	rooms map[string]*importChannel, seen map[string]int, out *importArchive) error {

	var post mattermostPost
	if err := json.Unmarshal(line.Post, &post); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	key := post.Team + "/" + post.Channel
	room, declared := rooms[key]
	if !declared {
		return fmt.Errorf("%s: a post in %s, which no `channel` line above it "+
			"declares — so nothing in this archive says whether that room was "+
			"public or private", where, key)
	}
	root, err := mattermostMessageOf(mattermostPostBase{
		User: post.User, Message: post.Message, CreateAt: post.CreateAt,
	}, key, where, seen, out)
	if err != nil {
		return err
	}
	if root.VendorID != "" {
		room.Messages = append(room.Messages, root)
	}
	for _, reply := range post.Replies {
		// A REPLY WHOSE ROOT WAS NOT IMPORTED still belongs in the room.
		// The root is skipped only when it carried no text at all — an
		// upload — and dropping the answers to it would take a
		// conversation out of the transcript because of what somebody
		// attached to its first line.
		read, err := mattermostMessageOf(reply, key, where, seen, out)
		if err != nil {
			return err
		}
		if read.VendorID == "" {
			continue
		}
		read.RootVendorID = root.VendorID
		room.Messages = append(room.Messages, read)
	}
	return nil
}

func mattermostMessageOf(post mattermostPostBase, channelKey, where string,
	seen map[string]int, out *importArchive) (importMessage, error) {

	if post.User == "" {
		return importMessage{}, fmt.Errorf("%s: a post names no `user`, so "+
			"nothing in this archive says who said it — and this import never "+
			"guesses an author", where)
	}
	if post.CreateAt <= 0 {
		return importMessage{}, fmt.Errorf("%s: a post by %s states no "+
			"`create_at`, so there is no instant to render it at and no stable "+
			"half of its identity across two passes", where, post.User)
	}
	if _, already := out.Users[post.User]; !already {
		// A BULK EXPORT NAMES ITS USERS BY USERNAME on the post itself,
		// so the id IS the display name here and the refusal for an
		// unmapped author still says who it is about.
		out.Users[post.User] = post.User
	}
	body := strings.TrimSpace(post.Message)
	if body == "" {
		out.skip("a post carrying no text (an upload, which this engine " +
			"stores no bytes for)")
		return importMessage{}, nil
	}
	return importMessage{
		VendorID:   mattermostVendorID(channelKey, post, seen),
		Author:     post.User,
		AuthoredAt: time.UnixMilli(post.CreateAt).UTC(),
		Body:       body,
		Where:      where,
	}, nil
}

// mattermostVendorID is a post's identity in an archive that gives it none.
//
// A Mattermost bulk export carries no post id — the format is written to be
// imported into a fresh installation, which mints its own — so the identity
// has to come from what IS in the file. (team, channel, instant, user) is the
// only tuple that is stable across two passes over one archive, and the
// ordinal behind it is what keeps two posts by one person in one millisecond
// two messages rather than one.
//
// The ordinal is a property of THIS archive's order, which is a file and
// therefore stable. It is deliberately not derived from the body: somebody
// who said "ok" twice in a millisecond said it twice.
func mattermostVendorID(channelKey string, post mattermostPostBase,
	seen map[string]int) string {

	base := fmt.Sprintf("%s/%d/%s", channelKey, post.CreateAt, post.User)
	n := seen[base]
	seen[base] = n + 1
	if n == 0 {
		return base
	}
	return base + "#" + strconv.Itoa(n)
}

// ---- reading JSON with a line number ------------------------------------//

// readJSONArray walks a JSON array, handing each element the file and line it
// starts on.
//
// THE LINE IS THE POINT. A refusal that named only the file would send an
// operator scrolling through a day of somebody's workspace; encoding/json's
// own errors carry a byte offset, which is worse than nothing because it
// looks like something an editor can use and is not.
func readJSONArray(path string, each func(json.RawMessage, string) error) error {
	raw, err := os.ReadFile(path) //nolint:gosec // the operator named this path
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	open, err := dec.Token()
	if err != nil {
		return decodeFailure(path, raw, dec, err)
	}
	if delim, isDelim := open.(json.Delim); !isDelim || delim != '[' {
		return fmt.Errorf("%s:1: this file is not the JSON array an export is "+
			"made of; it starts with %v", path, open)
	}
	for dec.More() {
		at := dec.InputOffset()
		var elem json.RawMessage
		if err := dec.Decode(&elem); err != nil {
			return decodeFailure(path, raw, dec, err)
		}
		if err := each(elem, locateElement(path, raw, at)); err != nil {
			return err
		}
	}
	return nil
}

// decodeFailure renders a decode error as `file:line`, at the byte that
// actually offended.
//
// encoding/json reports an offset as "the error occurred AFTER reading this
// many bytes", so the byte to point at is the one before it — which matters
// exactly where it is easiest to get wrong: an unterminated string fails on
// the newline that ends its line, and reporting the byte after it names the
// NEXT line, sending an operator to a line that is perfectly well formed.
func decodeFailure(path string, data []byte, dec *json.Decoder, err error) error {
	offset := dec.InputOffset()
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		offset = syntax.Offset
	case errors.As(err, &typed):
		offset = typed.Offset
	}
	if offset > 0 {
		offset--
	}
	return fmt.Errorf("%s: %w", locateExact(path, data, offset), err)
}

// locateExact renders a byte offset as `file:line`, pointing at that byte and
// no other.
func locateExact(path string, data []byte, offset int64) string {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	return fmt.Sprintf("%s:%d", path, bytes.Count(data[:offset], []byte("\n"))+1)
}

// locateElement renders the line an array element STARTS on.
//
// A decoder's position after a token is the byte AFTER it, which on a
// pretty-printed export is the end of the previous line — so the whitespace
// and the separating comma between there and the element are skipped. That is
// the opposite of what [locateExact] must do, which is why they are two
// functions rather than one with a flag: an error's offset points at a byte
// and an element's points before one.
func locateElement(path string, data []byte, offset int64) string {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	i := int(offset)
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' ||
		data[i] == '\n' || data[i] == '\r' || data[i] == ',') {
		i++
	}
	if i > len(data) {
		i = len(data)
	}
	return fmt.Sprintf("%s:%d", path, bytes.Count(data[:i], []byte("\n"))+1)
}

// ---- the operator's map --------------------------------------------------//

// importMap is what the operator writes to join an archive to this company.
//
// ONE DOCUMENT WITH TWO SECTIONS rather than two files, because it is one
// decision — "this archive is this company's history" — and an operator
// holding half of it has a run that refuses.
type importMap struct {
	// Channels maps an archive channel's name to the native room it goes
	// into, by id or by name. Absent means the names match.
	Channels map[string]string `yaml:"channels"`

	// Authors maps a vendor user id to the seat handle that person or
	// agent has here. A handle that is no longer a seat is ACCEPTED: a
	// person who has left still said what they said, and a transcript
	// that silently renamed them would be a worse record than one naming
	// somebody who is gone.
	Authors map[string]string `yaml:"authors"`
}

func loadImportMap(path string) (importMap, error) {
	if path == "" {
		return importMap{}, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the operator named this path
	if err != nil {
		return importMap{}, fmt.Errorf("reading the import map %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// STRICT, because the whole document is two keys and a typo in either
	// is a map that parses to nothing: `author:` would leave every author
	// unmapped and the run would refuse with a list the operator has
	// already written out.
	dec.KnownFields(true)
	var out importMap
	if err := dec.Decode(&out); err != nil && !errors.Is(err, io.EOF) {
		return importMap{}, fmt.Errorf("reading the import map %s: %w\n\n"+
			"It carries two keys, `channels:` and `authors:`, each a mapping "+
			"of archive name to this company's", path, err)
	}
	return out, nil
}

// ---- resolving the archive against the company --------------------------//

// importRoom is one native room this credential's seat can post in.
type importRoom struct {
	ID   string
	Name string
	Kind string
}

// importRooms reads the rooms this seat is in.
//
// THE RAIL RATHER THAN A LOOKUP BY NAME, and that is not a shortcut: an
// imported post takes the room's own membership gate exactly as anybody
// else's does, so a room this seat is not in is a room this import cannot
// write to whatever its name is. Resolving against the rail therefore refuses
// at the right moment — before a single message is written — rather than
// message by message against a node that answers 403.
func importRooms(ctx context.Context, client *nodeClient) ([]importRoom, error) {
	var answer chatChannelListing
	if err := client.get(ctx, "/chat/channels?include_archived=true", &answer); err != nil {
		return nil, err
	}
	out := make([]importRoom, 0, len(answer.Channels))
	for _, row := range answer.Channels {
		out = append(out, importRoom{ID: row.Channel.ID,
			Name: row.Channel.Name, Kind: row.Channel.Kind})
	}
	if !answer.Complete {
		// AN INCOMPLETE RAIL IS REFUSED, because the failure it produces
		// is the expensive one: a room missing from the list reads
		// exactly like a room that does not exist, and the refusal for
		// that tells an operator to create a room they already have.
		return nil, errors.New("this node has not applied the whole chat log, " +
			"so the room list is incomplete — a room missing from it reads " +
			"exactly like one that does not exist. Re-run once it has caught up")
	}
	return out, nil
}

// importAgentHandles is every AGENT seat's handle, which is how an author's
// kind is decided.
//
// `GET /agents` answers agent seats and no others, so membership in it IS the
// kind: a mapped handle it names is an `agent`, and everything else is a
// `human`. That fallback is the right way round — a handle that is no longer
// a seat at all is a person who has left, and people are what a chat archive
// is mostly made of.
func importAgentHandles(ctx context.Context, client *nodeClient) (map[string]bool, error) {
	var rows []struct {
		Handle string `json:"handle"`
	}
	if err := client.get(ctx, "/agents", &rows); err != nil {
		return nil, fmt.Errorf("reading this company's agent seats: %w\n\n"+
			"They are what decides whether an imported author is recorded as "+
			"an agent or as a person", err)
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.Handle != "" {
			out[row.Handle] = true
		}
	}
	return out, nil
}

// importPlan is every message this run will send, in the order it will send
// them, with everything already resolved.
//
// BUILT WHOLE BEFORE ANYTHING IS WRITTEN. Every refusal this command has —
// an unmapped author, a room that is not here, a body over the cap — is a
// property of the archive rather than of the network, so finding them all at
// once costs one read of a directory and saves an operator a partial import
// per problem.
type importPlan struct {
	Source string
	Rooms  []importPlanRoom

	// Reroot counts replies whose root is not in this archive. They import
	// as room posts rather than being dropped: an export that starts
	// mid-conversation is ordinary, and an answer nobody can see is still
	// something somebody said.
	Reroot int

	// Agents is how many authors resolved to an agent seat, for the
	// summary — an import that silently recorded every bot as a person
	// would be visible nowhere else.
	Agents int
}

type importPlanRoom struct {
	Archive   string
	ChannelID string
	Messages  []importSubmission
}

func (p importPlan) messageCount() int {
	var n int
	for _, room := range p.Rooms {
		n += len(room.Messages)
	}
	return n
}

// buildImportPlan resolves an archive against this company, or refuses.
//
// # The unmapped author, and the three alternatives it is chosen over
//
// A vendor user with no mapping STOPS THE RUN, and the refusal lists every
// one of them with the name the vendor displayed and the `authors:` lines to
// paste. Nothing is imported until every author in the archive is accounted
// for.
//
// Dropping the message is the option this is chosen over first, and it is the
// worst: a conversation missing one side of itself is a record that misleads
// rather than one that is incomplete, and a dropped root takes its whole
// thread's shape with it. Attributing to a placeholder handle — `unknown`,
// `former-employee` — puts words in a seat nobody holds and makes "who said
// this" unanswerable for ever, in the one store where that question is the
// whole point. Attributing to the operator running the migration is worse
// still: the transcript would say one person said everything in it.
//
// So: the operator says who, or nothing is written. It costs one line per
// person in a file they write once, it is checkable with -check before any
// node is touched, and it is the only option that cannot quietly produce a
// false record.
func buildImportPlan(archive *importArchive, mapping importMap,
	rooms []importRoom, agents map[string]bool) (importPlan, error) {

	plan := importPlan{Source: archive.Source}
	var missingRooms, widened, oversize []string
	unmapped := map[string]bool{}

	for _, room := range archive.Channels {
		if len(room.Messages) == 0 {
			continue
		}
		target, found := resolveImportRoom(room, mapping, rooms)
		if !found {
			missingRooms = append(missingRooms, room.Name)
			continue
		}
		// A PRIVATE ROOM MAY NOT WIDEN ON THE WAY ACROSS. This is the
		// one thing the archive's own kind is FOR, and it is the
		// disclosure the rest of this command's refusals are shaped
		// around: a channel that was private in the workspace, resolved
		// — by a name match or by a line of the operator's map — onto a
		// room the whole company reads, publishes every word of it at
		// once, on every node, with no inverse.
		if room.Kind == chat.KindPrivate && !importRoomIsRestricted(target.Kind) {
			widened = append(widened, fmt.Sprintf(
				"%s (private in the archive) -> %s (%s here)",
				room.Name, target.ID, target.Kind))
			continue
		}
		// THE ARCHIVE'S OWN IDS, so a reply's root resolves without a
		// read back — see the package comment.
		here := map[string]bool{}
		for _, msg := range room.Messages {
			here[msg.VendorID] = true
		}
		planned := importPlanRoom{Archive: room.Name, ChannelID: target.ID}
		for _, msg := range room.Messages {
			handle, mapped := mapping.Authors[msg.Author]
			if !mapped || strings.TrimSpace(handle) == "" {
				unmapped[msg.Author] = true
				continue
			}
			if size := len(msg.Body); size > chat.MaxBody {
				// REFUSED NAMING THE MESSAGE, never cut to fit.
				// A body shortened to fit is a record of
				// something nobody said, and the one surface
				// where that matters most is the one that keeps
				// what people said.
				oversize = append(oversize, fmt.Sprintf(
					"%s (%s, %d bytes against a %d byte cap)",
					msg.Where, msg.VendorID, size, chat.MaxBody))
				continue
			}
			kind := chat.AuthorHuman
			if agents[handle] {
				kind = chat.AuthorAgent
				plan.Agents++
			}
			out := importSubmission{
				Source: archive.Source, VendorID: msg.VendorID,
				Author: handle, AuthorKind: string(kind),
				AuthoredAt: msg.AuthoredAt, Body: msg.Body,
			}
			switch {
			case msg.RootVendorID == "":
			case here[msg.RootVendorID]:
				root, err := chat.ImportedMessageID(archive.Source, msg.RootVendorID)
				if err != nil {
					return importPlan{}, fmt.Errorf("%s: %w", msg.Where, err)
				}
				out.ReplyTo = root.String()
			default:
				plan.Reroot++
			}
			planned.Messages = append(planned.Messages, out)
		}
		plan.Rooms = append(plan.Rooms, planned)
	}

	if len(unmapped) > 0 {
		return importPlan{}, unmappedAuthors(archive, unmapped)
	}
	if len(missingRooms) > 0 {
		slices.Sort(missingRooms)
		return importPlan{}, fmt.Errorf(
			"this company has no room for %d of the archive's channels: %s\n\n"+
				"This import never creates one: a room's kind is a disclosure "+
				"decision and its membership is what a private room's "+
				"readability IS, so a room it invented would be either "+
				"unreachable or published. Create each room, join it (an "+
				"imported post takes the room's own membership gate), and "+
				"re-run — or map the archive's name onto a room you have:\n\n"+
				"channels:\n  %s: <this company's room>",
			len(missingRooms), strings.Join(missingRooms, ", "), missingRooms[0])
	}
	if len(widened) > 0 {
		slices.Sort(widened)
		return importPlan{}, fmt.Errorf(
			"%d of the archive's private channels resolve to rooms the whole "+
				"company reads:\n  %s\n\n"+
				"Nothing was written. Importing them there publishes every "+
				"word at once, on every node, and no gesture takes it back. "+
				"Point each one at a private room with `channels:` in the "+
				"map, or make the room private before re-running",
			len(widened), strings.Join(widened, "\n  "))
	}
	if len(oversize) > 0 {
		slices.Sort(oversize)
		return importPlan{}, fmt.Errorf(
			"%d message(s) are longer than one message may be:\n  %s\n\n"+
				"They are refused rather than shortened: a body cut to fit is "+
				"a record of something nobody said. Trim them in the archive "+
				"— it is a file you hold — and re-run",
			len(oversize), strings.Join(oversize, "\n  "))
	}
	if plan.messageCount() == 0 {
		return importPlan{}, fmt.Errorf(
			"%s holds nothing to import: %d message(s) were read and none of "+
				"them resolved to a room this seat is in",
			archive.Root, archive.messageCount())
	}
	return plan, nil
}

// importRoomIsRestricted reports whether a native room's readership is its
// membership.
//
// A KIND THIS BUILD CANNOT CLASSIFY IS NOT RESTRICTED, which is the same rule
// the write path takes for the same reason: an open enum read two-valued
// falls out as the permissive answer, and here the permissive answer is a
// private conversation published to everybody. A newer peer's room kind
// therefore refuses the import rather than being trusted with it.
func importRoomIsRestricted(kind string) bool {
	switch chat.Kind(kind) {
	case chat.KindPrivate, chat.KindDM, chat.KindGroup:
		return true
	}
	// KindPublic and KindUnit are both readable by any seat of the
	// company, joined or not — see [chat.Reader]'s own reachability rule
	// — so neither is somewhere a private conversation may land.
	return false
}

// resolveImportRoom finds the native room an archive channel goes into.
//
// THE MAP FIRST, AND THE NAME ONLY AFTER IT, because a company that renamed a
// room on the way across has said so explicitly and an implicit name match
// that beat it would silently import into the room they moved away from. A
// mapped value is read as an id and then as a name, in that order, so an
// operator may write either — an id is unambiguous and a name is what they
// can see.
func resolveImportRoom(room importChannel, mapping importMap, rooms []importRoom) (
	importRoom, bool) {

	wanted := room.Name
	if mapped := strings.TrimSpace(mapping.Channels[room.Name]); mapped != "" {
		wanted = mapped
	}
	for _, have := range rooms {
		if have.ID == wanted {
			return have, true
		}
	}
	for _, have := range rooms {
		if have.Name == wanted {
			return have, true
		}
	}
	return importRoom{}, false
}

// unmappedAuthors renders the refusal an operator acts on, with the lines to
// paste into their map.
func unmappedAuthors(archive *importArchive, unmapped map[string]bool) error {
	ids := make([]string, 0, len(unmapped))
	for id := range unmapped {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	lines := make([]string, 0, len(ids))
	for _, id := range ids {
		who := archive.Users[id]
		if who == "" {
			who = "no name in this archive"
		}
		lines = append(lines, fmt.Sprintf("  %s: <handle>   # %s", id, who))
	}
	return fmt.Errorf(
		"%d of the archive's authors are not mapped to a seat, so nothing was "+
			"written:\n\nauthors:\n%s\n\n"+
			"Every message keeps the name of whoever said it, and this import "+
			"neither guesses nor drops: a dropped message takes its thread's "+
			"shape with it, a placeholder handle makes \"who said this\" "+
			"unanswerable, and attributing to the credential running the "+
			"migration would have one person saying everything in the "+
			"archive. A handle that is no longer a seat is fine — somebody "+
			"who left still said what they said. Put the file behind -map and "+
			"re-run; -check reports without writing",
		len(ids), strings.Join(lines, "\n"))
}

// report prints what the plan will do, before it does any of it.
func (p importPlan) report(stdout io.Writer, archive *importArchive) {
	fmt.Fprintf(stdout, "%s export at %s: %d message(s) in %d room(s)\n",
		p.Source, archive.Root, p.messageCount(), len(p.Rooms))
	for _, room := range p.Rooms {
		fmt.Fprintf(stdout, "  %-24s -> %-24s %d message(s)\n",
			room.Archive, room.ChannelID, len(room.Messages))
	}
	if p.Agents > 0 {
		fmt.Fprintf(stdout, "  %d message(s) attributed to agent seats.\n", p.Agents)
	}
	if p.Reroot > 0 {
		fmt.Fprintf(stdout, "  %d repl(ies) whose thread root is not in this "+
			"archive import as room posts.\n", p.Reroot)
	}
	reasons := make([]string, 0, len(archive.Skipped))
	for reason := range archive.Skipped {
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	for _, reason := range reasons {
		fmt.Fprintf(stdout, "  %d skipped: %s\n", archive.Skipped[reason], reason)
	}
}

// ---- writing it ---------------------------------------------------------//

// runImport sends the plan, one message at a time.
//
// # Why this is strictly sequential, and why that is not a knob
//
// ORDER IS LOAD-BEARING. A reply is filed against its root's row, which the
// write path reads inside its own snapshot — so a reply published before its
// root is a reply the decide refuses. Messages are planned in the order the
// archive says they were said, which puts every root ahead of its answers,
// and concurrency would reorder exactly that. So there is no worker count
// here and nothing to tune: a faster import would be a broken one.
//
// # What an interruption costs
//
// Nothing but the time. Each message is its own record and its own apply
// transaction, so there is no half-written batch to undo, and the derived ids
// make a re-run decline everything that already landed. The command therefore
// has no resume flag and no cursor file: the second run IS the resume.
func runImport(ctx context.Context, stdout io.Writer, writer importWriter,
	plan importPlan, limit int) error {

	var sent, already int
	for _, room := range plan.Rooms {
		for _, msg := range room.Messages {
			if limit > 0 && sent >= limit {
				fmt.Fprintf(stdout, "\n-limit %d reached: %d message(s) "+
					"written. Re-run to continue — nothing already written "+
					"is written again.\n", limit, sent)
				return nil
			}
			answer, err := writer.Import(ctx, room.ChannelID, msg)
			if err != nil {
				return fmt.Errorf(
					"importing %s into %s after %d message(s): %w\n\n"+
						"Nothing has to be undone: each message is its own "+
						"record, and re-running the same archive writes "+
						"nothing that already landed",
					msg.VendorID, room.ChannelID, sent, err)
			}
			sent++
			if answer.Outcome == "pending" {
				// COUNTED, NOT WARNED ABOUT. `pending` is a durable
				// write this node has not applied yet, which on an
				// import of thousands is the ordinary case rather
				// than an event — a line per message would bury
				// the summary the operator is actually reading.
				already++
			}
			if sent%importProgressEvery == 0 {
				fmt.Fprintf(stdout, "  %s: %d/%d\n", room.Archive, sent,
					plan.messageCount())
			}
		}
	}
	fmt.Fprintf(stdout, "\nImported %d message(s); %d not yet applied on this "+
		"node.\n", sent, already)
	fmt.Fprintln(stdout, "Nobody was woken: an imported message carries "+
		"provenance and no routing snapshot, so a year of mentions is history "+
		"rather than tens of thousands of turns.")
	return nil
}
