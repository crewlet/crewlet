package accessdomain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TYPED PAYLOADS, one per (kind, op). A record carries its complete values,
// decided by the writer, so a replay from zero writes the rows the first apply
// did. Ids travel as text: an id a decoder refused would stall the log.

// Told is what one turn of a seat was told about its access: the snapshot it
// ran under, and the changes since the seat's previous told record, or none on
// a baseline. Snapshot and Changes are the capability package's JSON, opaque here.
type Told struct {
	V int `json:"v"`

	// Handle is the seat's handle and Turn the turn's id: the record's key.
	Handle string `json:"handle"`
	Turn   string `json:"turn"`

	// Digest is the snapshot's own digest, `sha256:` and 64 hex digits.
	Digest   string          `json:"digest"`
	Snapshot json.RawMessage `json:"snapshot"`
	Changes  json.RawMessage `json:"changes,omitempty"`

	// Baseline is a seat's first told record, which tells no change.
	Baseline bool `json:"baseline,omitempty"`

	// RevisionFrom is the company revision the previous told record ran under,
	// RevisionTo the one this turn runs under, and Authors who wrote the
	// revisions between them.
	RevisionFrom string   `json:"revision_from,omitempty"`
	RevisionTo   string   `json:"revision_to,omitempty"`
	Authors      []string `json:"authors,omitempty"`

	// SeenRemoval is the position of the seat's last removal the decide saw,
	// zero for none: one decided before a removal it did not see writes nothing.
	SeenRemoval int64 `json:"seen_removal,omitempty"`

	// ReadThrough is the position the turn's dispatch read the seat's baseline
	// at: a record that lands after a newer told record than that read is kept
	// but is not the baseline.
	ReadThrough int64 `json:"read_through,omitempty"`
}

// CertificatePurpose is what an SSH certificate was signed for.
type CertificatePurpose string

// The purposes a certificate is signed for.
const (
	// PurposeRead is a seat's engine-side read certificate for one turn.
	PurposeRead CertificatePurpose = "read"
	// PurposeBoxRead is a seat's read certificate written into its box.
	PurposeBoxRead CertificatePurpose = "box_read"
	// PurposeOperate is one approved command's certificate.
	PurposeOperate CertificatePurpose = "operate"
	// PurposeTest is an administrator's Test connection.
	PurposeTest CertificatePurpose = "test"
)

// CertificatePurposes are the four.
var CertificatePurposes = []CertificatePurpose{PurposeRead, PurposeBoxRead, PurposeOperate, PurposeTest}

// Valid reports whether a purpose off the wire is one this build knows.
func (p CertificatePurpose) Valid() bool { return slices.Contains(CertificatePurposes, p) }

// Certificate is one SSH certificate the engine signed. Only its public facts:
// the private key it certifies never leaves the engine's memory.
type Certificate struct {
	V int `json:"v"`

	// Serial is the certificate's serial, KeyID its key id.
	Serial uint64 `json:"serial"`
	KeyID  string `json:"key_id"`

	// Handle is the seat it was signed for: an agent seat, or the human seat
	// of the person who pressed Test connection. Turn and HeldCall name what
	// it served, empty where nothing did.
	Handle   string             `json:"handle"`
	Turn     string             `json:"turn,omitempty"`
	HeldCall string             `json:"held_call,omitempty"`
	Purpose  CertificatePurpose `json:"purpose"`

	// HostIDs are the registered hosts and Principals the principals it names.
	HostIDs    []string `json:"host_ids"`
	Principals []string `json:"principals"`

	ValidAfter  time.Time `json:"valid_after"`
	ValidBefore time.Time `json:"valid_before"`

	// PublicKeySHA256 is the certified key's fingerprint, CASHA256 the signing
	// authority's, each `SHA256:` and unpadded base64 as ssh-keygen prints it.
	PublicKeySHA256 string `json:"public_key_sha256"`
	CASHA256        string `json:"ca_sha256"`

	// Node is the engine node that signed it.
	Node string `json:"node"`
}

