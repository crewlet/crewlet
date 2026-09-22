/**
 * All work — the company's own, as a list you can arrange.
 *
 * # The list is the page
 *
 * It used to open on four numbers, a ranked bar chart of every project, six
 * tabs mixing shapes with saved views, a bar of eleven controls, the rows, and
 * a change feed under them — five questions on one scroll, of which a reader
 * wanted one. The overview is a page of its own now (`#/work/projects`), the
 * feed is a page of its own (`#/work/history`), and what is left here is the
 * work: the saved views, the bar, and the rows.
 *
 * # What this screen is NOT
 *
 * It is not a second tracker. A company running Jira has none of these
 * questions registered, and this screen says so rather than drawing an empty
 * board: an operator who wired Jira and then found a blank Crewlet board would
 * reasonably conclude their integration was broken.
 *
 * # An unhydrated projection is not an empty company
 *
 * Every row here comes from this node's own copy of the fleet's record, which
 * is the same copy a seat's tools read — so an operator and an agent looking at
 * one item see one item. A node that has not finished its boot reconcile
 * REFUSES rather than answering empty, and `QueryState` renders that refusal as
 * "still catching up". Drawing it as "no work" would be an answer somebody acts
 * on, by filing the duplicate.
 */

import { href } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { ItemsView, NoWorkYet } from "./ItemsView.tsx";

export function Work() {
  // THE PROJECT LIST, FOR ONE SENTENCE. "This company has filed nothing" is
  // the project list's answer, so it is gated on the project list's own state
  // — gated on the ITEMS query instead, a `work_projects` that was refused or
  // had simply not landed drew a positive statement that the company had
  // nothing, over a read that failed. A reader acts on that, by filing the
  // duplicate.
  const projects = useQuery("work_projects", { limit: 1 }, { pollMs: 120_000 });
  const none = (projects.data?.projects ?? []).length === 0;

  return (
    <>
      <PageActions>
        {/* REAL ANCHORS rather than buttons that navigate: these leave the
            list, so they are middle-clickable like every other way out of a
            screen, and they go through the router's own history rules instead
            of around them. */}
        <a className="t-link" href={href(["work", "projects"])}>
          Projects →
        </a>
        <a className="t-link" href={href(["work", "history"])}>
          History →
        </a>
      </PageActions>
      <PageNote>
        The company&rsquo;s own work — every project, in one list. Read-only here: work is filed and
        moved by the seats themselves, so every change is attributed to somebody.
      </PageNote>

      <div className="work-main">
        <ItemsView />
        {/* ONLY ONCE THE LIST HAS ANSWERED. `QueryState` puts a refusal ahead
            of the empty state, and this draws nothing at all while the read is
            in flight or once a project exists. */}
        <QueryState error={projects.error} loading={projects.loading}>
          {none && projects.data ? <NoWorkYet /> : null}
        </QueryState>
      </div>
    </>
  );
}
