/**
 * Company › Atlas › Sources: each integration the atlas reads for the
 * company, and how its last read went. The band's "Where it comes from" leads
 * here. Every figure is the collect duty's own standing for the source, off
 * the coordination store: when it was last read, what that read cost and left
 * out, what it was refused, and for the servers, each server's own last read.
 *
 * "Refresh now" asks for a read sooner than the next scheduled one. The
 * engine holds the floor between two reads of one source, so the button says
 * when it can be pressed again rather than letting a press be refused.
 */

import {
  EmptyState,
  FindingList,
  Link,
  PageNote,
  Skeleton,
  StatCard,
  StatGroup,
  StatusCard,
  StatusDot,
  Table,
  Text,
  type Finding,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import type {
  AtlasSourceScope,
  AtlasSourceStanding,
  AtlasSourceState,
  AtlasSourcesAnswer,
} from "~/contract/atlas.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDuration, plural, relTime } from "~/lib/format.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { ATLAS_POLL_MS } from "./filters.ts";
import { sourceName } from "./words.ts";
// THE ATLAS'S OWN SHEET, for a reader who opens this page first.
import "~/styles/atlas.css";

const NO_PARAMS = {};

/** What each state of a source is called, and the tone its tag takes. */
export const SOURCE_STATES: Readonly<
  Record<AtlasSourceState, { label: string; tone: "neutral" | "success" | "warning" | "danger" }>
> = {
  never: { label: "Not read yet", tone: "neutral" },
  read: { label: "Read", tone: "success" },
  partial: { label: "Partly read", tone: "warning" },
  failed: { label: "Last read failed", tone: "danger" },
};

/** When a source was read and when it is read next, as one line under its name. */
export function readLine(source: AtlasSourceStanding, now: number): string {
  if (!source.last_attempt_at) {
    return "It is read within a minute, once its integration holds the credential the read needs.";
  }
  const read =
    source.state === "failed"
      ? source.last_complete_at
        ? `Tried ${relTime(source.last_attempt_at, now)}, last read whole ${relTime(source.last_complete_at, now)}`
        : `Tried ${relTime(source.last_attempt_at, now)}, never read whole`
      : `Read ${relTime(source.last_attempt_at, now)}`;
  const next = source.next_read_at ? `next read ${relTime(source.next_read_at, now)}` : "due now";
  return `${read}, ${next}.`;
}

/** Why "Refresh now" would change nothing, or undefined where it may be pressed. */
export function refreshBlocked(source: AtlasSourceStanding, now: number): string | undefined {
  if (source.refresh_requested_at) return "A read is already asked for";
  const at = source.refreshable_at ? Date.parse(source.refreshable_at) : 0;
  if (at > now)
    return `Read moments ago. It can be read again ${relTime(source.refreshable_at, now)}`;
  return undefined;
}

/** What a source was refused and how its last read failed, as findings. */
function findingsOf(source: AtlasSourceStanding): Finding[] {
  const out: Finding[] = [];
  if (source.state === "failed") {
    out.push({
      key: "fault",
      tone: "danger",
      message:
        source.last_error ??
        "The read did not finish. An administrator is shown why. The atlas keeps what the last whole read found.",
    });
  }
  for (const one of source.unread) {
    out.push({
      key: `unread:${one.part}`,
      tone: "warning",
      location: one.part,
      message: one.detail ?? "This part was not read. What an earlier read found there stands.",
    });
  }
  if (source.unread_overflow > 0) {
    out.push({
      key: "unread:more",
      tone: "warning",
      message: `${plural(source.unread_overflow, "more part")} not read.`,
    });
  }
  return out;
}

/** One server's standing in a word. */
function scopeStanding(scope: AtlasSourceScope) {
  const unread = scope.unread ?? 0;
  return unread === 0 ? (
    <StatusDot tone="success" wrap>
      Read
    </StatusDot>
  ) : (
    <StatusDot tone="warning" wrap>
      {plural(unread, "part")} not read
    </StatusDot>
  );
}

function SourceCard({ source, now }: { source: AtlasSourceStanding; now: number }) {
  const refresh = useAct("atlas_refresh");
  const name = sourceName(source.kind);
  const state = SOURCE_STATES[source.state];
  const findings = findingsOf(source);
  const servers = source.kind === "ssh";
  // A SOURCE NEVER READ HAS NO BODY: its card is its name, its state and its action.
  const body =
    Boolean(source.last_complete_at) ||
    Boolean(source.refresh_requested_at) ||
    findings.length > 0 ||
    source.missing > 0 ||
    source.purged > 0;
  return (
    <StatusCard
      title={name}
      subtitle={readLine(source, now)}
      subtitlePlacement="below"
      status={{ label: state.label, tone: state.tone }}
      actions={
        <WriteButton
          write={refresh}
          size="small"
          variant="secondary"
          blocked={refreshBlocked(source, now)}
          onPress={() =>
            void refresh.run({ integration: source.kind }, { done: `Asked for a read of ${name}` })
          }
        >
          Refresh now
        </WriteButton>
      }
    >
      {body && (
        <div className="atlas-source">
          {source.last_complete_at && (
            <StatGroup columns={4} flush aria-label={`${name}, its last whole read`}>
              <StatCard flush label="Entries" value={source.entries.toLocaleString()} />
              <StatCard
                flush
                label={servers ? "Commands run" : "Vendor calls"}
                value={source.calls.toLocaleString()}
              />
              <StatCard
                flush
                label="Values withheld"
                value={source.withheld.toLocaleString()}
                sub="Shaped like a credential. Counted, never shown."
              />
              <StatCard
                flush
                label="Changes recorded"
                value={source.recorded.toLocaleString()}
                sub={
                  source.retired > 0
                    ? `${plural(source.retired, "entry", "entries")} retired`
                    : undefined
                }
              />
            </StatGroup>
          )}
          {source.refresh_requested_at && (
            <Text variant="body" tone="secondary">
              {source.refresh_requested_by
                ? `${source.refresh_requested_by} asked for a read ${relTime(source.refresh_requested_at, now)}.`
                : `A read was asked for ${relTime(source.refresh_requested_at, now)}.`}{" "}
              It is made within a minute.
            </Text>
          )}
          {findings.length > 0 && (
            <FindingList
              label={`What the last read of ${name} could not do`}
              toneLabels={{ danger: "Failed", warning: "Not read" }}
              density="compact"
              findings={findings}
            />
          )}
          {source.missing > 0 && (
            <Text variant="body" tone="secondary">
              {plural(source.missing, "entry was", "entries were")} not in the last read. An entry
              is retired only when a second whole read, a day later, does not list it either.
            </Text>
          )}
          {source.purged > 0 && (
            <Text variant="body" tone="secondary">
              {plural(source.purged, "entry", "entries")} an administrator purged{" "}
              {source.purged === 1 ? "was" : "were"} seen again and left out.
            </Text>
          )}
          {servers &&
            (source.scopes.length > 0 ? (
              <Table
                caption="Each server the engine reads"
                captionHidden
                headers={["Server", "Read whole", "Standing"]}
                data={source.scopes.map((scope) => [
                  <span key="id" className="mono">
                    {scope.id}
                  </span>,
                  scope.last_read_at ? relTime(scope.last_read_at, now) : "Never",
                  scopeStanding(scope),
                ])}
              />
            ) : (
              source.last_complete_at && (
                <Text variant="body" tone="secondary">
                  No server has its inventory switched on. A server is read only once it opts in,
                  under <Link href={href(["company", "integrations", "ssh"])}>its integration</Link>
                  .
                </Text>
              )
            ))}
        </div>
      )}
    </StatusCard>
  );
}

/** A cadence in words: "hour", "2 hours", "5 minutes", and otherwise the duration as figures. */
export function span(seconds: number): string {
  if (seconds > 0 && seconds % 3600 === 0) {
    return seconds === 3600 ? "hour" : `${seconds / 3600} hours`;
  }
  if (seconds > 0 && seconds % 60 === 0) {
    return seconds === 60 ? "minute" : `${seconds / 60} minutes`;
  }
  return fmtDuration(seconds * 1000);
}

/** How often the sources are read, and how soon one can be read again by hand. */
function cadence(answer: AtlasSourcesAnswer): string {
  const every = span(answer.refresh_interval_seconds);
  const floor = span(answer.refresh_floor_seconds);
  return `Each source is read every ${every}. Refresh now reads one sooner, at most once every ${floor}.`;
}

export function SourcesPage() {
  const now = useNow();
  const sources = useQuery("atlas_sources", NO_PARAMS, { pollMs: ATLAS_POLL_MS });
  const answer = sources.data;
  return (
    <div className="atlas-screen">
      <PageNote>
        The integrations the atlas reads for the company, and how each one’s last read went. A read
        takes in the whole vendor and records only what changed. Everything else on the map comes
        from the settings, the engine’s own records, people and agents.
      </PageNote>
      {!answer && sources.loading ? (
        <Skeleton variant="text" rows={6} label="Reading the sources" />
      ) : (
        <QueryState
          error={sources.error}
          refusal={sources.refusal}
          detail={sources.detail}
          loading={sources.loading}
        >
          {answer &&
            (answer.sources.length === 0 ? (
              <EmptyState
                title="No source is connected"
                description="The atlas reads a vendor whole once its integration is connected, and a server once it opts in. Connect one under Company, Integrations."
              />
            ) : (
              <>
                <Text variant="body" tone="secondary">
                  {cadence(answer)}
                </Text>
                <div className="atlas-sources">
                  {answer.sources.map((source) => (
                    <SourceCard key={source.kind} source={source} now={now} />
                  ))}
                </div>
              </>
            ))}
        </QueryState>
      )}
    </div>
  );
}
