package pulsar

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateRefusesAConfigThatCannotWork(t *testing.T) {
	t.Parallel()
	base := func() Config {
		return Config{URL: "pulsar://broker:6650", Tenant: "acme", Namespace: "prod"}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		mustSay string // empty means the config is valid
	}{
		{name: "a complete config", mutate: func(*Config) {}},
		{name: "tls broker", mutate: func(c *Config) { c.URL = "pulsar+ssl://broker:6651" }},
		{name: "no url", mutate: func(c *Config) { c.URL = "" }, mustSay: "url is required"},
		{name: "an http url", mutate: func(c *Config) { c.URL = "http://broker:8080" }, mustSay: "pulsar://"},
		{name: "a url with no host", mutate: func(c *Config) { c.URL = "pulsar://" }, mustSay: "names no host"},

		// The reason this backend exists. A company that never named a
		// tenant would share one namespace with every other company on
		// the estate — same topics, same subscription names, same seat
		// inboxes — and nothing would report an error.
		{name: "no tenant", mutate: func(c *Config) { c.Tenant = "" }, mustSay: "tenant is required"},
		{name: "no namespace", mutate: func(c *Config) { c.Namespace = "" }, mustSay: "namespace is required"},
		{name: "a tenant with a slash", mutate: func(c *Config) { c.Tenant = "acme/prod" }, mustSay: "tenant"},
		{name: "a namespace starting with a dash", mutate: func(c *Config) { c.Namespace = "-prod" }, mustSay: "namespace"},

		{name: "an admin url on the wrong scheme", mutate: func(c *Config) { c.AdminURL = "pulsar://broker:8080" }, mustSay: "admin_url"},
		{name: "a good admin url", mutate: func(c *Config) { c.AdminURL = "https://broker:8443" }},
		{name: "a negative budget", mutate: func(c *Config) { c.MaxDeliveries = -1 }, mustSay: "max_deliveries"},
		{name: "a negative wait", mutate: func(c *Config) { c.ReceiveWait = -time.Second }, mustSay: "receive_wait"},
	} {
		cfg := base()
		tc.mutate(&cfg)
		err := cfg.Validate()
		switch {
		case tc.mustSay == "" && err != nil:
			t.Errorf("%s: Validate() = %v, want nil", tc.name, err)
		case tc.mustSay == "":
		case err == nil:
			t.Errorf("%s: Validate() accepted it", tc.name)
		case !errors.Is(err, ErrConfig):
			t.Errorf("%s: Validate() = %v, want an ErrConfig", tc.name, err)
		case !strings.Contains(err.Error(), tc.mustSay):
			t.Errorf("%s: Validate() = %v, want it to mention %q", tc.name, err, tc.mustSay)
		}
	}
}

// TestValidateReportsEveryProblemAtOnce: an operator fixing a broker URL only
// to be told about the tenant on the next boot has been made to pay twice for
// one edit.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	err := Config{}.Validate()
	if err == nil {
		t.Fatal("an empty config validated")
	}
	for _, want := range []string{"url is required", "tenant is required", "namespace is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %v, want it to mention %q", err, want)
		}
	}
}

func TestDeriveAdminURLUsesPulsarsOwnDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, broker, want string }{
		{"plaintext", "pulsar://broker:6650", "http://broker:8080"},
		{"tls", "pulsar+ssl://broker:6651", "https://broker:8443"},
		{"no port on the broker url", "pulsar://broker", "http://broker:8080"},
		// The brackets have to survive or the admin port reads as another
		// group of the address.
		{"an ipv6 literal", "pulsar://[2001:db8::1]:6650", "http://[2001:db8::1]:8080"},
	} {
		got, err := DeriveAdminURL(tc.broker)
		if err != nil {
			t.Errorf("%s: DeriveAdminURL(%q): %v", tc.name, tc.broker, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: DeriveAdminURL(%q) = %q, want %q", tc.name, tc.broker, got, tc.want)
		}
	}
}

// TestDeriveAdminURLRefusesToGuess: guessing an endpoint that answers someone
// else's API is worse than saying so — the engine would report the
// subscription-existence invariant established against a service that never
// heard of it.
func TestDeriveAdminURLRefusesToGuess(t *testing.T) {
	t.Parallel()
	for _, broker := range []string{"", "http://broker:8080", "broker:6650", "pulsar://"} {
		if _, err := DeriveAdminURL(broker); !errors.Is(err, ErrConfig) {
			t.Errorf("DeriveAdminURL(%q) = %v, want an ErrConfig", broker, err)
		}
	}
}

func TestResolvedKnobsFallBackToTheDerivedDefaults(t *testing.T) {
	t.Parallel()
	var zero Config
	if got := zero.deliveryBudget(); got != maxDeliveries {
		t.Errorf("deliveryBudget() = %d, want %d", got, maxDeliveries)
	}
	if got := zero.nakDelay(); got != nakRedeliveryDelay {
		t.Errorf("nakDelay() = %v, want %v", got, nakRedeliveryDelay)
	}
	if got := zero.receiveWait(); got != receiveWait {
		t.Errorf("receiveWait() = %v, want %v", got, receiveWait)
	}
	if got := zero.discoveryPeriod(); got != autoDiscoveryPeriod {
		t.Errorf("discoveryPeriod() = %v, want %v", got, autoDiscoveryPeriod)
	}

	set := Config{MaxDeliveries: 3, NackRedeliveryDelay: time.Millisecond, ReceiveWait: 2 * time.Millisecond, AutoDiscoveryPeriod: 3 * time.Millisecond}
	if got := set.deliveryBudget(); got != 3 {
		t.Errorf("deliveryBudget() = %d, want 3", got)
	}
	if got := set.nakDelay(); got != time.Millisecond {
		t.Errorf("nakDelay() = %v, want 1ms", got)
	}
}

// TestPrefetchCoversTheBatchCap pins the rule an operator trips over: a batch
// subscriber needs at least one full batch available LOCALLY for the
// zero-linger drain to coalesce anything, so raising max_batch past the
// prefetch would silently start delivering partial batches — the coalescing
// quietly doing less than it says.
func TestPrefetchCoversTheBatchCap(t *testing.T) {
	t.Parallel()
	var cfg Config
	for _, tc := range []struct {
		maxBatch, want int
	}{
		{0, receiverQueueSize},
		{20, receiverQueueSize}, // the default batch cap fits three times over
		{64, 128},               // a raised cap pulls the prefetch up with it
		{1000, 2000},
	} {
		if got := cfg.prefetchFor(tc.maxBatch); got != tc.want {
			t.Errorf("prefetchFor(%d) = %d, want %d", tc.maxBatch, got, tc.want)
		}
	}
	// An operator's explicit floor is respected, and still raised to cover
	// the batch.
	if got := (Config{ReceiverQueueSize: 8}).prefetchFor(0); got != 8 {
		t.Errorf("prefetchFor(0) with an explicit size = %d, want 8", got)
	}
	if got := (Config{ReceiverQueueSize: 8}).prefetchFor(20); got != 40 {
		t.Errorf("prefetchFor(20) with an explicit size = %d, want 40", got)
	}
}
