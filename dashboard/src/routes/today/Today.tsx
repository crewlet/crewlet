/**
 * Today: a person's own first screen — what needs their answer, the work that
 * is theirs, and what reached them.
 *
 * # Why the landing is a person's day rather than the company's
 *
 * The screen everybody landed on used to be the company running: the fleet's
 * sentence, five figures, the turns in flight with their raw tool calls and
 * the spend by team. That is an admin's question, and it moved whole to the
 * console's Overview. A person opening Crewlet asks whether anything needs
 * them, and what is theirs to do — so Today answers that, from three reads
 * every one of which is the person's own: `decisions` (what is put to them),
 * the open work assigned to them, and their unread notices.
 *
 * # Who is reading decides what is drawn
 *
 * Anonymous, it is the sign-in: the company's name and mission — public — and
 * where a key goes. A key no seat links has no day to draw, and says so
 * rather than drawing an empty one that claims to have looked. Only a linked
 * key — a person — is shown their day.
 */

import { useMemo } from "react";
import { Button, ButtonLink, Card, EmptyState, Skeleton } from "@crewlethq/ui";
import { KeyGlyph, UserGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { DecisionRow, decisionSubjects, subjectKey } from "~/components/DecisionRow.tsx";
import { useNow } from "~/lib/clock.ts";
import { plainText } from "~/lib/markdown.ts";
import { relTime } from "~/lib/format.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { adminToAsk, useViewer, type ViewerState } from "~/lib/viewer.ts";
import { requestToken } from "~/protocol/index.ts";
import type { WorkInboxNotice } from "~/protocol/index.ts";
import { AssignedWork } from "~/routes/team/seat/Overview.tsx";

/** How many decisions and notices Today draws before the Inbox has the rest. */
const TODAY_ROWS = 3;

/** How many open tasks the queue card is read from. */
const QUEUE_PAGE = 50;

/** Every read here moves on the scale of a person's attention: half a minute. */
const TODAY_POLL_MS = 30_000;

export function Today() {
  const viewer = useViewer();
  if (viewer.asking) return <Skeleton variant="text" rows={6} label="Finding your day" />;
  if (viewer.anonymous) return <SignIn />;
  if (!viewer.linked) return <Unlinked viewer={viewer} />;
  return <Day viewer={viewer} />;
}

/** The sign-in: the company, as the public projection names it, and a key. */
function SignIn() {
  const org = useOrg();
  return (
    <EmptyState
      icon={<KeyGlyph size={32} />}
      title={org?.name ? `Sign in to ${org.name}` : "Sign in"}
      description={
        <>
          {org?.mission ? <span className="measure">{org.mission}</span> : null} Sign in with the
          access key you were given. It stays in this browser until you sign out.
        </>
      }
      action={
        <Button variant="primary" leadingIcon={<KeyGlyph />} onClick={requestToken}>
          Sign in
        </Button>
      }
    />
  );
}

/** A key no seat links: there is no person, so there is no day to draw. */
function Unlinked({ viewer }: { viewer: ViewerState }) {
  return (
    <EmptyState
      icon={<UserGlyph size={32} />}
      title="Your key isn't linked to you yet"
      description={`You're signed in, but no teammate in the org chart is linked to this key — so there are no questions, work or notices of yours to show. You can still read the company's work, team and pages. Ask ${adminToAsk(viewer.admins)} to link this key to your profile.`}
      action={
        <ButtonLink variant="primary" href={href(["work"])}>
          Browse work
        </ButtonLink>
      }
    />
  );
}

/** A person's day: what is put to them, what is theirs, and what reached them. */
function Day({ viewer }: { viewer: ViewerState }) {
  const now = useNow();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const seat = index.byHandle.get(viewer.handle);
  const decisions = useQuery("decisions", undefined, { pollMs: TODAY_POLL_MS });
  const work = useQuery(
    "work_items",
    {
      assignee: viewer.handle,
      status_group: "not_started,active",
      subtasks: "separate",
      sort: "-priority,updated",
      limit: QUEUE_PAGE,
    },
    { pollMs: TODAY_POLL_MS },
  );
  const notices = useQuery(
    "work_inbox",
    { handle: viewer.handle, unread: true, primary_only: true, snoozed: "exclude", limit: 20 },
    { pollMs: TODAY_POLL_MS },
  );
  // WHAT IS PUT TO THIS PERSON: the asks and the coding runs parked on a
  // question to them — the engine's own `decisions`, and nothing about how
  // the machine is doing. A seat stopped on its budget is an admin's to
  // resolve, and it is the console's.
  const subjects = decisionSubjects(decisions.data ?? null, []);
  const reached = (notices.data?.notices ?? []).filter((n) => !n.read).slice(0, TODAY_ROWS);
  return (
    <div className="col gap-4">
      <Card padding="none" className="home-card">
        <Card.Header
          actions={
            <a className="t-link" href={href(["inbox"])}>
              Open Inbox
            </a>
          }
        >
          <Card.Title as="h3">Needs you</Card.Title>
        </Card.Header>
        <QueryState error={decisions.error} loading={decisions.loading && !decisions.data}>
          {subjects.length === 0 ? (
            <p className="home-card-note">Nothing is waiting on you.</p>
          ) : (
            <ul className="decision-list">
              {subjects.slice(0, TODAY_ROWS).map((subject) => (
                <DecisionRow
                  key={subjectKey(subject)}
                  subject={subject}
                  decider={{ handle: viewer.handle }}
                  now={now}
                />
              ))}
            </ul>
          )}
        </QueryState>
      </Card>
      {seat && <AssignedWork seat={seat} work={work} now={now} />}
      <Card padding="none" className="home-card">
        <Card.Header
          actions={
            <a className="t-link" href={href(["inbox"])}>
              All of them
            </a>
          }
        >
          <Card.Title as="h3">What reached you</Card.Title>
        </Card.Header>
        <QueryState error={notices.error} loading={notices.loading && !notices.data}>
          {reached.length === 0 ? (
            <p className="home-card-note">Nothing new has reached you.</p>
          ) : (
            <ul className="today-notices">
              {reached.map((notice) => (
                <Notice key={notice.record_id} notice={notice} now={now} />
              ))}
            </ul>
          )}
        </QueryState>
      </Card>
    </div>
  );
}

/** One unread notice: what it says, on which task, and when — leading to the task. */
function Notice({ notice, now }: { notice: WorkInboxNotice; now: number }) {
  const said = plainText(notice.excerpt ?? "");
  const key = notice.subject_key ?? "";
  const body = (
    <>
      {key && <span className="work-key mono">{key}</span>}
      <span className="truncate">{said || notice.kind}</span>
      <span className="t-caption">{relTime(notice.at, now)}</span>
    </>
  );
  return (
    <li>
      {key ? (
        <a className="today-notice" href={href(["work", key])}>
          {body}
        </a>
      ) : (
        <span className="today-notice">{body}</span>
      )}
    </li>
  );
}
