/**
 * The Inbox — the landing screen, and the one this product was missing.
 *
 * # Two bands, because there are two kinds of claim on a person
 *
 * **Decisions** is what is waiting on somebody: a notice that ASKS something,
 * a coding run parked on a clarification, an engine condition with a remedy.
 * **Notices** is what merely happened to reach them — a task they watch moved,
 * a sprint they are in started.
 *
 * The split is the engine's own, not this screen's: `work_inbox` reports the
 * `primary_reasons` that were APPLIED, defaulted from the person's record, so
 * a company that has re-decided what counts as primary gets its own split
 * here without this file knowing anything about it.
 *
 * # The reason is the opening fact of every row
 *
 * The applier records, per change and per recipient, the ONE reason of twenty
 * under which that person heard about it. Nothing has ever drawn it, and it is
 * the fact no commercial tracker keeps: Linear, Jira and ClickUp can all tell
 * you that you were notified, and none can tell you why. So it opens the row
 * rather than hiding in a tooltip.
 *
 * # Read-only, like everything else
 *
 * Marking a notice read is a WRITE, and it belongs to the person whose inbox
 * it is — through their own assistant, with `mark_inbox`, attributed to them.
 * A button here would write as "the dashboard", which is not a person and
 * cannot be asked why.
 */

import { useMemo } from "react";
import { href, useParam } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Banner, Button, Empty, Panel, Segmented, Skeleton } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { reasonPhrase, reasonWhy } from "~/lib/reasons.ts";
import { useAgents, useConnection, useOrg, useOrgBudget, useSandboxes } from "~/lib/store-hooks.ts";
import { attentionQueue, type Attention } from "~/lib/attention.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import type { WorkInboxNotice } from "~/protocol/index.ts";

/** Which band a notice belongs to, from the split the ENGINE applied. */
function isPrimary(notice: WorkInboxNotice, primary: string[]): boolean {
  return primary.includes(notice.reason);
}

export function Inbox() {
  const viewer = useViewer();
  const now = useNow();
  const org = useOrg();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const budget = useOrgBudget();
  const { connected, authRejected } = useConnection();
  const { data: engine } = useQuery("stream", undefined, { pollMs: 15_000 });

  // THE STATE IS IN THE URL, like every other screen: which band, whether
  // read notices are shown, and which reasons are being looked at.
  const [band, setBand] = useParam("band", "decisions", "section");
  const [state, setState] = useParam("state", "unread");
  const [reason, setReason] = useParam("reason", "");

  const inbox = useQuery(
    "work_inbox",
    viewer.handle
      ? {
          handle: viewer.handle,
          limit: 50,
          unread: state === "unread",
          include_snoozed: state === "snoozed",
          ...(reason ? { reasons: reason } : {}),
        }
      : undefined,
    { enabled: viewer.handle !== "", pollMs: 30_000 },
  );
  usePageCoverage(inbox.data);

  const primary = inbox.data?.primary_reasons ?? [];
  const notices = inbox.data?.notices ?? [];
  const decisions = notices.filter((n) => isPrimary(n, primary));
  const rest = notices.filter((n) => !isPrimary(n, primary));

  // THE ENGINE'S OWN CONDITIONS, in the same list. An alarm the engine raised
  // is a claim on a person exactly as a notice is, and it used to live in a
  // popover behind a pill in the sidebar's foot.
  const index = useMemo(() => indexOrg(org), [org]);
  const attention = useMemo(
    () =>
      attentionQueue({
        agents,
        sandboxes,
        budget,
        engine: engine ?? null,
        seats: index.seats,
        connected,
        authRejected,
        now,
      }),
    [agents, sandboxes, budget, engine, index.seats, connected, authRejected, now],
  );

  // EVERY REASON THAT IS ACTUALLY ON THE PAGE, so the filter offers what the
  // person has rather than the whole vocabulary of twenty.
  const reasons = useMemo(() => {
    const seen = new Map<string, number>();
    for (const n of notices) seen.set(n.reason, (seen.get(n.reason) ?? 0) + 1);
    return [...seen.entries()].sort((a, b) => b[1] - a[1]);
  }, [notices]);

  const shown = band === "decisions" ? decisions : rest;

  return (
    <>
      <PageActions>
        {viewer.handle && (
          <a className="t-link" href={href(["me"])}>
            My work →
          </a>
        )}
      </PageActions>

      {/* THREE VIEWER STATES, three different sentences — and only one of them
          is anybody's fault. The engine half of this screen works for all
          three, which is why the page is not simply locked. */}
      {viewer.anonymous ? (
        <Banner tone="caution">
          No API token is presented, so this browser is nobody. The engine's own conditions are
          below; a person's notices need a credential bound to their seat.
        </Banner>
      ) : viewer.unbound ? (
        <Banner tone="caution">
          This token is <code className="inline">{viewer.operatorID}</code> and no seat claims it.
          Give a human seat <code className="inline">contact.crewlet_operator_id</code> with that
          value and this becomes their inbox.
        </Banner>
      ) : null}

      <PageNote>
        What reached you, and the one reason of twenty it reached you under. Read-only: a notice is
        marked read by the person whose it is, through their own assistant.
      </PageNote>

      {attention.length > 0 && (
        <Panel
          title="Needs a person"
          icon="alert"
          count={attention.length}
          subtitle="Conditions the engine raised — each names what it costs to leave it."
          padding="none"
        >
          <div className="list">
            {attention.map((item) => (
              <AttentionRow key={item.id} item={item} now={now} />
            ))}
          </div>
        </Panel>
      )}

      {viewer.handle && (
        <>
          <div className="toolbar">
            <Segmented
              ariaLabel="Band"
              value={band}
              onChange={setBand}
              options={[
                { value: "decisions", label: `Decisions ${decisions.length}` },
                { value: "notices", label: `Notices ${rest.length}` },
              ]}
            />
            <Segmented
              ariaLabel="State"
              value={state}
              onChange={setState}
              options={[
                { value: "unread", label: "Unread" },
                { value: "all", label: "All" },
                { value: "snoozed", label: "Snoozed" },
              ]}
            />
            <span className="spacer" />
            {reason && (
              <Button size="sm" icon="x" onClick={() => setReason("")}>
                {reasonPhrase(reason)}
              </Button>
            )}
          </div>

          {/* THE REASONS ON THIS PAGE, as a facet row. Counts are over the
              LOADED page and say so: the engine counts no totals here, and a
              facet claiming one would be inventing it. */}
          {reasons.length > 1 && !reason && (
            <div className="facet-row">
              {reasons.map(([name, count]) => (
                <button key={name} className="facet" onClick={() => setReason(name)}>
                  <span className="truncate">{reasonPhrase(name)}</span>
                  <span className="t-num">{count}</span>
                </button>
              ))}
              <span className="t-caption">counts over the page loaded</span>
            </div>
          )}

          {inbox.loading && !inbox.data && <Skeleton rows={5} />}

          <QueryState
            error={inbox.error}
            loading={inbox.loading}
            empty={
              shown.length
                ? undefined
                : {
                    title:
                      band === "decisions"
                        ? "Nothing is waiting on you"
                        : "Nothing else reached you",
                    hint:
                      band === "decisions"
                        ? "A decision is a notice under a reason your record counts as primary — a mention, a question, work assigned to you."
                        : "Everything else the company told you about would appear here: a task you watch moving, a sprint you are in starting.",
                  }
            }
          >
            <div className="list">
              {shown.map((notice) => (
                <NoticeRow key={notice.record_id} notice={notice} now={now} />
              ))}
            </div>
            {inbox.data?.next_cursor && (
              <p className="t-caption">
                More notices exist beyond this page. The engine returns at most 50 at a time.
              </p>
            )}
          </QueryState>

          <p className="t-caption">
            Your assistant marks these read with <code className="inline">mark_inbox</code>, which
            is attributed to you. Nothing on this screen writes.
          </p>
        </>
      )}

      {!viewer.handle && !viewer.loading && attention.length === 0 && (
        <Empty
          icon="inbox"
          title="Nothing needs a person"
          hint="The engine raised no conditions. With a credential bound to a seat, this screen also shows what reached that person."
        />
      )}
    </>
  );
}

