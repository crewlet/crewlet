# ADR-0127: Reading the atlas takes the atlas permission

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/iam.TestAtlasIsTheLastApp`, `internal/iam.TestAtlasIsOptIn`, `internal/iam.TestEveryAppIsAGrantAndAdminIsNot`, `internal/authz.TestEveryAppIsNamedByARow`, `internal/authz.TestEveryAppRowAdmitsAHolderAndRefusesANonHolder`, `internal/authz.TestShowsFollowsTheKindsApp`, `internal/authz.TestARequesterWithoutTheAppRefusesItsVerbs`, `internal/api/iamapi.TestTheDashboardOffersExactlyTheEnginesGrants`, `cmd/crewlet.TestAPersonsInviteDefaultsToTheDefaultApps`, `internal/agent/builtin.TestEveryAppOpensAPersonFacingTool`, `internal/agent/builtin.TestTheAtlasIsReadOnlyByAHolderOfItsApp`, `internal/agent/builtin.TestTheAtlasListsRepositoriesOnlyForAGitletHolder`, `internal/engine.TestATurnsPromptNamesTheAtlasOnlyWhereWhoeverWokeItMayReadIt`, `internal/agent/builtin.TestAnAgentReadsAPrivateAddressOnlyWithinItsReach`
- **Cost-when-tried:** not tried. No build has shown the atlas to anybody, so there is no earlier rule to replace. The nearest one is Gitlet before [ADR-0110](0110-reaching-a-repository-takes-the-gitlet-permission.md): a product every person could read, which a company could not withhold from a colleague.
- **Tag-status:** unreleased

## The decision

**Atlas is an app.** `atlas` is the sixth member of `iam.Apps` and the seventh permission, appended after `gitlet`, so no stored row moves. An administrator holds it through `iam.Expand`, and an agent seat holds it as it holds every app. A person, a personal access token, a Tier A token and a service account hold it when they are given it.

**It is opt-in.** `iam.OptInApps` names it, and `iam.DefaultApps`, every app but the opt-in ones, is what a person's invitation and create carry when nothing names their access: `crewlet iam invite` and `create` without `-grants`, and the dashboard's **Invite** and **Add person**, which open with it unticked. A full map of a company's servers, addresses and vendors is what an intruder looks for first, so holding it is a decision somebody makes about the person. The people who run the company hold it from the start, because admin includes every app.

**A holder reads all of it.** Every verb of the authority table that reads the atlas names the app (`atlas_search`, the first, a person's read on the operator surface), and nothing narrower stands behind it: every holder reads every entry with its facts, sources and relations, private addresses included. A principal without it is refused at the table, with the app's own refusal, before an entry is read. An atlas entry is the app's kind (`authz.KindAtlasEntry`), so an answer that mixes apps shows none to a reader without it.

**The atlas is never a way round another permission.** A panel the atlas joins from another app's records, such as an integration's findings, the work that touches an entry, or a repository only one unit may read, is decided by that app where it is joined, never by `atlas`. So the atlas reads Gitlet as its reader and only for a reader who may read the company's code: a holder of `atlas` without `gitlet` is shown no repository, and the answer says Gitlet was not read and why.

**The requester ceiling carries the app to a seat's work**, by [ADR-0109](0109-a-turn-somebody-woke-is-held-to-the-apps-they-use.md)'s rule for every app. A seat's atlas verb in a turn a person, or a credential acting as no seat, woke is admitted only if they hold `atlas`, so an agent reads the atlas only within what the person who woke it may read. Turns nobody woke, and turns another seat woke, are unchanged.

**No export and no per-read record.** No route of the app copies the atlas out of the engine as a file, and reads are not recorded one by one: the permission is the decision, and a log of reads of a map every holder reads in full would record nothing a later decision depends on.

**Erasing is an administrator's.** The app reads the atlas and, once the atlas keeps what people tell it, corrects it. Purging a value everywhere, history included, destroys what every holder reads, so it is an `admin` verb behind a fresh sign-in check, never the app's.

**A rolling upgrade opens nothing.** An older node knows no `atlas`: an unknown grant is not `Valid` there, is kept on the row and grants nothing, and an older node serves no atlas verb at all.

## Why the obvious alternative is wrong

**Everybody's, as the org chart and the agents' status are.** The atlas is assembled from what the engine already holds, and each fact in it is visible to somebody. But not to everybody, and not in one place: the settings that name the servers and accounts are an administrator's, and a map that joins them is reconnaissance, readable by every invited contractor.

**Administrators only.** The people who ask "what depends on this?" are the ones who run systems day to day, an on-call engineer or a team's lead. Making them administrators to read a map hands them host access ([ADR-0033](0033-access-is-admin-and-one-permission-per-app.md)) for a read.

**Filtered per reader by what their own access reaches.** A map with holes cut per reader answers "what depends on this database?" with a shorter list than the truth, and nothing on the screen says what was cut. The app is the person's boundary, as it is for every app, and the details another app guards stay with that app (the panels rule above).

**Ticked by default, like the other apps.** Every invited colleague would hold the company's map on their first day, and the company would have to remember to untick it for every contractor. An app is opt-in when holding it is a decision about the person rather than about the company's tools.

## What this does not decide

- **Which entries exist and how they are read.** That is the atlas's own record and its package.
- **What an agent may record in the atlas.** Reading is decided here; recording, and the evidence it takes, is decided where the atlas first takes an agent's records.
- **Which screens show the atlas.** The dashboard locks the Atlas screen for somebody without the app, never hides it, as it locks Gitlet's workspace, when that screen lands.
