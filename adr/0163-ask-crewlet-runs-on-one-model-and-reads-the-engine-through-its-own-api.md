# ADR-0163: Ask Crewlet runs on one model, reads the engine through its own API and the web through the engine

- **Status:** accepted
- **Authority:** `internal/assistant`
- **Enforced-by:**
  - `internal/api.TestPermissionParityAcrossFivePrincipals`
  - `internal/assistant/catalog.TestAGenericCallNamesARouteOfTheTable`
  - `internal/api.TestNoWriteWithoutApproval`
  - `internal/api.TestInjectedTextNeverBecomesAWrite`
  - `internal/assistant.TestAProposalAfterOutsideTextIsTainted`
  - `internal/assistant.TestAWebTaintedTurnProposesOnlyOnItsSubject`
  - `internal/research.TestAPageIsReadAsItsText`
  - `internal/assistantdomain.TestAConversationPastItsRetentionIsReaped`
  - `internal/assistantdomain.TestAnotherPersonsConversationIsNotFound`
  - `internal/assistantdomain.TestAPersonsConversationsAreListedNewestFirstAndSearchedByTitle`
  - `internal/assistantdomain.TestAPersonSetTitleIsNeverOverwritten`
  - `internal/api/assistantapi.TestAConversationIsRenamedAndSearchedByItsOwner`
  - `internal/config.TestAskCrewletIsConfiguredByOneModel`
  - `internal/config.TestAHelperRunsOnAnOrdinaryEntry`
  - `internal/engine.TestAHelpersRunHasNoWebToolWhateverItsEntrySays`
  - `internal/api/queries.TestEveryModelEntrySaysWhetherAHelperCanRunOnIt`
  - `internal/assistant.TestATurnRunsUnderTheFixedBounds`
  - `internal/assistant.TestABigTaskBecomesATaskForAnAgentSeat`
  - `internal/assistant.TestAQuestionsImagesAreHandedToItsModel`
  - `internal/api.TestEveryScreenReadIsAnAssistantRead`
  - `internal/api.TestAGenericReadIsTaggedWithTheAppsItDrewOn`
  - `internal/assistant/catalog.TestDescribeAPIListsOnlyWhatThePersonMayRead`
  - `internal/assistant/catalog.TestAReadSelectsOnlyTheFieldsAsked`
  - `internal/research.TestAnyPublicHttpsPageIsRead`
  - `internal/research.TestAPrivateAddressIsNeverReached`
  - `internal/assistant.TestAURLTheModelComposedIsNeverFetched`
  - `internal/providers/llm/harness.TestASearchResultsURLIsTakenFromTheToolResultNotTheProse`
  - `internal/providers/llm/harness.TestTheSearchRunHasNoToolButTheVendorsSearch`
  - `internal/config.TestAnEntryCannotTakeTheSearchStance`
- **Measured:** the catalogue offered 45 curated read tools, each wrapping one or two GET routes that the generic read (`api_request`) already reached under the same grant and the same 16 KiB answer to the model; a turn's own bounds were 30 rounds and 900 seconds against a read grant that lives at most 1200 seconds.
- **Cost-when-tried:** Settings › Ask Crewlet carried eleven settings on six cards (a model chain, a stronger chain and its briefs switch, four turn bounds, eight tool families to turn off, a host list, a deep-work model and its three bounds), and no ordinary model entry could be chosen until somebody made one with its web tools denied. A page from a vendor's documentation the list did not name was unreadable, and the search needed a second vendor's server and key.
- **Tag-status:** unreleased

## The decision

**Ask Crewlet is configured by one model and nothing else, `assistant.model`, a `providers.llm` key; absent, it is off.** Every bound a setting used to carry is a constant at its consumer: 40 model rounds a turn, a 20-minute turn (the read grant's own ceiling), a 15-minute approval wait ([ADR-0051](0051-an-assistant-write-executes-only-from-its-owners-approval-of-a-server-rendered-preview.md)) and 30 days of conversations ([ADR-0053](0053-an-assistant-conversation-belongs-to-its-owner.md)). There is no stronger second model, no company-wide list of tools turned off (what a person's assistant may do is what the person may do, less [ADR-0052](0052-every-authority-row-says-what-the-assistant-may-do-with-it.md)'s human-only list), and no host list. A person's own Ask Crewlet budget stays a budget, set beside the company's.

**Any ordinary entry qualifies; the engine turns the harness's own tools off by construction.** The helper's provider is built from the entry's merged profile with its web tools forced to denied and its local tools denied, whatever the entry says, so the company's usual Claude Code or Pi entry runs Ask Crewlet without a dedicated copy, and a seat on the same entry keeps its web tools. Only a harness that cannot be forced is refused, by one check shared with the Advisor's model: no measured web denial, local tools the vendor cannot turn off, or a tool with no switch (Codex's `apply_patch`, as [ADR-0050](0050-the-assistant-acts-only-through-a-grant-derived-from-the-persons-session.md) already says).

