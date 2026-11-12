package datadog

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/integration"
)

// Datadog's half of the atlas (ADR-0129, ADR-0130): the organization, its
// monitors with the state of each, its service catalogue, its hosts and its
// Synthetic tests, read whole under the key pair the company already gave
// the engine. Each listing takes a permission of its own, and one the keys
// lack is a finding on the source's standing, never a fault of the read.
//
// DATADOG SAYS AN ENTRY EXISTS ONLY WHERE THE ENTRY LIVES AT DATADOG: the
// organization, a monitor, a catalogue service, a host. What a monitor or a
// test watches is named and related, never claimed, so a monitor somebody
// deletes takes its relation with it and retires nothing another source holds.

func init() { collect.Register(atlasSource{}) }

// atlasSource is Datadog's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindDatadog }

// Collect reads the organization and every listing the keys may read, and
// reports them as claims in the integration's voice. It writes nothing at
// Datadog.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	client, creds, provisioning, err := companyClient(in.Config, in.Env, in.HTTP)
	if err != nil {
		return collect.Read{}, err
	}
	return readAtlas(ctx, client, creds, provisioning.Site, in.Now)
}

// The key namespaces of the entries that live at Datadog.
const (
	monitorKeyPrefix = string(atlas.VendorDatadog) + "/monitor/"
	hostKeyPrefix    = string(atlas.VendorDatadog) + "/host/"
)

// isMonitor reports a monitor of this organization.
func isMonitor(r atlas.Ref) bool {
	return r.Kind == atlas.KindMonitor && strings.HasPrefix(r.Key, monitorKeyPrefix)
}

// watches reports a claim that an entry is watched by one of the monitors.
func watches(c atlas.Claim) bool {
	return c.Facet.Kind == atlas.FacetRelation && c.Facet.Relation == atlas.RelationMonitoredBy &&
		isMonitor(c.Facet.Target.Entry)
}

// tagged reports an entry a monitor's own tags name: a service, or a host by
// its Datadog name. A Synthetic test names an address, a name or a server by
// its address instead, which is how the two parts' relations are told apart.
func tagged(r atlas.Ref) bool {
	return r.Kind == atlas.KindService || (r.Kind == atlas.KindHost && strings.HasPrefix(r.Key, hostKeyPrefix))
}

// holds is which claims each listing's part of a read made, read off the claim
// alone, so a read refused one part carries exactly what an earlier read of
// it recorded. The four are disjoint, and none holds the organization's own.
var holds = map[Listing]func(atlas.Claim) bool{
	ListMonitors:   func(c atlas.Claim) bool { return isMonitor(c.Subject) || (watches(c) && tagged(c.Subject)) },
	ListSynthetics: func(c atlas.Claim) bool { return watches(c) && !tagged(c.Subject) },
	ListServices:   func(c atlas.Claim) bool { return c.Subject.Kind == atlas.KindService && !watches(c) },
	ListHosts: func(c atlas.Claim) bool {
		return c.Subject.Kind == atlas.KindHost && strings.HasPrefix(c.Subject.Key, hostKeyPrefix) && !watches(c)
	},
}

