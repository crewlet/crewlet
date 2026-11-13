# ADR-0140: A task names the atlas entries it touches by id, on its own record

- **Status:** accepted
- **Authority:** `internal/worklet`
- **Enforced-by:** `internal/worklet.TestATaskNamesAnAtlasEntryOnlyByItsID`, `internal/worklet.TestAHistoryRowCountsTheAtlasEntriesAndNamesNone`, `internal/worklet.TestADetailWithoutEntriesCountsThemAndKeepsEveryOtherEdge`, `internal/worklet.TestTheOpenWorkOnEachAtlasEntryIsCounted`, `internal/worklet.TestAnAtlasEntrysWorkIsItsOpenAndRecentlyFinishedTasks`, `internal/worklet.TestAClosedRecordVersionGainsNoField`, `internal/agent/builtin.TestATaskIsLinkedToTheAtlasEntriesItTouches`, `internal/agent/builtin.TestLinkingAndReadingATasksEntriesTakesTheAtlas`, `internal/api/queries.TestAWorkItemNamesItsAtlasEntriesOnlyToAnAtlasReader`, `internal/api/queries.TestTheWorkOnAnEntryIsReadOnlyForAWorkletReader`, `internal/api/queries.TestAMarkOnAnEntryTheAtlasDoesNotHoldMarksNothing`, `internal/agent/prefetch.TestTheSystemsBlockListsWhatTheWorkItemNamesFirst`
- **Cost-when-tried:** not tried. The nearest rule is a task's linked page: an edge on the task's own record whose other end is another domain's id, with no write to that domain.
- **Tag-status:** unreleased

## The decision

**The link is the task's.** Work that touches a server, a service, a database or an account says so as a relation of kind `atlas_entry` on the task, beside its links and its pages. It rides the task's own record on the tracker's log, so every data node holds it identically with the task, in the task's history, and it leaves with the task. The atlas is never written: an entry does not know which tasks name it, and nothing about a link is a claim about the entry.

**The other end is the entry's id, never its name or key.** The writer refuses anything but an id in its canonical spelling, so no host name, address or vendor account enters the tracker's log, and an entry purged from the atlas leaves nothing to purge in the tracker. A task names at most 32 entries, inside the 128 edges it may carry besides its dependencies.

**An id is withheld with the name it would confirm.** An entry's id is derived from its kind and key, so a reader handed the id could test a guessed address against it. A reader who may not read the atlas is therefore told how many entries a task names and handed none: the work item's answer drops the edges and carries the count, on the dashboard's question and on the tool a seat reads the item with. A history row records how many entries the task named before and after a change, never which, because a history row is read wherever the task is.

**Each app decides its own half**, as [ADR-0127](0127-reading-the-atlas-takes-the-atlas-permission.md) rules for every panel the atlas joins from another app:

- *Linking* is `update_work_item`'s `linked_entries`, so it takes Worklet. An entry is added only through the caller's own read of the atlas, which takes `atlas` as well (and, for a seat, whoever its turn works for, by [ADR-0109](0109-a-turn-somebody-woke-is-held-to-the-apps-they-use.md)): nobody links what they may not read, and no id that is not an entry is recorded. A link is taken off by its id with no read, so one to an entry the atlas no longer holds can be removed.
- *The work on an entry* (the open tasks counted per entry, and one entry's open and recently finished tasks) is asked on the atlas's app and read from the tracker only for an asker who uses Worklet. Anybody else is told no work was read and why, and the tracker is not asked.

**Read through the entry the id merged into.** Two records of one thing are one entry when the atlas is read ([ADR-0126](0126-the-atlas-computes-what-the-engine-knows-and-records-what-it-is-told.md)), so a link is resolved at read time through whichever entry holds its id: a mark lands on the entry the map draws, an entry's work is asked by every id it answers to, and an id the reader's atlas does not hold marks and names nothing.

**A turn on a work item is told its entries first.** The systems block of a turn's prompt lists the entries the turn's work item names, in the item's order, before the entries its ask names, inside the same bounds and only where the turn may read the atlas.

**Record version 16.** A build reading 15 would keep the edge and leave it out of the history row's deltas, which are derived from the kinds it knows, so its rows would differ from every newer node's. A record whose relations name an entry is stamped 16 and an older node holds it back.

## Why the obvious alternative is wrong

**A relation in the atlas, from the entry to the task.** The atlas records claims about what the company runs on, each with a source and a confidence, folded on every read. "Somebody is working on this" is not a claim about the entry, would need a writer in the atlas for every task change, and would be history the atlas keeps for 400 days about work the tracker already keeps.

**The entry's key as the other end.** A key is the entry's name: a host name, an address, an account id. Stored on a task it is readable by every Worklet holder, which is the map Atlas's permission withholds, and an endpoint's key is an address the tracker would read as a pull request to wait on.

**A custom field on the task.** A field's values are the project's to declare and anybody's to read, and they are compared and filtered as text. An entry is another app's object behind another permission.

**Showing the ids to every Worklet reader.** The id is a name-based hash of the kind and key. An address space is small enough to enumerate, so the id of a linked entry confirms its address to somebody the atlas was withheld from.

## What this does not decide

- **What a link says about the entry's health.** Open work is drawn as a mark of its own and never changes the entry's health, which is the engine's word for its open signals.
- **Links from other trackers.** A company whose work is kept outside Worklet has no such links; the questions say so.
