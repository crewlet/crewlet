package pulsar

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config configures a Pulsar-backed queue.
//
// Tenant and Namespace are REQUIRED and deliberately not defaulted. Pulsar's
// built-in public/default pair exists and works, which is exactly the hazard:
// a company that never set a tenant would silently share one namespace with
// every other company on the estate — same topics, same subscription names,
// same seat inboxes — and nothing would report an error, because from the
// broker's point of view they are one deployment. Multi-tenancy is the reason
// this backend exists over embedded JetStream; making the operator name the
// boundary is the cheapest way to make sure it is one.
type Config struct {
	// URL is the broker service URL: pulsar://host:6650 or
	// pulsar+ssl://host:6651.
	URL string

	// Tenant and Namespace scope every topic this queue touches. Neither
	// is ever auto-created — Pulsar auto-creates TOPICS, never tenants or
	// namespaces — so both must exist before the engine starts, created
	// out of band with pulsar-admin.
	Tenant    string
	Namespace string

	// AdminURL is the broker's admin HTTP endpoint, which subscription
	// lifecycle runs over (see admin.go). Empty derives it from URL using
	// Pulsar's own default ports, which is what every standalone and helm
	// deployment ships.
	AdminURL string

	// Token is the JWT presented to both the broker and the admin API —
	// one authorization, two doors. A token scoped only to
	// produce+consume is NOT sufficient: the admin calls need subscription
	// lifecycle permission on the namespace.
	Token string

	// TLSTrustCertsPath is the CA bundle for a pulsar+ssl broker and its
	// https admin endpoint.
	TLSTrustCertsPath string

	// MaxDeliveries overrides the delivery budget: how many times a
	// persistently failing message is handed to a handler before it is
	// routed to the dead-letter topic. Zero uses the derived default.
	// Tests shrink it so a dead-letter assertion costs a handful of
	// handler runs rather than ten.
	MaxDeliveries int

	// NackRedeliveryDelay, ReceiveWait and AutoDiscoveryPeriod override
	// the loop timings. Zero uses the derived defaults; tests shrink them
	// so a suite that exercises redelivery does not spend its life in
	// timers.
	NackRedeliveryDelay time.Duration
	ReceiveWait         time.Duration
	AutoDiscoveryPeriod time.Duration

	// ReceiverQueueSize overrides how many messages a durable consumer
	// prefetches. Zero uses the derived default; see receiverQueueSize for
	// why it is set explicitly at all.
	ReceiverQueueSize int
}

// Validate reports every problem with a configuration at once.
//
// All of them, not the first: an operator fixing a broker URL only to be told
// about the tenant on the next boot has been made to pay twice for one edit.
func (c Config) Validate() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	switch parsed, err := url.Parse(c.URL); {
	case strings.TrimSpace(c.URL) == "":
		add("url is required (pulsar://host:6650)")
	case err != nil:
		add("url %q is not a URL: %v", c.URL, err)
	case parsed.Scheme != "pulsar" && parsed.Scheme != "pulsar+ssl":
		add("url %q must use the pulsar:// or pulsar+ssl:// scheme", c.URL)
	case parsed.Hostname() == "":
		add("url %q names no host", c.URL)
	}

	for _, field := range []struct{ name, value string }{
		{"tenant", c.Tenant}, {"namespace", c.Namespace},
	} {
		switch {
		case field.value == "":
			add("%s is required: a Pulsar estate serves many companies and "+
				"the tenant is the boundary between them", field.name)
		case !pulsarNamePattern.MatchString(field.value):
			add("%s %q must start alphanumeric and contain only letters, "+
				"digits, '.', '_' or '-'", field.name, field.value)
		}
	}

	if c.AdminURL != "" {
		switch parsed, err := url.Parse(c.AdminURL); {
		case err != nil:
			add("admin_url %q is not a URL: %v", c.AdminURL, err)
		case parsed.Scheme != "http" && parsed.Scheme != "https":
			add("admin_url %q must use http:// or https://", c.AdminURL)
		case parsed.Host == "":
			add("admin_url %q names no host", c.AdminURL)
		}
	}

	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"nack_redelivery_delay", c.NackRedeliveryDelay},
		{"receive_wait", c.ReceiveWait},
		{"auto_discovery_period", c.AutoDiscoveryPeriod},
	} {
		if field.value < 0 {
			add("%s must not be negative, got %v", field.name, field.value)
		}
	}
	if c.MaxDeliveries < 0 {
		add("max_deliveries must not be negative, got %d", c.MaxDeliveries)
	}
	if c.ReceiverQueueSize < 0 {
		add("receiver_queue_size must not be negative, got %d", c.ReceiverQueueSize)
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrConfig, errors.Join(problems...))
}

// adminPorts maps a broker scheme onto the admin scheme and port Pulsar's own
// defaults use.
var adminPorts = map[string]struct {
	scheme string
	port   int
}{
	"pulsar":     {"http", 8080},
	"pulsar+ssl": {"https", 8443},
}

// DeriveAdminURL returns the admin endpoint implied by a broker service URL:
// pulsar://host:6650 becomes http://host:8080, pulsar+ssl://host:6651 becomes
// https://host:8443.
//
// These are Pulsar's own default ports and the layout every standalone and
// helm deployment ships, so an operator who never thought about the admin
// endpoint still gets a working one — and one who has a different layout sets
// AdminURL rather than discovering at boot that subscription lifecycle is
// talking to someone else's API.
func DeriveAdminURL(brokerURL string) (string, error) {
	parsed, err := url.Parse(brokerURL)
	if err != nil {
		return "", fmt.Errorf("%w: cannot derive an admin URL from %q: %w", ErrConfig, brokerURL, err)
	}
	mapping, ok := adminPorts[parsed.Scheme]
	if !ok || parsed.Hostname() == "" {
		return "", fmt.Errorf("%w: cannot derive an admin URL from %q — set admin_url explicitly",
			ErrConfig, brokerURL)
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") {
		// An IPv6 literal has to keep its brackets or the port reads as
		// another group of the address.
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s://%s:%d", mapping.scheme, host, mapping.port), nil
}

// --- resolved knobs -------------------------------------------------------

func (c Config) deliveryBudget() int {
	if c.MaxDeliveries > 0 {
		return c.MaxDeliveries
	}
	return maxDeliveries
}

func (c Config) nakDelay() time.Duration {
	if c.NackRedeliveryDelay > 0 {
		return c.NackRedeliveryDelay
	}
	return nakRedeliveryDelay
}

func (c Config) receiveWait() time.Duration {
	if c.ReceiveWait > 0 {
		return c.ReceiveWait
	}
	return receiveWait
}

func (c Config) discoveryPeriod() time.Duration {
	if c.AutoDiscoveryPeriod > 0 {
		return c.AutoDiscoveryPeriod
	}
	return autoDiscoveryPeriod
}

// prefetchFor sizes a durable consumer's receiver queue.
//
// A batch subscriber needs at least one full batch available LOCALLY for the
// zero-linger drain pass to coalesce anything; an operator who raises
// notification_coalesce_max_batch past the default prefetch would otherwise
// silently start getting partial batches, with the coalescing quietly doing
// less than it says. Two batches, floored at the default.
func (c Config) prefetchFor(maxBatch int) int {
	base := c.ReceiverQueueSize
	if base <= 0 {
		base = receiverQueueSize
	}
	return max(base, 2*maxBatch)
}
