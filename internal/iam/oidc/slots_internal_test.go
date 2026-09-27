package oidc

import (
	"testing"

	"github.com/crewlet/crewlet/internal/httpx"
)

// A PROVIDER BUILT WITH NO CLIENT IS REACHED OVER ITS OWN CAPPED TRANSPORT.
//
// The engine builds its provider with none, so this default is what every
// discovery, key fetch, exchange and probe in production goes through: the
// shared transport would carry no cap, and a fresh one per provider would be a
// cap per client rather than per party.
func TestAProviderIsReachedOverItsOwnCappedTransport(t *testing.T) {
	t.Parallel()
	p := NewProvider(Config{}, nil, nil)
	if p.client.Transport != httpx.IdentityProviderClient(0).Transport {
		t.Error("a provider built with no client is not on the identity " +
			"provider's capped transport")
	}
	if cap(p.slots) != ExchangeSlots {
		t.Errorf("a provider admits %d requests to its token endpoint, want %d",
			cap(p.slots), ExchangeSlots)
	}
}