// CommandLevel is the level a command ran at.
type CommandLevel string

// The two levels.
const (
	LevelRead    CommandLevel = "read"
	LevelOperate CommandLevel = "operate"
)

// CommandLevels are the two, least first.
var CommandLevels = []CommandLevel{LevelRead, LevelOperate}

// Valid reports whether a level off the wire is one this build knows.
func (l CommandLevel) Valid() bool { return slices.Contains(CommandLevels, l) }

// CommandDecision is a person's answer to an operate command; a read has none.
type CommandDecision string

// The decisions.
const (
	DecisionNone     CommandDecision = ""
	DecisionApproved CommandDecision = "approved"
	DecisionDenied   CommandDecision = "denied"
)

// Valid reports whether a decision off the wire is one this build knows.
func (d CommandDecision) Valid() bool {
	return d == DecisionNone || d == DecisionApproved || d == DecisionDenied
}

// CommandFailure is why a command did not run or did not finish, in a closed
// set of words: never free text, so neither the command nor its output can
// reach a row in the clear through it.
type CommandFailure string

// The failures a command records.
const (
	FailureNone CommandFailure = ""
	// FailureUnreachable is a server the engine could not reach.
	FailureUnreachable CommandFailure = "unreachable"
	// FailureHostKeyRefused is a server that presented no pinned host key.
	FailureHostKeyRefused CommandFailure = "host_key_refused"
	// FailureAuthenticationRefused is a server that refused the certificate.
	FailureAuthenticationRefused CommandFailure = "authentication_refused"
	// FailureSessionFailed is a session that broke after it was opened.
	FailureSessionFailed CommandFailure = "session_failed"
	// FailureTimedOut is a command killed at the integration's timeout.
	FailureTimedOut CommandFailure = "timed_out"
	// FailureEngineError is the engine's own failure before it connected.
	FailureEngineError CommandFailure = "engine_error"
)

// CommandFailures are every failure but none.
var CommandFailures = []CommandFailure{
	FailureUnreachable, FailureHostKeyRefused, FailureAuthenticationRefused,
	FailureSessionFailed, FailureTimedOut, FailureEngineError,
}

// Valid reports whether a failure off the wire is one this build knows.
func (f CommandFailure) Valid() bool { return f == FailureNone || slices.Contains(CommandFailures, f) }

// Command is one SSH command: run by the engine, or denied by a person. The
// command text is sealed by the writer and never opened by the applier; stdin
// and output are kept as their sizes and SHA-256 digests only.
type Command struct {
	V int `json:"v"`

	// ID is the command's own id, Handle the seat that asked for it, and Turn
	// and HeldCall what it served.
	ID       string `json:"id"`
	Handle   string `json:"handle"`
	Turn     string `json:"turn,omitempty"`
	HeldCall string `json:"held_call,omitempty"`

	Host  string       `json:"host"`
	User  string       `json:"user"`
	Level CommandLevel `json:"level"`

	// CommandSealed is the command text sealed under the fleet keyring
	// ([AAD]), and ContentKey the keyring key it is sealed under.
	CommandSealed string `json:"command_sealed"`
	ContentKey    string `json:"content_key"`

	StdinBytes  int64  `json:"stdin_bytes,omitempty"`
	StdinSHA256 string `json:"stdin_sha256,omitempty"`

	// Ran is false for a command that never started: denied, or refused or
	// failed before the server ran it ([Command.Failure] says why).
	Ran          bool   `json:"ran,omitempty"`
	ExitCode     int    `json:"exit_code,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	StdoutBytes  int64  `json:"stdout_bytes,omitempty"`
	StderrBytes  int64  `json:"stderr_bytes,omitempty"`
	OutputSHA256 string `json:"output_sha256,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`

	// Approver is who answered an operate command and Decision what they
	// answered; both empty for a read.
	Approver string          `json:"approver,omitempty"`
	Decision CommandDecision `json:"decision,omitempty"`

	// CertificateSerial is the certificate the command ran under, zero for
	// one that never connected.
	CertificateSerial uint64 `json:"certificate_serial,omitempty"`

	// Failure is why the command did not run or did not finish, empty for one
	// that ran to its exit: a closed word, never the command, its output or
	// an error's text, which a log carries redacted instead.
	Failure CommandFailure `json:"failure,omitempty"`
}

