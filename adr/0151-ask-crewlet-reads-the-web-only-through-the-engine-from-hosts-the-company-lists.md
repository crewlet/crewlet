# ADR-0151: Ask Crewlet reads the web only through the engine, from hosts the company lists

- **Status:** superseded
- **Superseded-by:** ADR-0163
- **Authority:** `internal/assistant`
- **Enforced-by:**
  - `internal/config.TestAHelperRunsOnAnOrdinaryEntry`
  - `internal/engine.TestAHelpersRunHasNoWebToolWhateverItsEntrySays`
  - `internal/research.TestAPageIsReadAsItsText`
  - `internal/assistant.TestATurnReadsAFewPagesFencedAsTheSites`
  - `internal/assistant.TestAProposalAfterOutsideTextIsTainted`
  - `internal/assistant.TestAWebTaintedTurnProposesOnlyOnItsSubject`
  - `internal/api/queries.TestASearchAnswersOrSaysWhyNot`
  - `internal/assistant.TestATurnSearchesAFewTimesFencedAsTheSites`
- **Tag-status:** unreleased

## The decision

**Ask Crewlet's quick path reads the web only through catalogue reads the
engine itself runs, and only from the hosts an administrator lists.** The
model's harness keeps its own web tools denied (`web_tools: denied`), as it
does today.

- **Off until turned on.** `assistant.research` is off by default. An
  administrator turns it on and lists the hosts, `https` only; each host is
  confirmed by a person, and the suggested list comes from the connected
  integrations' documentation hosts.
- **Two reads.** `fetch_web_page` reads one page from a listed host;
  `search_web` searches through the company's own search tool server, held to
  the listed hosts, its key in the secret store and never in the model's
  context.
- **What a fetch may do.** `GET` only, with no cookies, no `Authorization` and
  no header from the company's configuration, under a fixed generic
  `User-Agent`. A redirect is followed only to a listed host. The address is
  checked when the connection is made (`httpx.Guarded`), so no private,
  loopback or link-local target is reached.
- **Bounds.** A page body of at most 2 MiB, 16 KiB of its text to the model,
  15 seconds, at most 8 fetches and 4 searches a turn, a query of at most 200
  characters, and a ten-minute cache on each node keyed by URL.
- **Trust.** Every page and result is fenced as external, which taints the
  turn. Each page read is a source record (the engine-built URL, its title,
  when it was read) that the answer cites; a link the model writes stays
  inert.
- **A tainted turn stays on its subject.** After reading web text, a turn
  proposes changes only on the records the person's question or page
  selection named.
- **Every call is visible.** Each URL and query is a step the person sees
  before the answer and the conversation keeps, and `assistant_turn_completed`
  records each step's tool and route as it does for every read.

## Why the obvious alternative is wrong

The obvious alternative is turning on the harness's own web tools for the
quick path. The harness would then fetch whatever URL a fenced instruction
steered it to, with the person's data in the query string, outside every
request the engine sees, decides or records. Deep work (ADR-0054) already
reaches any host, in a box and only where it is configured; the quick path
answers in the dashboard, beside proposals the person approves, and must not
be a second unbounded path out of the company.

With the engine as the fetcher, every read is a step the person sees, bounded
by hosts an administrator chose, carried by the address check every other
outbound call uses, and fenced like any other text from outside the company.

## What this does not decide

- Deep work's own web access, which ADR-0054 decides.
- Which hosts a company lists.
- A managed search key for hosted tenants.
- What a person may do with a page they open from a cited source.
