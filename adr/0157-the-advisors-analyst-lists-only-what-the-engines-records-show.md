# ADR-0157: The advisor's analyst lists only what the engine's records show

- **Status:** superseded
- **Superseded-by:** ADR-0164
- **Authority:** `internal/advisor/modelpass`
- **Enforced-by:**
  - `internal/advisor/modelpass.TestTheModelPassListsOnlyWhatItsOwnReadsShow`
  - `internal/advisor/modelpass.TestARunThatDeclinesOrRunsOutSaysSo`
  - `internal/advisor.TestNoDetectorCallsAModel`
  - `internal/config.TestAHelperRefusesOnlyAHarnessItCannotRunWithNoToolOfItsOwn`
- **Cost-when-tried:** not tried. The rule list named only what a rule was written for: six costly patterns in one company's week (runs ending on their round cap, a seat stopped on its budget, open work on one agent, an item twelve others waited on, merges escalated for want of CI, ready work behind a person) went unmentioned until each got a rule.
- **Tag-status:** unreleased

## The decision

**A model may advise about the company, and nothing it says is listed until the engine has checked it.** The setup advisor's analyst is one run on the company's own models (`advisor.analyst.llm`, off until named), on demand and on a cadence, that reads the week through read tools and submits findings. [ADR-0115](0115-a-setup-finding-carries-its-fix-and-only-a-person-sends-it.md) still holds for the rule list: no detector calls a model, and the analyst's findings are listed apart, never in the Inbox, Home or the go-live check.

- **It reads records, never words.** Its tools answer numbers, closed words, times, handles and record ids from the reads `GET /advisor` makes, over the redacted document. No comment, message, title or transcript reaches it, so words somebody wrote cannot steer what an administrator is told.
- **Grounding is the engine's.** A finding is listed only when every record it cites and every number (at the path it names) is one this run's own tools answered, it does not say a rule finding again, and its fix is one the engine builds and checks against the company's rules. Everything else is dropped and counted by why.
- **It never applies anything.** A fix is the advisor's own kind of change, previewed and sent by a person.
- **It is bounded as a person's assistant is.** Its model entries are held to Ask Crewlet's member rules, its rounds are charged to the company's windows and never a seat's, under a token ceiling of its own, and one run goes at a time across the fleet, claimed on its coordination record.
- **People's answers shape what follows.** Useful, wrong or already known, kept by the finding's subject, records and kind of fix: a wrong one is never listed again, an already-known one not for 30 days, and the next run is told.

## Why the obvious alternative is wrong

**Listing what the model writes.** A model reading a company's week will name records it never read and numbers it rounded, and an administrator acting on one is acting on nothing. The check costs a lookup per citation; a wrong finding costs a person's trust in the whole list.

**Handing the model the company's words.** A comment or an item title is written by agents and by people outside the company; a model that reads them can be told what to report. The patterns that cost a company are in the numbers and the shape of the work, and the words stay one click away on the records it cites.