// Forget removes what a seat the company removed was told.
type Forget struct {
	V      int    `json:"v"`
	Handle string `json:"handle"`
}

// Expire removes every record broker-stamped before Through, but each seat's
// newest told record. Through is never later than the retention's cutoff.
type Expire struct {
	V       int       `json:"v"`
	Through time.Time `json:"through"`
}

// Eviction is a node's eviction from this log, or its readmission: pinned at
// [GateRecordVersion] for ever.
type Eviction struct {
	V      int    `json:"v"`
	NodeID string `json:"node_id"`

	EvictedBy string    `json:"evicted_by,omitempty"`
	EvictedAt time.Time `json:"evicted_at"`

	// Readmitted makes this the inverse commit rather than a delete.
	Readmitted bool `json:"readmitted,omitempty"`
}

// Generation is a reanchor's record.
type Generation struct {
	V           int       `json:"v"`
	Generation  uint32    `json:"generation"`
	By          string    `json:"by,omitempty"`
	PrevHighest uint64    `json:"prev_highest,omitempty"`
	StreamAt    time.Time `json:"stream_created_at"`
}

// DecodeMutation reads the typed payload for one record. The dispatch is on
// (kind, op) alone, so an unknown pair is an error naming both.
func DecodeMutation(rec MutationRecord) (any, error) {
	k, op := rec.Subject.Kind, rec.Op
	switch {
	case k == KindTold && op == OpTold:
		return decodePayload[Told](rec)
	case k == KindCertificate && op == OpIssued:
		return decodePayload[Certificate](rec)
	case k == KindCommand && op == OpRan:
		return decodePayload[Command](rec)
	case k == KindSeat && op == OpForget:
		return decodePayload[Forget](rec)
	case k == KindExpiry && op == OpExpire:
		return decodePayload[Expire](rec)
	case k == KindEviction && op == OpEviction:
		return decodePayload[Eviction](rec)
	case k == KindGeneration && op == OpGeneration:
		return decodePayload[Generation](rec)
	case k == KindBarrier && op == OpBarrier, k == KindTrimmed && op == OpTrimmed:
		// The two payload-free records: nil is their value.
		return nil, nil
	}
	return nil, fmt.Errorf("accessdomain: no payload shape for (%s, %s): a pair this build "+
		"does not know is a newer peer's record", k, op)
}

// decodePayload reads one typed payload.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("accessdomain: the record on %s carries no payload, and "+
			"(%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("accessdomain: decode the %s payload on %s: %w", rec.Op, rec.Subject, err)
	}
	return out, nil
}

// EncodeBarrier renders the read index's payload-free append. An op id is
// refused: a repeat would be answered from the dedupe cache with no quorum.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("accessdomain: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("accessdomain: a barrier carries no op id and this one has one")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			Subject: BarrierSubject(), Op: OpBarrier, Scope: ScopeSet{Subject: true}, Gen: env.Gen,
		},
	})
}

// EncodeTrimmed renders the trim's trimmed record (ADR-0080). The writer
// travels, so the eviction gate can drop an evicted node's; no op id.
func EncodeTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind || env.Subject.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("accessdomain: a %q envelope on %q is not a trimmed record",
			env.Kind, env.Subject)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("accessdomain: a trimmed record carries no op id and this one has one")
	}
	floor, err := statelog.TrimmedFloor(Domain{}.Stream().Name, env.Subject)
	if err != nil {
		return nil, fmt.Errorf("accessdomain: %w", err)
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// Pinned at the base version for ever: every build must read it.
			V: baseRecordVersion, Subject: TrimmedSubject(floor), Op: OpTrimmed,
			Scope: ScopeSet{Subject: true}, Gen: env.Gen, Writer: env.Writer,
		},
	})
}
