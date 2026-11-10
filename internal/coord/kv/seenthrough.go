package kv

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

// ---- the seats' seen-through marks ------------------------------------- //

// SeenThrough is the mark scope holds for conversation and its revision.
func (f *FleetStore) SeenThrough(ctx context.Context, scope, conversation string) (string, uint64, error) {
	if scope == "" || conversation == "" {
		return "", 0, errors.New("coord/kv: a seen-through mark names its seat and its conversation")
	}
	entry, err := f.get(ctx, f.ledger, seenKey(scope, conversation))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return "", 0, nil
	case err != nil:
		return "", 0, unavailable("read a seen-through mark", err)
	}
	return string(entry.Value()), entry.Revision(), nil
}

// MarkSeenThrough writes mark over the one read at revision, false where it moved.
func (f *FleetStore) MarkSeenThrough(ctx context.Context, scope, conversation, mark string,
	revision uint64) (bool, error) {

	if scope == "" || conversation == "" || mark == "" {
		return false, errors.New("coord/kv: a seen-through mark names its seat, its conversation and its position")
	}
	key := seenKey(scope, conversation)
	var err error
	if revision == 0 {
		_, err = f.create(ctx, f.ledger, key, []byte(mark))
	} else {
		_, err = f.ledger.Update(ctx, key, []byte(mark), revision)
	}
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyExists) || lostUpdateRace(err):
		return false, nil
	}
	return false, unavailable("write a seen-through mark", err)
}

// seenKey is a mark's ledger key: apart from every completion key, which is a
// derived work key and never starts with this prefix.
func seenKey(scope, conversation string) string {
	return encodeKey("seen|" + scope + "|" + conversation)
}
