package engine

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
)

// copyVerdict is what the estate's router reads of this node's copy of the
// estate, PER DOMAIN (ADR-0079): which domains' copies are WRONG
// ([stateLog.judgeCopy] says what that means), and of the rest, which answer
// requests now.
//
// Never one verdict for the node: each domain is its own log and applier, so a
// fault is always one log's. An operation's routing and floors are its own
// domain's, so no fault in rows it never reads routes it away, nor may a
// domain no seat depends on go unjudged. A read crossing into a second domain asks that domain's verdict
// too: a declared one runs only where both copies serve (a page's backlinks),
// and a search's semantic half is best effort, skipped rather than moving the
// search ([searchCopies]).
type copyVerdict struct {
	// shut is a copy whose FILE is not open: no log is to blame, and every
	// domain's copy is out of service at once.
	shut bool

	// domains is each running domain's verdict, by name. A domain absent
	// from it was never judged: wrong about nothing, answering nothing.
	domains map[string]domainVerdict
}

// domainVerdict is one domain's copy as the request gate reads it.
type domainVerdict struct {
	// unread is a domain whose health this reading could not read: it says
	// nothing about the copy, and the verdict that stores the reading keeps
	// what it held for it ([copyVerdict.carried]).
	unread bool

	// wrong is [statelog.Health.Healthy] false: the copy cannot become
	// current by applying more records, so it is out of service. why is the
	// fault that holds ([statelog.Health.Fault]), named in the announcing
	// line because it is what an operator acts on; empty while sound.
	wrong bool
	why   statelog.ReadRefusal

	// answers is [statelog.Health.Answers]: whether the copy answers
	// requests now.
	answers bool

	// gates is the domain's own declaration
	// ([statelog.Domain.GatesSeatAdmission]), carried so the SEAT copy is
	// judged over exactly the domains a seat depends on.
	gates bool
}

// shutEstate is what the log names for a copy whose file is not open.
const shutEstate = "estate"

// serves reports whether this node's copy of domain is in service — or, for
// [estate.SeatCopy], whether every domain that gates seat admission is. A
// domain never judged is in service, since nobody found it wrong; a request on
// it still fails [copyVerdict.answers].
func (v copyVerdict) serves(domain string) bool {
	if v.shut {
		return false
	}
	if domain == estate.SeatCopy {
		for _, d := range v.domains {
			if d.gates && d.wrong {
				return false
			}
		}
		return true
	}
	return !v.domains[domain].wrong
}

// answers reports whether this node's copy of domain answers requests now —
// for [estate.SeatCopy], whether every domain that gates seat admission does,
// false where none was judged. A domain never judged answers nothing: nobody
// has measured it.
func (v copyVerdict) answers(domain string) bool {
	if v.shut {
		return false
	}
	if domain != estate.SeatCopy {
		return v.domains[domain].answers
	}
	judged := false
	for _, d := range v.domains {
		if !d.gates {
			continue
		}
		if !d.answers {
			return false
		}
		judged = true
	}
	return judged
}

// carried is v as it is STORED over held, the verdict before it (valid where
// known): every domain this reading could not read keeps what held said of it,
// and every domain it did read takes what it found. So a broker blip changes
// nothing it could not see, and only for the domain it hid: a tracker found
// sound is served at once beside an unread knowledge base. A copy never judged
// answers nothing on an unread reading; a shut reading needs no broker, so it
// is definite and carries nothing.
func (v copyVerdict) carried(held copyVerdict, known bool) copyVerdict {
	if v.shut {
		return v
	}
	out := copyVerdict{domains: make(map[string]domainVerdict, len(v.domains))}
	for name, d := range v.domains {
		if d.unread {
			before, had := held.domains[name]
			had = known && had
			d.answers = had && before.answers
			// A copy that was shut stays out: the reading that would
			// bring it back is the one that could not be made.
			d.wrong = known && (held.shut || (had && before.wrong))
			d.why = ""
			if d.wrong && had {
				d.why = before.why
			}
		}
		out.domains[name] = d
	}
	return out
}

// wentOut is one copy the log announces going out of service: the domain, and
// the fault that holds ([statelog.Health.Fault]) — empty for [shutEstate],
// whose cause is the file rather than any log.
type wentOut struct {
	domain string
	why    statelog.ReadRefusal
}

// newlyWrong is every domain this verdict holds wrong that held (where known)
// did not — what the log announces going out of service — sorted by domain,
// or [shutEstate] alone for a copy whose file has just been found shut.
func (v copyVerdict) newlyWrong(held copyVerdict, known bool) []wentOut {
	if v.shut {
		if known && held.shut {
			return nil
		}
		return []wentOut{{domain: shutEstate}}
	}
	var out []wentOut
	for name, d := range v.domains {
		if !d.wrong {
			continue
		}
		if known && (held.shut || held.domains[name].wrong) {
			continue
		}
		out = append(out, wentOut{domain: name, why: d.why})
	}
	slices.SortFunc(out, func(a, b wentOut) int { return strings.Compare(a.domain, b.domain) })
	return out
}
