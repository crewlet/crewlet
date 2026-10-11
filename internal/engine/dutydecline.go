package engine

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// A FLEET DUTY DECLINED BY A NODE WHOSE COPY IT DECIDES FROM IS OUT OF SERVICE
// — ADR-0031.
//
// A fleet-singleton duty that decides from a domain's rows decides for the
// whole fleet. The embedding duty publishes the vectors every node applies,
// and it withdraws the vector of every source its own copy no longer shows:
// on a holder whose tracker copy stopped applying while its vectors copy kept
// up, every task created since has a vector and no row, and the duty withdrew
// those vectors fleet-wide. The trim counts the evictions this node's own rows
// hold, and a holder whose copy applied an eviction and not the readmission
// after it uncounted a node that was back and purged past its position. The
// object collector pins its reads through the tracker's read authority, which
// refuses a copy out of service, so its holder renewed the duty and collected
// nothing for as long as the copy stayed wrong.
//
// So such a duty asks the verdict of every domain it decides from BEFORE it
// claims ([Engine.copyOutOfService]), and a node whose copy of any of them is
// out of service DECLINES: it does not claim, it GIVES THE DUTY'S LEASE BACK
// if it holds it ([Engine.workerGiveBack]), and a node whose copies serve
// takes the duty at its own next tick. Not claiming is the conservative
// direction for every one of them — nothing withdrawn, nothing purged, nothing
// deleted. Left to lapse instead, the lease kept the duty from every node for
// its whole TTL — the trim's 45 minutes, plus a 15-minute tick, with no log
// trimmed — over a fault that does not heal on its own. A single data node
// whose copy is wrong runs none of them until it recovers, which that copy's
// own `estate_copy_not_served` line and alarms already name.

// declinable is one duty as its decline is announced.
type declinable struct {
	// event prefixes the duty's two lines, `<event>_declined` and
	// `<event>_resumed`.
	event string
	// duty is the duty as a sentence names it.
	duty string
	// hazard is what deciding from a wrong copy would do — why it declines.
	hazard string
}

// The duties that decide from a domain's rows.
var (
	embedDecline = declinable{event: "embed_duty", duty: "the embedding duty",
		hazard: "deciding from it would withdraw or re-embed the fleet's vectors from " +
			"rows the node has stopped serving"}
	retentionDecline = declinable{event: "retention_duty", duty: "the trim",
		hazard: "deciding from it would count the evictions in rows the node has " +
			"stopped serving, and could purge past the position of a node that is back"}
	collectorDecline = declinable{event: "object_collector", duty: "the object collector",
		hazard: "its passes read the rows that name the files through a read the " +
			"copy refuses, so it would hold the duty and collect nothing"}
)

// dutyDecline is what one duty remembers of its declining: the domain it is
// declining for, "" while it is not. The zero value has declined nothing and
// logs to the engine's logger.
type dutyDecline struct {
	// log is where the two lines go; nil is the engine's.
	log *slog.Logger

	mu  sync.Mutex
	out string
}

// check asks copies — [Engine.copyOutOfService] — whether any of domains is
// out of service on this node, and reports the first that is, to decline
// for. It SAYS SO ON A CHANGE ONLY: a line when the duty starts declining or
// declines for another domain, and one when it stops, so a copy out of service
// for an hour is one line rather than one a tick. A nil copies declines
// nothing — a node that judges no copy of its own.
func (d *dutyDecline) check(ctx context.Context, of declinable,
	copies func(context.Context, ...string) (string, bool), domains []string) (string, bool) {

	if copies == nil {
		return "", false
	}
	domain, out := copies(ctx, domains...)
	if !out {
		domain = ""
	}
	d.mu.Lock()
	was := d.out
	d.out = domain
	d.mu.Unlock()
	logger := cmp.Or(d.log, log)
	switch {
	case out && domain != was:
		logger.WarnContext(ctx, of.event+"_declined", "domain", domain,
			"detail", fmt.Sprintf("this node's copy of %s is out of service, so it does "+
				"not hold %s — %s; it gives the duty back if it holds it, a node whose "+
				"copies serve takes it at its next tick, and this node takes it again "+
				"once its copy is back in service",
				domain, of.duty, of.hazard))
	case !out && was != "":
		logger.InfoContext(ctx, of.event+"_resumed", "domain", was,
			"detail", fmt.Sprintf("this node's copy of %s is back in service, so it "+
				"claims %s again", was, of.duty))
	}
	return domain, out
}

// giveBack gives the duty back where this node holds it ([Engine.workerGiveBack])
// — on every tick that declines, so a give-back the store did not answer is
// tried again a tick later rather than left to the lease's TTL. Nil release
// gives back nothing: a node with no fleet holds no lease.
func (d *dutyDecline) giveBack(ctx context.Context, of declinable,
	release func(context.Context) (bool, error)) {

	if release == nil {
		return
	}
	logger := cmp.Or(d.log, log)
	released, err := release(ctx)
	switch {
	case err != nil:
		logger.WarnContext(ctx, of.event+"_not_given_back", "err", err,
			"detail", "this node declines "+of.duty+" and could not give its lease "+
				"back, so the next tick tries again — or a node whose copies serve "+
				"takes it once the lease lapses")
	case released:
		logger.InfoContext(ctx, of.event+"_given_back",
			"detail", "this node declined "+of.duty+" and gave its lease back, so a "+
				"node whose copies serve takes it at its next tick")
	}
}
