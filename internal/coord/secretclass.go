package coord

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The secrets bucket's one key class (ADR-0175, "The secrets bucket declares
// its class"): a credential's record under its name, which writers read and
// decide on (a put keeping a write-only mark, a create's lost round, a
// rekey's re-read, a grant's renewal), so its read is given its reply's
// crossing at [MaxSecretRecordBytes]. Every field the record keeps is
// bounded where it is written: the value is sealed from at most 64 KiB
// (internal/secrets' MaxValueBytes) under a key id of at most 255 bytes, the
// name is at most 255 bytes (internal/secrets' MaxNameBytes), and the
// provenance is a login's 64 bytes and an operation id's 128. Both backends
// store the record through [EncodeSecret], so the twin refuses what the KV
// backend does.

// MaxSecretRecordBytes bounds one credential's record as stored
// ([EncodeSecret]). Its widest holds a 64 KiB value sealed under a 255-byte
// key id (an envelope of 87,683 bytes) beside that key id, a 255-byte name,
// every provenance field at its widest and the instant to the nanosecond:
// 88,651 bytes (internal/fleetsecrets.TestTheWidestSecretRecordFitsItsBound),
// 9.7 KB under the bound, room for a field a later build widens. At the
// slowest link pace its read waits 3.8 s beyond the read term, and a pass
// 2.85 s beyond its idle; the one boot read, the secret snapshot, runs after
// `engine_recovered`, which no probe bounds.
const MaxSecretRecordBytes = 96 << 10

// secretWire is a credential's record as a backend stores it. The VALUE IS
// ALREADY AN ENVELOPE, sealed by the Tier A keyring on each node's disk, so
// no store can open it; KeyID is all a rotation sweep reads. The version is
// not in it: it is the store's own revision, handed back beside the row
// ([SecretRecord.Version]), so there is one answer.
type secretWire struct {
	Name          string    `json:"name"`
	Value         string    `json:"value"`
	KeyID         string    `json:"key_id"`
	UpdatedAt     time.Time `json:"updated_at"`
	UpdatedBy     string    `json:"updated_by,omitempty"`
	UpdatedByKind string    `json:"updated_by_kind,omitempty"`
	OperatorID    string    `json:"operator_id,omitempty"`
	Source        string    `json:"source,omitempty"`
	OpID          string    `json:"op_id,omitempty"`
	WriteOnly     bool      `json:"write_only,omitempty"`
}

// EncodeSecret is a credential's record as a backend stores it, refusing a
// row no store should hold: one with no name, one with no sealed value
// (stored, it would resolve as an empty ${VAR} on every node), and one past
// [MaxSecretRecordBytes] as [ErrValueTooLarge]. ONE CHECK FOR EVERY WRITE,
// conditional or not, on both backends.
func EncodeSecret(rec SecretRecord) ([]byte, error) {
	switch {
	case rec.Name == "":
		return nil, errors.New("coord: a secret needs a name")
	case rec.Value == "":
		return nil, fmt.Errorf("coord: secret %q has no sealed value", rec.Name)
	}
	raw, err := json.Marshal(secretWire{
		Name: rec.Name, Value: rec.Value, KeyID: rec.KeyID,
		UpdatedAt: rec.UpdatedAt.UTC(), UpdatedBy: rec.UpdatedBy,
		UpdatedByKind: rec.UpdatedByKind, OperatorID: rec.OperatorID,
		Source: rec.Source, OpID: rec.OpID, WriteOnly: rec.WriteOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("coord: encode the secret %q: %w", rec.Name, err)
	}
	if err := HoldValue("a secret's record", len(raw), MaxSecretRecordBytes); err != nil {
		return nil, err
	}
	return raw, nil
}

// DecodeSecret reads one record [EncodeSecret] stored, reporting false for
// bytes that are not one. The version is the caller's to set from the store.
func DecodeSecret(raw []byte) (SecretRecord, bool) {
	var rec secretWire
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Name == "" {
		return SecretRecord{}, false
	}
	return SecretRecord{
		Name: rec.Name, Value: rec.Value, KeyID: rec.KeyID,
		UpdatedAt: rec.UpdatedAt, UpdatedBy: rec.UpdatedBy,
		UpdatedByKind: rec.UpdatedByKind, OperatorID: rec.OperatorID,
		Source: rec.Source, OpID: rec.OpID, WriteOnly: rec.WriteOnly,
	}, true
}