/** One engine condition, with where to go about it. */
function AttentionRow({ item, now }: { item: Attention; now: number }) {
  const body = (
    <>
      <span className="attention-icon" data-severity={item.severity}>
        <Icon name={item.icon} size="sm" />
      </span>
      <span className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
        <strong className="t-cell truncate">{item.title}</strong>
        <span className="t-caption">{item.detail}</span>
      </span>
      {item.who && <Badge outline>{item.who}</Badge>}
      {item.at && <span className="t-caption">{relTime(item.at, now)}</span>}
      {item.path && <Icon name="arrowRight" size="sm" />}
    </>
  );
  return item.path ? (
    <a className="thread-entry" href={href(item.path, item.query)}>
      {body}
    </a>
  ) : (
    <div className="thread-entry">{body}</div>
  );
}

/** One notice: the reason first, then what changed. */
function NoticeRow({ notice, now }: { notice: WorkInboxNotice; now: number }) {
  return (
    <div className="thread-entry" style={{ opacity: notice.read ? 0.72 : 1 }}>
      <span className="col" style={{ gap: 2, flex: 1, minWidth: 0 }}>
        <span className="row gap-1">
          {/* THE REASON, FIRST. It is the fact nothing else on this row
              carries, and the one no other tracker records. */}
          <Badge outline title={reasonWhy(notice.reason)}>
            {reasonPhrase(notice.reason)}
          </Badge>
          {notice.addressed && (
            <Badge tone="caution" title="this asks something of you">
              asks
            </Badge>
          )}
          {notice.fallback && (
            <Badge outline title="nobody better was found for this">
              fallback
            </Badge>
          )}
          {notice.subject_key && (
            <a className="mono t-link" href={href(["work", notice.subject_key])}>
              {notice.subject_key}
            </a>
          )}
          <span className="spacer" />
          {notice.at && <span className="t-caption">{relTime(notice.at, now)}</span>}
        </span>
        <span className="t-cell truncate">{notice.excerpt || notice.kind.replace(/_/g, " ")}</span>
      </span>
      {!notice.read && <i className="dot accent" title="unread" />}
    </div>
  );
}
