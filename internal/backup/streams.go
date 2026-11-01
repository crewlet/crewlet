package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/jsapi"
)

// Snapshotting the stream estate, and why it is the wire API.
//
// Everything a fleet agrees on lives in JetStream: the agent mailboxes, the
// engine's own streams, and every coordination bucket — leases, the
// activation pointer, the completion ledger, the budget counters, the sealed
// credentials. On the default topology that broker is EMBEDDED in the engine
// and binds no socket at all (`DontListen`), so the `nats` CLI cannot reach
// it: there is no address to give it. Nothing outside this process can back
// this estate up.
//
// Inside the process the options are narrower than they look. The vendored
// nats.go has no snapshot call on either JetStream API — the whole module has
// no Snapshot or Restore method — and jsm.go, which does, is not a dependency
// here. The server's own snapshot is a method on an unexported type reachable
// only through unexported lookups; the two exported "Snapshot" methods on
// *server.Server are RAFT metadata snapshots, produce no bytes, and refuse
// outright on a solo node.
//
// What is left is the thing the `nats` CLI itself uses: the JetStream API on
// the wire, over the connection this process already holds. The request and
// response types are the server's own — imported rather than restated, so a
// protocol change is a compile error instead of a JSON field that silently
// stops matching.
//
// # The protocol, since it is not obvious from the types
//
// A snapshot is a request naming a subject the server should deliver to, then
// a stream of chunk messages on that subject. Two things are easy to get
// wrong and both are silent:
//
//   - EVERY CHUNK MUST BE ANSWERED. The chunk's reply subject is a flow
//     control credit, not an acknowledgement anybody reads. A receiver that
//     does not reply stalls the server, which gives up after its own timeout
//     and aborts the transfer — so a backup that forgets this succeeds on
//     small streams and truncates large ones.
//   - THE TERMINATOR IS AN EMPTY MESSAGE, and it carries the verdict in its
//     status header: 204 for a complete snapshot, 408 or 500 for one the
//     server abandoned. A receiver that stops at the empty message without
//     reading that header writes a truncated file and reports success.
//
// And one that is easy to get wrong the other way, because it looks like a
// failure of the node: A CONSUMER THAT ENDS WHILE ITS STREAM IS COPIED
// ABANDONS THE COPY. The server copies a stream's consumers before its
// messages, from a list it takes as the snapshot starts, and gives the whole
// snapshot up (500) when one of them has ended by the time it reads that
// consumer's state. A working engine ends consumers all the time — every
// listing of a coordination bucket is an ephemeral consumer that lives as long
// as the listing, and a removed seat's mailbox ends with it — so that is a race
// the copy loses at a rate set by load, and the stream is snapshotted again
// (see [snapshotStream]).

// snapshotChunkSize is what the server is asked to send per chunk.
//
// 128 KB is the server's own default and the middle of what it accepts
// (1 KB to 1 MB, clamped). Left at the default deliberately: the tuning this
// knob exists for is a slow or lossy link, and this transfer is between two
// halves of one process over an in-memory connection, where the only effect
// of a larger chunk is a larger buffer.
const snapshotChunkSize = 128 * 1024

// snapshotChunkWait bounds the wait for the NEXT chunk, not the transfer.
//
// The server aborts its side after its own ack timeout, so this only has to
// outlast a healthy pause: a chunk read from disk plus the round trip that
// credits it. Thirty seconds is far past that and still bounded, because the
// alternative to a bound is a backup that hangs a request forever when the
// broker stops talking mid-stream.
const snapshotChunkWait = 30 * time.Second

// snapshotRequestTimeout bounds the initial request — the one round trip that
// asks the server to start. The server waits up to two seconds for interest
// on the deliver subject before giving up, so this must comfortably outlast
// that.
const snapshotRequestTimeout = 10 * time.Second

// snapshotAttempts is how many snapshots of one stream a backup takes before a
// consumer ending under each of them is reported rather than taken again.
//
// FIVE. One loss needs a consumer to end in the instant between the server
// listing the stream's consumers and reading each one's state. Measured: one
// backup in about fifty lost it (3 of 160) on a single node with sixteen
// engines contending for four CPUs, every loss on the positions bucket, which
// every create's zero fence and every tick of the trim list — so five losses
// in a row is a stream on which something creates and ends consumers far
// faster than any listing of the engine's does, which is worth an operator's
// attention rather than a sixth attempt. A lost attempt costs almost nothing: the consumers are copied
// before a single message, so the server gives up before it has sent one.
const snapshotAttempts = 5