**The API is the read surface.** Ask Crewlet reads through one generic read of the GET routes the API itself mounts (`App.AssistantRoutes`), each call an ordinary request under the person's read grant, with a field selection so a large answer fits the model's share, and one description tool that lists the readable routes of a domain filtered to what this person may read. The per-screen read tools are retired. A gate walks every GET route the dashboard reads and fails unless the assistant's table holds it or its verb is human-only, so "anything the person can see" stays true as screens are added. Writes are unchanged: a proposal rendered from a server-side preview, approved by the person, sent once ([ADR-0051](0051-an-assistant-write-executes-only-from-its-owners-approval-of-a-server-rendered-preview.md)).

**There is no deep work.** A task that needs a shell, files or hours of steps is proposed as a work item for one of the company's agent seats, which already run in boxes with their own tools, budgets and review. [ADR-0054](0054-deep-work-runs-the-assistants-cli-with-its-own-tools-only-in-a-box.md) is superseded.

**The web is read through the engine, from any public host, and only at addresses nobody composed after seeing the company's data.** A page is fetched by the engine (`https`, port 443, `httpx.Guarded` at connect time, `GET` with no credential or configured header, ADR-0151's size, time and per-turn bounds) only at a URL the engine recorded as seen in the conversation: in the person's own words, in a search tool's result blocks, among the links of a page it fetched, or in a URL field of an answer the turn read. A URL the model wrote is refused. A search runs as a search-only run of the same entry's harness: the vendor's server-side search and no other tool, no engine tool server, and nothing but the query. Every page and result is fenced as external and taints the turn, and a tainted turn proposes only on the records the person named. [ADR-0151](0151-ask-crewlet-reads-the-web-only-through-the-engine-from-hosts-the-company-lists.md) is superseded.

**Its page is a chat.** `/ask` is a full page with the person's conversation history (listed, searched by title, renamed, deleted), and the drawer opens the same conversation there.

## Why the obvious alternatives are wrong

**A tool per screen.** Forty-five tools cost prompt on every turn, cover only what somebody wrapped, and drift from the routes they wrap; the route table is already the authority on what exists and what the person may read, and the model's job is to choose a route.

**A host list as the web's safety.** A list bounds where a page comes from, not what it says: a listed documentation site can carry an injected instruction as well as any other. What an instruction could do is already bounded elsewhere: it cannot write (the person approves a preview), cannot leave the turn's subject (taint), and, with URL provenance, cannot carry the company's data out in an address, because every address it could reach was written before it could see that data. The list cost every company an administrator's afternoon and still left the vendor page nobody listed unreadable.

**The harness's own fetch and search.** Its fetch runs on the engine host, outside `httpx.Guarded`, and fetches whatever URL the model writes, which is the exfiltration path. Its search alone, in a run holding nothing but the query, sends the query to the vendor that already processes the whole turn.

**Folding deep work into the one model.** A runner with its own shell beside the company's agents is a second agent runtime with weaker review and a box of its own; the company's agents already do that work, and an item makes it visible, budgeted and reviewed.

**A dedicated entry for each helper.** It is a setting in disguise, and the safety it bought is the engine's to enforce: the helper's argv is the engine's to build, so the engine builds it with the tools off.

## What this does not decide

The grant, the approval and the conversation store, which ADR-0050, ADR-0051 and ADR-0053 still decide. The Advisor ([ADR-0164](0164-the-advisor-keeps-what-its-detectors-find-and-a-fix-is-applied-only-after-the-engine-re-checks-it.md)). Which harnesses have a measured search-only stance, which `internal/providers/llm/harness/harnessprofile` records.
