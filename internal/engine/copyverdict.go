package engine

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
)

// copyVerdict is what the estate's router reads of this node's copy of the
// estate, PER DOMAIN (ADR-0072): which domains' copies are WRONG
// ([stateLog.judgeCopy] says what that means), and of the rest, which answer
// requests now.
//
// # Why per domain, and never one verdict for the node
//
// The domains are separate logs with separate appliers, and a fault is always
// one log's: an applier halted at one page, one log a node fell below, one
// record past the deferral grace. A verdict for the whole copy took every
// domain out of service for it — a knowledge-base applier halted on one page
// sent every tracker call the node's seats made to a peer — and judged only
// the domains a seat depends on, so a wrong vectors or usage copy was served
// for ever. An operation's routing is on its own domain, and so are its
// floors: the rows it answers from are that domain's, so no operation is
// routed away for another domain's fault it does not read. The reads that
// cross into a second domain's rows ask that domain's verdict too: an
// operation that declares one runs only where both copies serve (a page's
// backlinks, over the tracker's), and a search's semantic half, over the
// vectors, is best effort and never moves the search it belongs to — the
// scanner asks the vectors' verdict itself and skips the half
// ([searchCopies]).
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
	// current by applying more records, so it is out of service — and why
	// is the fault that holds ([statelog.Health.Fault]), which the line
	// announcing it names, since which one holds is what an operator acts
	// on. Empty while the copy is sound.
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
// domain never judged is in service: nobody has found it wrong, and a request
// on it is held to [copyVerdict.answers], which it fails.
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
// which is false where none was judged. A domain never judged answers
// nothing: nobody has measured it.
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
// and every domain it did read takes what it found.
//
// A BROKER BLIP CHANGES NOTHING IT COULD NOT SEE — a copy that was answering
// keeps answering, and one that was wrong stays wrong — and it is PER DOMAIN:
// one domain's health going unread brings no OTHER domain's copy back and
// takes none out, so a reading that found the tracker sound and could not
// read the knowledge base serves the tracker at once. A copy never judged
// before answers nothing on an unread reading. A shut reading is definite —
// the file's state needs no broker — and carries nothing.
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
			// A COPY THAT WAS SHUT is still out of service on no
			// information: the reading that would bring it back is the
			// one that could not be made.
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