// readAtlas is the read itself, over a client.
func readAtlas(ctx context.Context, client *Client, creds Credentials, site string, now time.Time) (
	collect.Read, error) {

	b := collect.NewBuilder(integration.KindDatadog, now)
	// THE ORGANIZATION FIRST, AND IT IS NOT OPTIONAL: Datadog refuses a bad
	// key with the status it refuses a missing permission with, so only keys
	// this call has just proved make a later refusal the permission's.
	org, err := client.VerifyCredentials(ctx, creds)
	if err != nil {
		return collect.Read{}, fmt.Errorf("datadog: read the organization: %w",
			integration.Reject(err, Status(err)))
	}
	// THE KEY THE SETTINGS GIVE THE ACCOUNT, so the two name one entry.
	account, ok := b.Ref("org", atlas.KindAccount, string(atlas.VendorDatadog)+"/"+strings.TrimSpace(site))
	if !ok {
		return collect.Read{}, fmt.Errorf("datadog: the organization at %q is not an entry the atlas can name",
			client.Site())
	}
	b.Says("org.name", account, atlas.FactAccountName, org.Name)
	b.Says("org.public_id", account, atlas.FactAccountRef, org.Public)

	for _, part := range []struct {
		listing Listing
		read    func() error
	}{
		{ListMonitors, func() error {
			monitors, err := client.Monitors(ctx, creds)
			if err == nil {
				readMonitors(b, account, monitors)
			}
			return err
		}},
		{ListServices, func() error {
			services, err := client.ServiceDefinitions(ctx, creds)
			if err == nil {
				readServices(b, account, services)
			}
			return err
		}},
		{ListHosts, func() error {
			hosts, err := client.Hosts(ctx, creds)
			if err == nil {
				readHosts(b, account, hosts)
			}
			return err
		}},
		{ListSynthetics, func() error {
			tests, err := client.SyntheticTests(ctx, creds)
			if err == nil {
				readSynthetics(b, tests)
			}
			return err
		}},
	} {
		name := part.listing.Name
		err := part.read()
		switch {
		case err == nil:
		case Status(err) == http.StatusForbidden:
			title, action := part.listing.Missing()
			b.Unread(collect.Unread{Part: name, Detail: title + " " + action, Holds: holds[part.listing]})
		case errors.Is(err, ErrInventoryTooLarge):
			b.Unread(collect.Unread{Part: name, Holds: holds[part.listing],
				Detail: "The organization holds more " + name + " than one read walks (" +
					strconv.Itoa(inventoryPageCeiling*inventoryPageSize) + "), so none is read."})
		default:
			// ANYTHING ELSE IS A READ THAT DID NOT FINISH, and fails whole:
			// a listing cut short would retire what it did not reach.
			return collect.Read{}, fmt.Errorf("datadog: read the %s: %w", name, err)
		}
	}
	b.Calls(client.Calls())
	return b.Read(), nil
}

// readMonitors claims each monitor, its name, its state and since when, and
// relates what its tags name to it. A monitor watches the service its
// `service:` tags name and the host its `host:` tags name, by those exact
// keys; its query and its message are never read.
func readMonitors(b *collect.Builder, account atlas.Ref, monitors []Monitor) {
	slices.SortFunc(monitors, func(a, z Monitor) int { return cmp.Compare(a.ID, z.ID) })
	for _, m := range monitors {
		where := "monitors[" + strconv.FormatInt(m.ID, 10) + "]"
		monitor, ok := b.Ref(where+".id", atlas.KindMonitor, monitorKey(m.ID))
		if !ok {
			continue
		}
		b.Relates(where, monitor, atlas.RelationBelongsTo, account)
		b.Says(where+".name", monitor, atlas.FactName, collect.PlainLabel(m.Name))
		state := monitorState(m.State)
		b.Says(where+".overall_state", monitor, atlas.FactMonitorState, state)
		// SINCE WHEN, ONLY FOR A STATE THAT RAISES A SIGNAL: Datadog moves
		// this instant with every group of a multi alert, and a healthy
		// monitor's would otherwise be a record on every such move.
		if !m.StateSince.IsZero() && state != atlas.MonitorOK && state != atlas.MonitorUnknown && state != "" {
			b.Says(where+".overall_state_modified", monitor, atlas.FactStateSince,
				atlas.Instant(m.StateSince).Text)
		}
		for i, tag := range m.Tags {
			at := where + ".tags[" + strconv.Itoa(i) + "]"
			key, value, _ := strings.Cut(tag, ":")
			var watched atlas.Ref
			switch key {
			case "service":
				watched, ok = b.Names(at, atlas.KindService, value)
			case "host":
				watched, ok = b.Names(at, atlas.KindHost, hostKeyPrefix+value)
			default:
				continue
			}
			if ok {
				b.Relates(at, watched, atlas.RelationMonitoredBy, monitor)
			}
		}
	}
}

// monitorKey is the key of the monitor Datadog numbers id.
func monitorKey(id int64) string { return monitorKeyPrefix + strconv.FormatInt(id, 10) }

// monitorState folds Datadog's overall states into the atlas's words: the
// four that say something as they are, and every other (ignored, skipped,
// unknown, one this build has not met) as unknown.
func monitorState(vendor string) string {
	switch vendor {
	case "":
		return ""
	case "OK":
		return atlas.MonitorOK
	case "Alert":
		return atlas.MonitorAlert
	case "Warn":
		return atlas.MonitorWarn
	case "No Data":
		return atlas.MonitorNoData
	}
	return atlas.MonitorUnknown
}

