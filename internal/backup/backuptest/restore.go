// Package backuptest restores a backup's stream snapshots into a broker, for
// the suites that prove a backup restores — the runbook's `nats stream
// restore`, which the engine has no command for (restore is an operator
// procedure, docs/guides/backup.md) and which cannot reach the embedded broker,
// since it listens on no socket.
//
// THE SERVER'S OWN PROTOCOL, in the server's own types, as internal/backup's
// snapshot half speaks its own (streams.go): a restore request naming the
// stream with the manifest's VERBATIM configuration and state, then the
// snapshot file in chunks on the subject the server answers with — each chunk a
// request whose reply is the server's flow-control credit, or its refusal —
// then an empty message that ends it, whose reply is the restored stream's
// information or the server's error.
//
// The configuration goes back VERBATIM, raw JSON and never a decoded struct,
// for the reason the manifest keeps it raw: a field this build does not know
// survives the round trip. One field of it is load-bearing for everything a
// restore is for. The broker dates a restored stream to the moment of the
// restore, and the instant a node's rows are keyed to is the one the stream's
// creator stamped into its configuration (statelog.CreatedAtKey) — so a
// configuration handed back changed is a restore every node refuses as a
// recreated log.
//
// # A restored stream's duplicate window is its broker's NEXT START
//
// Every record keeps its headers through a restore, the operation id a state
// log appends under (Nats-Msg-Id) among them, and a stream rebuilds its
// duplicate window from those ids — but nats-server (2.15.0, measured) does so
// when it RECOVERS a stream from its store, not at the restore: a stream just
// restored collapses no repeated operation until the broker holding it starts
// again. So a suite that relies on the window restarts the broker on what it
// restored before relying on it, as a node does — the runbook restores the
// embedded broker on a node started for that purpose and then starts the
// fleet. TestARestoredStreamHoldsWhatWasSnapshotted pins both halves, so this
// paragraph fails the day the server rebuilds the window at the restore.
//
// Test support: nothing in production imports it.
package backuptest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/jsapi"
)

// restoreChunkSize is how much of a snapshot one chunk carries.
//
// 128 KiB, the snapshot side's own (internal/backup's snapshotChunkSize) and
// the server's default: the transfer is between two halves of one process,
// where a larger chunk only buys a larger buffer.
const restoreChunkSize = 128 * 1024

// restoreChunkWait bounds each chunk's round trip — the server writing it into
// the restore and crediting it — and the final message's, which the server
// answers once the stream is restored.
//
// Thirty seconds, the snapshot side's own bound on its next chunk
// (snapshotChunkWait): far past a healthy write and still bounded, because the
// alternative to a bound is a suite that hangs on a broker that stopped
// answering mid-restore. The server's own stall watchdog gives up after five
// seconds without a chunk, so a restore that waited longer than that has
// already failed on the server's side.
const restoreChunkWait = 30 * time.Second

// Option shapes a [RestoreStreams].
type Option func(*options)

type options struct {
	skip []string
}

// Skip leaves the named streams out — a runbook step that must not put one
// back: a restore into a new object store leaves out KV_crewlet_objects, the
// record naming the old one (docs/guides/backup.md, "Restoring into a new
// bucket"). A name the manifest does not hold is an error, because a misspelt
// skip restores exactly the stream it was meant to leave out.
func Skip(streams ...string) Option {
	return func(o *options) { o.skip = append(o.skip, streams...) }
}

// RestoreStreams restores every stream the manifest names from dir, through
// conn, in the JetStream API api addresses, in the manifest's order.
//
// A stream that already exists is an error naming it: a restore creates its
// stream, and the server refuses one over a stream of the same name. So is a
// restored stream that does not end at the sequence its snapshot ended at,
// which is the one figure the manifest records that any restore must keep.
//
// A restored stream's duplicate window is empty until conn's broker starts
// again on what it restored (package doc).
func RestoreStreams(ctx context.Context, conn *nats.Conn, api jsapi.API, dir string, m backup.Manifest,
	opts ...Option) error {

	var o options
	for _, opt := range opts {
		opt(&o)
	}
	for _, name := range o.skip {
		if !slices.ContainsFunc(m.Streams, func(a backup.StreamArtifact) bool { return a.Name == name }) {
			return fmt.Errorf("backuptest: Skip names %s, which the manifest holds no snapshot of", name)
		}
	}
	for _, artifact := range m.Streams {
		if slices.Contains(o.skip, artifact.Name) {
			continue
		}
		if err := restoreStream(ctx, conn, api, dir, artifact); err != nil {
			return err
		}
	}
	return nil
}

