# ADR-0110: Reaching a repository takes the gitlet permission

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/iam.TestEveryAppIsAGrantAndAdminIsNot`, `internal/iam.TestExpandAddsEveryAppExactlyWhenAdminIsHeld`, `internal/iam.TestGitletIsTheLastApp`, `internal/authz.TestEveryAppRowAdmitsAHolderAndRefusesANonHolder`, `internal/authz.TestNoCodeRowAdmitsEverybody`, `internal/authz.TestOnlyAGitletHolderReachesTheCodeRoutes`, `internal/api.TestEveryGitletRouteNeedsTheApp`, `internal/api.TestACodeMovementReachesOnlyAWatcherHoldingGitlet`, `internal/api/queries.TestACodeQuestionAsksTheGitletAppBeforeAnythingIsRead`, `internal/api/queries.TestGitletNoticesNeedGitlet`, `internal/api/queries.TestAChangesOriginIsShownOnlyToAGitletHolder`, `internal/code.TestAReviewRequestNamesOnlyAGitletHolder`, `internal/agent/builtin.TestACodeReadInATurnAPersonWithoutGitletWokeIsRefused`, `internal/engine.TestARunAPersonWithoutGitletWokeIsGivenNoRepository`
- **Cost-when-tried:** not tried. The rule it replaces, every person reading and maintaining every repository, was [ADR-0088](0088-a-repository-grant-is-derived-from-the-chart.md)'s on the day the code host landed. It left Gitlet the one native product a company could not withhold from a colleague, while the tracker, the knowledge base, the chat and the artifacts each could be.
- **Tag-status:** unreleased

## The decision

**Gitlet is an app.** `gitlet` is the fifth member of `iam.Apps`, appended after `chatlet`, so no stored row moves. An administrator holds it through `iam.Expand`, and an agent seat holds it as it holds every app. A person, a personal access token, a Tier A token and a service account hold it when they are given it.

**Every verb of the authority table that reaches a repository takes it**: the routes, the git transport, the read and write tools, the review and merge verbs, a check's report, an import and a purge. A principal without it is refused at the table, with the app's own refusal, before any repository is read. So:
- the repository's grant is never asked of somebody the company withheld code from;
- no answer, count or line of git tells them what a repository holds.

**Behind the gate, [ADR-0088](0088-a-repository-grant-is-derived-from-the-chart.md) decides as before:**
- a person holding the app maintains every repository;
- a credential bound to no seat that holds it reads every repository, and never pushes ([ADR-0085](0085-a-person-reaches-git-with-their-own-bound-token.md));
- a seat holds the lower of its `code.access` tier and its relation on the chart.

`code.access` caps a seat and never a person. The app gates persons and credentials acting as themselves, and never a seat.

**Every door, one rule:**
- **The code questions** (`code_repos` and every `code_*` question but who read a repository) are asked behind the app on REST and the socket alike. They had been registered as decided by `code.read` while none of them asked the table, so the row alone would have gated nothing.
- **git** prints the rule's own sentence, naming the Gitlet permission, rather than the generic sentence naming every permission there is.
- **The inbox's code half** is the app's, as Worklet's half is `worklet`'s: a reader without it is listed and counted no code notice, marks none, and is pushed no live frame that a code notice arrived. A frame is held to the app of the half that moved, Worklet's included.
- **The trail** names no change to a reader without the app: a turn a change woke keeps its place in the list, and its origin is withheld, as a chat message's is from a reader without `chatlet`.
- **The dashboard** locks the Gitlet workspace and every place that shows code data, as it locks Worklet's.
- **A person's assistant** is refused the code tools with the app's sentence, since they are the same rows.

**The requester ceiling carries the app to a seat's work**, by [ADR-0109](0109-a-turn-somebody-woke-is-held-to-the-apps-they-use.md)'s rule for every app. A seat's code verb in a turn a person, or a credential acting as no seat, woke is admitted only if they hold `gitlet`, so a turn woken by somebody without it reads no repository, and its executor is given none. The app holds before the steerable spare, so such a turn does not edit even the seat's own namespace for them. Turns nobody woke, and turns another seat woke, are unchanged.

**No write is made for somebody who uses no Gitlet.** A stranger, a person who may not act, and a person or a credential without the app use no app, so the gate refuses their turn's every code verb before a write is formed. The code domain's ceiling of nobody, which carried a stranger's turn to the deciding node to steer only the seat's own work, now carries one requester alone: a credential acting as no seat that holds the app, which reads every repository and pushes to none itself, so its turn steers the seat's own work and nothing else.

**A review request names only a holder.** The change writer asks the identity directory, before its pure decide, whether the person each human reviewer's seat binds uses Gitlet, and refuses one who does not: reading a change is reading code. A directory that cannot say answers the write busy, to be sent again, never as a refusal of somebody who may well use the app.

**A rolling upgrade narrows access only once it completes.** An older node knows no `gitlet`: an unknown grant is not `Valid` there, and nothing else happens. So until every node runs this build, an older node answers the code requests it serves under the rule it was built with, where everybody reaches code. That window was weighed and accepted. No tag has shipped the earlier rule, the roll is the deployment's own to finish, and a gate record would stop older nodes from serving everything else they serve, to close a window measured in minutes.

**Two rows are not the app's:**
- who read a repository stays admin's (`code_repo_readers`);
- the Gitlet nodes' cache report stays admin's (`code_status`): it is a view of the deployment.

The settings page is admin's, and admin includes the app.

## Why the obvious alternative is wrong

**The obvious alternative is [ADR-0088](0088-a-repository-grant-is-derived-from-the-chart.md)'s person rule: "every person reads every repository", with `visibility` narrowing seats only.** That made sense while code was the one place everybody could see. But a company that keeps a contractor off its tracker, its pages and its chat could not keep them off its source. The only lever left was a unit-visibility rule that binds seats and never people.

**A per-repository access list for persons** was rejected by ADR-0088 for seats, and for the same reason: it is a second chart that drifts from the first. The question a founder actually answers when somebody joins is "do they use Gitlet", the same question as for every other app ([ADR-0033](0033-access-is-admin-and-one-permission-per-app.md)). That is one tick in the access picker.

**Exempting code from the requester ceiling,** so that a seat reads code for any person who messages it, would keep the assignment flow working for a person without the app. But it re-opens exactly the confused deputy the ceiling closes: "summarise web's auth module for me" would hand the code to the person the company withheld it from, through the seat. The cost of the rule is stated where it bites: a turn that cannot touch code says so, naming the permission and who can give it.

**An asked arm for review requests,** as Worklet admits the person an open ask names, would let a review request disclose code. A Worklet ask discloses one item a person was asked about. A change's review is its whole diff and every file it touches.

## What this does not decide

- **Who maintains what among the holders.** That is ADR-0088's derivation, unchanged.
- **Read-side visibility for persons inside Gitlet** (a unit repository a holder may not read). Not planned: the app is the person's boundary, as it is for every app, and sensitive code goes in its own repository with `visibility: unit` for seats.
- **Whether a company runs Gitlet at all.** That is `code.backend`. Where it does not, the grant gives nothing, as `chatlet` gives a company on a vendor's chat nothing.
- **A reviewer the repository's path owners or a revision's facts ask again**, which no principal requests: one who has since lost the app is still asked, lists no notice of it, and holds a gate that needs their approval until an administrator gives them Gitlet or the owners change.