// retakeFirstWait, retakeMaxWait and retakeJitter space the attempts.
//
// The engine's listings come in BURSTS — one tick of the trim reads four key
// classes of the positions bucket back to back, each its own consumer — so an
// attempt taken the instant one of them ended lands among its neighbours. Ten
// milliseconds doubling to eighty, four waits and 150 ms in all, steps past a
// burst on a broker under the load that makes the race likely, and is nothing
// beside the backup it is part of. The jitter is the fifth the engine's other
// retry loops spread by.
const (
	retakeFirstWait = 10 * time.Millisecond
	retakeMaxWait   = 80 * time.Millisecond
	retakeJitter    = 0.2
)

// StreamArtifact is one snapshotted stream in a backup.
type StreamArtifact struct {
	// Name is the stream.
	//
	// A COORDINATION BUCKET APPEARS HERE TOO, under the stream name
	// JetStream stores it as: KV_<bucket>. A bucket is not a separate kind
	// of thing to back up — it IS one of these streams — which is why
	// enumerating streams captures the fleet's leases, ledgers, counters
	// and sealed credentials without naming a single bucket.
	Name string `json:"name"`

	// File is the snapshot, relative to the backup directory.
	File string `json:"file"`

	// Bytes is the snapshot's size.
	Bytes int64 `json:"bytes"`

	// Messages is how many the stream held when the snapshot was taken.
	// Recorded because it is the one number an operator can sanity-check a
	// restore against.
	Messages uint64 `json:"messages"`

	// Config and State are the server's own descriptions of the stream,
	// kept VERBATIM because a restore has to hand both back unchanged. Raw
	// JSON rather than decoded structs so that a field this build does not
	// know still survives the round trip — the same additive-evolution
	// rule the event envelope holds to, for the same reason: a backup
	// taken by one build is restored by another.
	Config json.RawMessage `json:"config"`
	State  json.RawMessage `json:"state"`
}

