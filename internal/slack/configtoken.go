package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/provision"
)

// Keeping the app-configuration token alive, wherever it is kept.
//
// # Why one place decides when to rotate
//
// Slack's app-configuration access token lives twelve hours, and the refresh
// token that renews it is SINGLE USE: each rotation answers a new pair and
// retires the refresh token it was given. So the rules are the same whoever
// holds the pair (the CLI's ledger file, or the company's sealed secret the
// reconcile loop reads, D11): reuse a token that is still good, rotate only
// when it is not, and make the new pair durable before using it. Written
// twice, one holder would eventually rotate needlessly or use before saving,
// and either locks the company out of its own apps.

// ConfigTokenStore keeps the app-configuration token pair between runs.
type ConfigTokenStore interface {
	// LoadConfigToken answers the held pair, or the zero pair when none
	// is held. An error means the store could not say, which is not the
	// same as holding nothing.
	LoadConfigToken(ctx context.Context) (ConfigToken, error)

	// SaveConfigToken makes a freshly rotated pair durable. It returns
	// only once the pair would survive a crash.
	SaveConfigToken(ctx context.Context, token ConfigToken) error
}

// ErrNoConfigToken reports that no app-configuration token is held and none
// was supplied. It is a step for a person, not a fault: the token is created
// at Slack by a workspace member, once.
var ErrNoConfigToken = errors.New(
	"slack: no app-configuration token: create one at " +
		"https://api.slack.com/reference/manifests#config-tokens and " +
		"supply its refresh token, which is exchanged for a " +
		"12-hour access token")

// configTokenMargin is how much life a held token must have left to be used.
// A run that started with seconds left would fail half way through.
const configTokenMargin = time.Minute

// Usable reports a held access token with enough life left to use at at.
// A token with no recorded expiry is used: Slack answered none, and a refusal
// is the signal to rotate ([Admin.ConfigToken] with force).
func (t ConfigToken) Usable(at time.Time) bool {
	return strings.TrimSpace(t.Token) != "" &&
		(t.ExpiresAt.IsZero() || t.ExpiresAt.After(at.Add(configTokenMargin)))
}

// ConfigToken answers an access token to manage apps with, rotating and
// saving a fresh pair when the held one is not usable.
//
// bootstrap is a refresh token a person supplied (a flag, a variable, the
// connect form). THE STORE'S PAIR BEATS IT once the store holds one: every
// rotation retires the refresh token it was given, so the supplied value is
// dead the moment it is first used, and preferring it would trade the live
// pair for one Slack has already retired.
//
// force rotates even a token that looks usable, for the caller that has just
// had it refused (revoked at Slack, or an expiry Slack did not report).
func (a *Admin) ConfigToken(
	ctx context.Context, store ConfigTokenStore, bootstrap string, at time.Time, force bool,
) (string, error) {
	held, err := store.LoadConfigToken(ctx)
	if err != nil {
		return "", fmt.Errorf("slack: read the app-configuration token: %w", err)
	}
	if !force && held.Usable(at) {
		return held.Token, nil
	}
	refresh := strings.TrimSpace(held.RefreshToken)
	if refresh == "" {
		refresh = strings.TrimSpace(bootstrap)
	}
	if refresh == "" {
		return "", ErrNoConfigToken
	}
	rotated, err := a.RotateConfigToken(ctx, refresh)
	if err != nil {
		return "", err
	}
	// PERSISTED BEFORE USE. The rotate already retired the refresh token
	// it was given, so this pair is now the only way back into the
	// company's apps.
	if err := store.SaveConfigToken(ctx, rotated); err != nil {
		return "", fmt.Errorf(
			"slack: the app-configuration token was rotated and could not be "+
				"recorded, which invalidates the refresh token it replaced: %w", err)
	}
	return rotated.Token, nil
}

// EncodeConfigToken is the pair as ONE sealed value.
//
// ONE VALUE, NOT TWO SECRETS, because a rotation changes both halves and the
// old refresh token is dead the moment Slack answers: two separate writes
// could land one and fail the other, leaving a new access token beside a
// retired refresh token, which locks the company out twelve hours later with
// nothing to say why.
func EncodeConfigToken(token ConfigToken) (string, error) {
	encoded, err := json.Marshal(sealedConfigToken(token))
	if err != nil {
		return "", fmt.Errorf("slack: encode the app-configuration token: %w", err)
	}
	return string(encoded), nil
}

// DecodeConfigToken reads a value [EncodeConfigToken] wrote.
//
// A BARE REFRESH TOKEN IS ACCEPTED TOO, since that is what a person pastes
// into the connect form: Slack's console shows the refresh token beside the
// access token, and the pair's first rotation turns it into a sealed value.
// The error never quotes the value, which is a credential.
func DecodeConfigToken(raw string) (ConfigToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ConfigToken{}, nil
	}
	if !strings.HasPrefix(raw, "{") {
		return ConfigToken{RefreshToken: raw}, nil
	}
	var sealed sealedConfigToken
	if err := json.Unmarshal([]byte(raw), &sealed); err != nil {
		return ConfigToken{}, errors.New(
			"slack: the sealed app-configuration token is not in the form this " +
				"engine writes; supply a fresh refresh token from Slack")
	}
	return ConfigToken(sealed), nil
}

// sealedConfigToken is the sealed value's shape.
type sealedConfigToken struct {
	Token        string    `json:"token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
}

// SinkConfigTokenStore keeps the pair as one company secret in a
// [provision.TokenSink], which is how the reconcile loop holds it (D11).
type SinkConfigTokenStore struct {
	Sink provision.TokenSink

	// Var is the company variable the pair is sealed under
	// ([ConfigTokenVar]).
	Var string
}

// LoadConfigToken implements [ConfigTokenStore].
func (s SinkConfigTokenStore) LoadConfigToken(ctx context.Context) (ConfigToken, error) {
	raw, ok, err := s.Sink.Value(ctx, s.Var)
	if err != nil || !ok {
		return ConfigToken{}, err
	}
	return DecodeConfigToken(raw)
}

// SaveConfigToken implements [ConfigTokenStore].
func (s SinkConfigTokenStore) SaveConfigToken(ctx context.Context, token ConfigToken) error {
	encoded, err := EncodeConfigToken(token)
	if err != nil {
		return err
	}
	return s.Sink.Record(ctx, s.Var, encoded)
}

// ledgerConfigTokenStore keeps the pair in the CLI's ledger file.
type ledgerConfigTokenStore struct {
	ledger *Ledger
	path   string
}

func (s ledgerConfigTokenStore) LoadConfigToken(context.Context) (ConfigToken, error) {
	held := s.ledger.ConfigToken
	return ConfigToken{
		Token: held.Token, RefreshToken: held.RefreshToken, ExpiresAt: held.ExpiresAt,
	}, nil
}

func (s ledgerConfigTokenStore) SaveConfigToken(_ context.Context, token ConfigToken) error {
	s.ledger.ConfigToken.Token = token.Token
	s.ledger.ConfigToken.RefreshToken = token.RefreshToken
	s.ledger.ConfigToken.ExpiresAt = token.ExpiresAt
	return s.ledger.Save(s.path)
}