// restoreRequest is server.JSApiStreamRestoreRequest with its two fields kept
// raw, so the manifest's configuration and state reach the server byte for
// byte. The tags are the server's own, and request_internal_test.go holds the
// two shapes to one another.
type restoreRequest struct {
	Config json.RawMessage `json:"config"`
	State  json.RawMessage `json:"state"`
}

// restoreStream restores one stream from its snapshot.
func restoreStream(ctx context.Context, conn *nats.Conn, api jsapi.API, dir string, a backup.StreamArtifact) error {
	var snapshotted server.StreamState
	if err := json.Unmarshal(a.State, &snapshotted); err != nil {
		return fmt.Errorf("backuptest: read %s's recorded state: %w", a.Name, err)
	}
	file, err := os.Open(filepath.Join(dir, a.File))
	if err != nil {
		return fmt.Errorf("backuptest: open %s's snapshot: %w", a.Name, err)
	}
	defer func() { _ = file.Close() }()

	request, err := json.Marshal(restoreRequest{Config: a.Config, State: a.State})
	if err != nil {
		return fmt.Errorf("backuptest: encode the restore of %s: %w", a.Name, err)
	}
	// IN THE API THE CLIENT SPEAKS, as the snapshot was asked for: the
	// library has no restore call, and a raw subject formed against the
	// account's own API is answered by nothing on a broker serving its
	// JetStream under a domain.
	subject, err := api.Subject(fmt.Sprintf(server.JSApiStreamRestoreT, a.Name))
	if err != nil {
		return fmt.Errorf("backuptest: address the restore of %s: %w", a.Name, err)
	}
	reply, err := ask(ctx, conn, subject, request)
	if err != nil {
		return fmt.Errorf("backuptest: ask to restore %s: %w", a.Name, err)
	}
	var started server.JSApiStreamRestoreResponse
	if decodeErr := json.Unmarshal(reply, &started); decodeErr != nil {
		return fmt.Errorf("backuptest: read the answer to restoring %s: %w", a.Name, decodeErr)
	}
	if started.Error != nil {
		return fmt.Errorf("backuptest: the broker refused to restore %s: %s", a.Name, started.Error.Description)
	}

	buf := make([]byte, restoreChunkSize)
	for {
		n, readErr := io.ReadFull(file, buf)
		if n > 0 {
			credit, sendErr := ask(ctx, conn, started.DeliverSubject, buf[:n])
			if sendErr != nil {
				return fmt.Errorf("backuptest: send %s's snapshot: %w", a.Name, sendErr)
			}
			// AN EMPTY REPLY IS THE CREDIT; one with a body is the
			// server's refusal of the chunk, after which it reads no
			// more of the restore.
			if len(credit) > 0 {
				if _, refusal := restored(credit, a.Name); refusal != nil {
					return refusal
				}
				return fmt.Errorf("backuptest: the broker answered %s's chunk with a stream "+
					"before the restore was ended", a.Name)
			}
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("backuptest: read %s's snapshot: %w", a.Name, readErr)
		}
	}
	// THE EMPTY MESSAGE ENDS IT, and its reply is the verdict.
	end, err := ask(ctx, conn, started.DeliverSubject, nil)
	if err != nil {
		return fmt.Errorf("backuptest: end the restore of %s: %w", a.Name, err)
	}
	info, err := restored(end, a.Name)
	if err != nil {
		return err
	}
	if info.State.LastSeq != snapshotted.LastSeq {
		return fmt.Errorf("backuptest: %s was restored ending at sequence %d and its snapshot "+
			"ended at %d", a.Name, info.State.LastSeq, snapshotted.LastSeq)
	}
	return nil
}

// restored reads the server's answer to the end of a restore: the restored
// stream's information, or its refusal.
func restored(reply []byte, name string) (*server.StreamInfo, error) {
	var answer server.JSApiStreamCreateResponse
	if err := json.Unmarshal(reply, &answer); err != nil {
		return nil, fmt.Errorf("backuptest: read the end of restoring %s: %w", name, err)
	}
	if answer.Error != nil {
		return nil, fmt.Errorf("backuptest: the broker could not restore %s: %s", name, answer.Error.Description)
	}
	if answer.StreamInfo == nil {
		return nil, fmt.Errorf("backuptest: the broker ended the restore of %s naming no stream", name)
	}
	return answer.StreamInfo, nil
}

// ask sends one request and waits, bounded, for its reply.
func ask(ctx context.Context, conn *nats.Conn, subject string, data []byte) ([]byte, error) {
	waitCtx, cancel := context.WithTimeout(ctx, restoreChunkWait)
	defer cancel()
	reply, err := conn.RequestWithContext(waitCtx, subject, data)
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}
