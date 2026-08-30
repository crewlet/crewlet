package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

// snapshotStreams captures every stream the broker holds under root.
//
// EVERY STREAM, ENUMERATED RATHER THAN LISTED. The engine's own five are
// known here, but a namespace stream is created on first publish to it
// (CREWLET_NS_*) and a coordination bucket is a stream whose name depends on
// a configurable prefix — so a hardcoded list would silently omit whatever a
// deployment actually has. What is backed up is what is there.
//
// The snapshots land in a subdirectory of root, and each artifact's File is
// relative to root rather than to that subdirectory, because that is the path
// a restore reading the manifest has to join.
func snapshotStreams(ctx context.Context, nc *nats.Conn, root string) ([]StreamArtifact, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("backup: reach the JetStream API: %w", err)
	}
	names, err := streamNames(ctx, js)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, streamDirName), 0o700); err != nil {
		return nil, fmt.Errorf("backup: create %s: %w", filepath.Join(root, streamDirName), err)
	}

	artifacts := make([]StreamArtifact, 0, len(names))
	for _, name := range names {
		artifact, err := snapshotStream(ctx, nc, name, root)
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
// deployment: the listing's own order is not stable, and an operator
// comparing yesterday's manifest with today's should see what changed rather
// than a reshuffle.
func streamNames(ctx context.Context, js jetstream.JetStream) ([]string, error) {
	lister := js.StreamNames(ctx)
	var names []string
	for name := range lister.Name() {
		names = append(names, name)
	}
	if err := lister.Err(); err != nil {
		return nil, fmt.Errorf("backup: list streams: %w", err)
	}
	sort.Strings(names)
	return names, nil
}

// snapshotStream writes one stream's snapshot under root.
func snapshotStream(ctx context.Context, nc *nats.Conn, name, root string) (StreamArtifact, error) {
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

	askCtx, cancel := context.WithTimeout(ctx, snapshotRequestTimeout)
	defer cancel()
	reply, err := nc.RequestWithContext(askCtx,
		fmt.Sprintf(server.JSApiStreamSnapshotT, name), request)
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
	description := msg.Header.Get("Description")
	if description == "" {
		description = "no reason given"
	}
	return fmt.Errorf("the broker abandoned the snapshot: %s %s", status, description)
}

// snapshotSize reports what a set of artifacts occupies.
func snapshotSize(artifacts []StreamArtifact) int64 {
	var total int64
	for _, a := range artifacts {
		total += a.Bytes
	}
	return total
}