// readServices claims each service of the catalogue, watched by Datadog and
// built from the repositories its definition links. A repository is named by
// its forge address and never claimed: the link shows once a source that
// holds the repository reports it.
func readServices(b *collect.Builder, account atlas.Ref, services []ServiceDefinition) {
	slices.SortFunc(services, func(a, z ServiceDefinition) int { return cmp.Compare(a.Name, z.Name) })
	for _, s := range services {
		where := "services[" + s.Name + "]"
		service, ok := b.Ref(where+".dd-service", atlas.KindService, s.Name)
		if !ok {
			continue
		}
		b.Relates(where, service, atlas.RelationMonitoredBy, account)
		for i, link := range s.Repos {
			at := where + ".links[" + strconv.Itoa(i) + "].url"
			address, _, err := collect.Address(link)
			if err != nil {
				b.Withhold(at, err)
				continue
			}
			key, err := atlas.RepositoryKeyOf(address)
			if err != nil {
				b.Withhold(at, err)
				continue
			}
			if repo, ok := b.Names(at, atlas.KindRepository, key); ok {
				b.Relates(at, service, atlas.RelationBuiltFrom, repo)
			}
		}
	}
}

// readHosts claims each host that reports to Datadog, by its Datadog name,
// watched by Datadog, and suggests its environment from its `env` tag where
// every such tag agrees. No other tag is read.
func readHosts(b *collect.Builder, account atlas.Ref, hosts []Host) {
	slices.SortFunc(hosts, func(a, z Host) int { return cmp.Compare(a.Name, z.Name) })
	for _, h := range hosts {
		where := "hosts[" + h.Name + "]"
		host, ok := b.Ref(where+".name", atlas.KindHost, hostKeyPrefix+h.Name)
		if !ok {
			continue
		}
		b.Says(where+".name", host, atlas.FactName, h.Name)
		b.Relates(where, host, atlas.RelationMonitoredBy, account)
		var envs []string
		for _, tag := range h.Tags {
			if value, ok := strings.CutPrefix(tag, "env:"); ok && collect.EnvironmentWord(value) != "" {
				envs = append(envs, collect.EnvironmentWord(value))
			}
		}
		slices.Sort(envs)
		if envs = slices.Compact(envs); len(envs) == 1 {
			b.Says(where+".tags_by_source", host, atlas.FactEnvironment, envs[0])
		}
	}
}

// readSynthetics relates what each running Synthetic test checks to the
// monitor that carries the test's state: the address it requests, and that
// address's name or server. A paused test watches nothing. A test's request
// headers, body and credentials are never read.
func readSynthetics(b *collect.Builder, tests []SyntheticTest) {
	slices.SortFunc(tests, func(a, z SyntheticTest) int { return cmp.Compare(a.PublicID, z.PublicID) })
	for _, t := range tests {
		if !t.Live || t.MonitorID == 0 {
			continue
		}
		where := "tests[" + t.PublicID + "].config.request"
		monitor, ok := b.Names(where, atlas.KindMonitor, monitorKey(t.MonitorID))
		if !ok {
			continue
		}
		host := t.Host
		if t.URL != "" {
			address, name, err := collect.Address(t.URL)
			if err != nil {
				b.Withhold(where+".url", err)
				continue
			}
			host = name
			if endpoint, named := b.Names(where+".url", atlas.KindEndpoint, address); named {
				b.Relates(where+".url", endpoint, atlas.RelationMonitoredBy, monitor)
			}
		}
		if host == "" {
			continue
		}
		at := where + ".host"
		var watched atlas.Ref
		if ip, err := netip.ParseAddr(host); err == nil {
			key, err := atlas.AddressHostKey(ip.String())
			if err != nil {
				b.Withhold(at, err)
				continue
			}
			watched, ok = b.Names(at, atlas.KindHost, key)
		} else {
			watched, ok = b.Names(at, atlas.KindHostname, host)
		}
		if ok {
			b.Relates(at, watched, atlas.RelationMonitoredBy, monitor)
		}
	}
}