// snapshotStreams captures every stream the broker holds under root, the
// stream named last — the object store's own bucket on `nats` — after every
// other.
//
// EVERY STREAM, ENUMERATED RATHER THAN LISTED. The engine's own five are
// known here, but a namespace stream is created on first publish to it
// (CREWLET_NS_*) and a coordination bucket is a stream whose name depends on
// a configurable prefix — so a hardcoded list would silently omit whatever a
// deployment actually has. What is backed up is what is there.
//
// THE OBJECT STREAM AFTER EVERY LOG, as a rule rather than an accident of
// sorting (`CREWLET_` happens to sort before `OBJ_`). A restore replays each
// log to the last sequence its snapshot captured, and an object is uploaded
// before the record naming it is written, so every object a record of a log's
// snapshot names was in the bucket before the object stream's snapshot began —
// which is what lets the backup count an object the store holds after the
// snapshots as one the object stream's snapshot carries ([objectPass.finish]).
// Snapshotted first, the bucket would miss every file written while the logs'
// snapshots ran.
//
// The snapshots land in a subdirectory of root, and each artifact's File is
// relative to root rather than to that subdirectory, because that is the path
// a restore reading the manifest has to join.
func snapshotStreams(ctx context.Context, nc *nats.Conn, api jsapi.API, root, last string) ([]StreamArtifact, error) {
	js, err := api.Client(nc)
	if err != nil {
		return nil, fmt.Errorf("backup: reach the JetStream API: %w", err)
	}
	names, err := streamNames(ctx, js)
	if err != nil {
		return nil, err
	}
	if i := slices.Index(names, last); last != "" && i >= 0 {
		names = append(slices.Delete(names, i, i+1), last)
	}
	if err := os.MkdirAll(filepath.Join(root, streamDirName), 0o700); err != nil {
		return nil, fmt.Errorf("backup: create %s: %w", filepath.Join(root, streamDirName), err)
	}

	artifacts := make([]StreamArtifact, 0, len(names))
	for _, name := range names {
		artifact, err := snapshotStream(ctx, nc, js, api, name, root)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

// streamNames lists every stream, sorted.
//
// Sorted so a manifest diffs cleanly between two backups of the same
// deployment, whatever order a listing comes back in: an operator comparing
// yesterday's manifest with today's should see what changed rather than a
// reshuffle.
//
// FROM THE METADATA LEADER, each page asked again while nobody answers it
// ([jsapi.Meta.StreamNames]): only it answers, so a member that still
// believes in a leader the group has just lost drops the request without a
// word, and the client's lister held its one request for five seconds and
// failed the backup over a reply nobody would send. And every stream that
// exists throughout the listing is in it, so no stream is left out of a
// backup because another was deleted while the listing ran.
func streamNames(ctx context.Context, js jetstream.JetStream) ([]string, error) {
	meta, err := jsapi.NewMeta(js)
	if err != nil {
		return nil, fmt.Errorf("backup: list streams: %w", err)
	}
	var names []string
	if err := meta.StreamNames(ctx, func(name string) error {
		names = append(names, name)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("backup: list streams: %w", err)
	}
	slices.Sort(names)
	return names, nil
}

// snapshotStream writes one stream's snapshot under root, taking it again
// while the server abandons it over a consumer that ended under the copy.
//
// WHAT THE SERVER LOST IS THE RACE, NOT THE STREAM. It copies the consumers it
// listed as the snapshot started and gives the whole snapshot up when one of
// them has ended by the time it reads that consumer's state (see the file
// doc), and a consumer that has ended holds nothing a restore could want — so
// another snapshot, taken now, is exactly what the lost one would have been: a
// copy of the stream at one moment. Answered as a failure, it failed the
// backup of a node with nothing wrong with it, at a rate set by how busy the
// node was ([snapshotAttempts] has the measurement).
//
// ASKED, NOT ASSUMED: the abandonment is read as that race only when the
// consumer the server names is one it no longer holds ([endedUnder]). A
// consumer that is still there and could not be read, and every other way a
// snapshot fails, fails the backup as it always did.
func snapshotStream(ctx context.Context, nc *nats.Conn, js jetstream.JetStream, api jsapi.API,
	name, root string,
) (StreamArtifact, error) {
	return retakeOverEnded(ctx, js, name, func() (StreamArtifact, error) {
		return snapshotOnce(ctx, nc, api, name, root)
	})
}

// retakeOverEnded runs take until it returns a snapshot of stream, fails in a
// way a consumer ending does not explain, or has lost [snapshotAttempts] races
// in a row — the last reported naming the consumer.
//
// take is handed in rather than called by name so the race can be held to the
// real server: a case drives the snapshot protocol itself to make the server
// lose it on purpose, and this is the loop it then has to get through.
func retakeOverEnded(ctx context.Context, js jetstream.JetStream, stream string,
	take func() (StreamArtifact, error),
) (StreamArtifact, error) {
	for attempt := 1; ; attempt++ {
		artifact, err := take()
		if err == nil {
			return artifact, nil
		}
		consumer, ended := endedUnder(ctx, js, stream, err)
		if !ended {
			return StreamArtifact{}, err
		}
		if attempt == snapshotAttempts {
			return StreamArtifact{}, fmt.Errorf("backup: %s: a consumer ended while "+
				"the server copied the stream on each of %d attempts, the last %q — "+
				"something is creating and ending consumers on it far faster than "+
				"the engine's own listings do: %w", stream, snapshotAttempts, consumer, err)
		}
		log.InfoContext(ctx, "backup_stream_retaken",
			"stream", stream, "consumer", consumer, "attempt", attempt,
			"detail", "the server abandoned the snapshot because this consumer "+
				"ended while it was copied, so the stream is snapshotted again")
		wait := time.NewTimer(backoff.Jitter(
			backoff.Doubling(attempt, retakeFirstWait, retakeMaxWait), retakeJitter))
		select {
		case <-ctx.Done():
			wait.Stop()
			return StreamArtifact{}, fmt.Errorf("backup: %s: %w", stream, ctx.Err())
		case <-wait.C:
		}
	}
}

// endedUnder reports whether err is the server abandoning a snapshot of stream
// over a consumer it no longer holds, and which consumer.
//
// THE CONSUMER IS LOOKED UP, because the server's words say only that it could
// not read the consumer's state: "it has ended" is the server answering that
// it holds no such consumer now. The server removes a consumer from its stream
// before it closes the consumer's store, so a store found closed is a consumer
// already gone from the listing. A lookup that fails some other way is not
// that answer, and the abandonment stands.
func endedUnder(ctx context.Context, js jetstream.JetStream, stream string, err error) (string, bool) {
	var abandoned *abandonedSnapshot
	if !errors.As(err, &abandoned) {
		return "", false
	}
	consumer := abandoned.consumer(stream)
	if consumer == "" {
		return "", false
	}
	askCtx, cancel := context.WithTimeout(ctx, snapshotRequestTimeout)
	defer cancel()
	_, lookupErr := js.Consumer(askCtx, stream, consumer)
	return consumer, errors.Is(lookupErr, jetstream.ErrConsumerNotFound)
}

// snapshotOnce writes one snapshot of one stream under root.
func snapshotOnce(ctx context.Context, nc *nats.Conn, api jsapi.API, name, root string) (StreamArtifact, error) {
	// SUBSCRIBED BEFORE THE REQUEST, and flushed. The server checks for
	// interest on the deliver subject before it starts and waits only
	// about two seconds for it to appear — so a subscription created after
	// the request, or one still sitting in the client's write buffer, is a
	// transfer that never begins.
	inbox := nc.NewRespInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return StreamArtifact{}, fmt.Errorf("backup: subscribe for %s: %w", name, err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	// Bounded on its own: the client requires a deadline here, and an
	// unbounded flush against a broker that has stopped reading would hang
	// the backup before it had asked for anything.
	flushCtx, stopFlush := context.WithTimeout(ctx, snapshotRequestTimeout)
	defer stopFlush()
	if flushErr := nc.FlushWithContext(flushCtx); flushErr != nil {
		return StreamArtifact{}, fmt.Errorf("backup: register interest for %s: %w", name, flushErr)
	}

	request, err := json.Marshal(server.JSApiStreamSnapshotRequest{
		DeliverSubject: inbox,
		ChunkSize:      snapshotChunkSize,
		// CONSUMERS INCLUDED. A stream restored without them is a
		// mailbox with no subscriber: the messages are back and
		// nothing is positioned to read them, so every seat's delivery
		// position — what it had already been handed — is lost.
		NoConsumers: false,
		// Checksums NOT verified. It reads every block of every
		// message before a byte is sent, which on the event stream is
		// the whole retention window; the copy is verified by its own
		// terminator status, and a backup that costs a full read of
		// the estate is one an operator takes less often.
		CheckMsgs: false,
	})
	if err != nil {
		return StreamArtifact{}, fmt.Errorf("backup: encode the request for %s: %w", name, err)
	}

	// IN THE API THE CLIENT SPEAKS, because this request goes past the
	// client: the library has no snapshot call, and a raw subject formed
	// against the account's own API is answered by nothing on a broker that
	// serves its JetStream under a domain.
	subject, err := api.Subject(fmt.Sprintf(server.JSApiStreamSnapshotT, name))
	if err != nil {
		return StreamArtifact{}, fmt.Errorf("backup: address the snapshot of %s: %w", name, err)
	}
	askCtx, cancel := context.WithTimeout(ctx, snapshotRequestTimeout)
	defer cancel()
	reply, err := nc.RequestWithContext(askCtx, subject, request)
	if err != nil {
		return StreamArtifact{}, fmt.Errorf("backup: ask for a snapshot of %s: %w", name, err)
	}
	var answer server.JSApiStreamSnapshotResponse
	if decodeErr := json.Unmarshal(reply.Data, &answer); decodeErr != nil {
		return StreamArtifact{}, fmt.Errorf("backup: read the answer for %s: %w", name, decodeErr)
	}
	if answer.Error != nil {
		return StreamArtifact{}, fmt.Errorf("backup: the broker refused a snapshot of %s: %s",
			name, answer.Error.Description)
	}

	file := filepath.Join(streamDirName, name+".snapshot")
	written, err := receiveSnapshot(ctx, sub, filepath.Join(root, file), name)
	if err != nil {
		return StreamArtifact{}, err
	}

	artifact := StreamArtifact{Name: name, File: file, Bytes: written}
	if answer.Config != nil {
		if artifact.Config, err = json.Marshal(answer.Config); err != nil {
			return StreamArtifact{}, fmt.Errorf("backup: record %s's config: %w", name, err)
		}
	}
	if answer.State != nil {
		artifact.Messages = answer.State.Msgs
		if artifact.State, err = json.Marshal(answer.State); err != nil {
			return StreamArtifact{}, fmt.Errorf("backup: record %s's state: %w", name, err)
		}
	}
	return artifact, nil
}

// receiveSnapshot drains the chunk stream into path, answering each chunk.
func receiveSnapshot(ctx context.Context, sub *nats.Subscription, path, name string) (int64, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("backup: create %s: %w", path, err)
	}
	// Closed on every path, and the close error is CHECKED on the happy
	// one: a snapshot whose last write failed to reach the disk is a
	// truncated file that looks complete, and on a network filesystem
	// close is where that surfaces. Discarding it here would make this
	// comment a claim the code does not keep.
	//
	// closed guards the double close: the happy path closes explicitly so
	// it can read the error, and this runs on every path that did not.
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	var written int64
	for {
		chunk, err := nextChunk(ctx, sub)
		if err != nil {
			_ = os.Remove(path)
			return 0, fmt.Errorf("backup: receive %s: %w", name, err)
		}
		// The empty message ends the transfer, and its status says
		// whether what came before it is a whole snapshot.
		if len(chunk.Data) == 0 {
			if verdictErr := snapshotVerdict(chunk); verdictErr != nil {
				_ = os.Remove(path)
				return 0, fmt.Errorf("backup: %s: %w", name, verdictErr)
			}
			break
		}
		n, err := file.Write(chunk.Data)
		if err != nil {
			_ = os.Remove(path)
			return 0, fmt.Errorf("backup: write %s: %w", path, err)
		}
		written += int64(n)
		// THE CREDIT. Not an acknowledgement anybody reads — the
		// server hands out a fixed number of in-flight slots and this
		// returns one. Skipping it stalls the transfer into the
		// server's ack timeout, which aborts it.
		if chunk.Reply != "" {
			if err := chunk.Respond(nil); err != nil {
				_ = os.Remove(path)
				return 0, fmt.Errorf("backup: keep %s flowing: %w", name, err)
			}
		}
	}
	if err := file.Sync(); err != nil {
		_ = os.Remove(path)
		return 0, fmt.Errorf("backup: flush %s: %w", path, err)
	}
	closed = true
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return 0, fmt.Errorf("backup: close %s: %w", path, err)
	}
	return written, nil
}

// nextChunk waits for the next message, bounded.
func nextChunk(ctx context.Context, sub *nats.Subscription) (*nats.Msg, error) {
	waitCtx, cancel := context.WithTimeout(ctx, snapshotChunkWait)
	defer cancel()
	msg, err := sub.NextMsgWithContext(waitCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("the broker stopped sending mid-snapshot "+
				"(no chunk for %s)", snapshotChunkWait)
		}
		return nil, err
	}
	return msg, nil
}

// snapshotVerdict reads the terminator's status.
//
// 204 is the whole snapshot. Anything else is the server saying it gave up —
// 408 when the transfer stalled or interest vanished, 500 when its own
// snapshot failed — and every one of them arrives looking exactly like a
// normal end of stream. This is the only thing between that and a truncated
// file reported as a backup.
func snapshotVerdict(msg *nats.Msg) error {
	status := msg.Header.Get("Status")
	if status == "" || status == "204" {
		return nil
	}
	return &abandonedSnapshot{Status: status, Description: msg.Header.Get("Description")}
}

// abandonedSnapshot is a snapshot the server gave up on: its terminator's
// status and the server's own description of why.
//
// A TYPE rather than a sentence, because one of the reasons — a consumer that
// ended while it was copied — is answered by taking the snapshot again, and a
// caller has to be able to tell that one from the rest without matching the
// whole message.
type abandonedSnapshot struct {
	Status      string
	Description string
}

func (a *abandonedSnapshot) Error() string {
	description := a.Description
	if description == "" {
		description = "no reason given"
	}
	return fmt.Sprintf("the broker abandoned the snapshot: %s %s", a.Status, description)
}

// The two sentences the server ends a snapshot with when it cannot read a
// consumer's state (nats-server server/stream_backup.go, streamSnapshotV2):
// a stream on one server reads each consumer's own store, and a replicated
// stream asks each consumer's leader. Both reach the terminator as a 500.
// TestAConsumerEndingUnderASnapshotIsTakenAgain holds the first to the
// server this build links, by making it lose the race for real.
const (
	consumerStatePrefix = "couldn't load consumer '"
	consumerStateSuffix = "' state: "
	clusterStatePrefix  = "failed to get consumer state for '"
)

// consumer is the consumer of stream the server names as the one whose state
// it could not read, or "" when the abandonment names none.
//
// THE SERVER'S OWN SENTENCES, matched whole at both ends rather than searched
// for a quoted word: a consumer's name may hold a quote, and a description
// that matches neither form is some other failure, which is never taken again.
func (a *abandonedSnapshot) consumer(stream string) string {
	if a.Status != "500" {
		return ""
	}
	if rest, ok := strings.CutPrefix(a.Description, consumerStatePrefix); ok {
		if end := strings.LastIndex(rest, consumerStateSuffix); end > 0 {
			return rest[:end]
		}
		return ""
	}
	if rest, ok := strings.CutPrefix(a.Description, clusterStatePrefix+stream+" > "); ok {
		if name, ok := strings.CutSuffix(rest, "'"); ok {
			return name
		}
	}
	return ""
}

// snapshotSize reports what a set of artifacts occupies.
func snapshotSize(artifacts []StreamArtifact) int64 {
	var total int64
	for _, a := range artifacts {
		total += a.Bytes
	}
	return total
}
