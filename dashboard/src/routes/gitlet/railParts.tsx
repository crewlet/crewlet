/**
 * The parts a Gitlet rail is made of, and its pushes by day and who pushes here: shared by a
 * repository's rail and the rail beside Gitlet › Pushes, which live in different chunks.
 */

import { useId, type ReactNode } from "react";
import { Skeleton } from "@crewlethq/ui";
import { useNow } from "~/lib/clock.ts";
import { plural } from "~/lib/format.ts";
import { dateTimeFormat } from "~/lib/formatters.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { CodePusher, CodePushers, CodeRepoAnswer } from "~/protocol/index.ts";
import { PUSHER_DAYS, PushSpark, SPARK_DAYS, useDayBounds } from "./activity.tsx";
import { REFUSED_CODES, refusalSentence } from "./repo/marks.ts";
import { pusherRest } from "./repo/rail.ts";
import { Face, GITLET, useWhoName } from "./ui.tsx";
import "~/styles/gitlet.css";

/** The pushers it names; the rest are one line. */
const PUSHER_ROWS = 6;

const DAY_MS = 86_400_000;

/** A date in UTC, as the pushers' whole UTC days are counted. */
const utcDay = (ms: number) =>
  dateTimeFormat(undefined, "UTC", { month: "short", day: "2-digit" }).format(ms);

/** Why a section's read drew nothing: the engine's refusal in its words, or a failed read. */
export function unreadLine(read: QueryResult<unknown>, refused: string, failed: string): string {
  if (!REFUSED_CODES.has(read.error ?? "")) return failed;
  const sentence = refusalSentence(read);
  return sentence ? `${refused}: ${sentence.replace(/\.$/, "")}.` : `${refused}.`;
}

/** One section: its head row (a title, a count where known, a caption or a link), then its body. */
export function Section({
  title,
  count,
  end,
  children,
}: {
  title: string;
  count?: number | undefined;
  end?: ReactNode;
  children: ReactNode;
}) {
  const id = useId();
  return (
    <section className="code-repo-rail__sec" aria-labelledby={id}>
      <Head id={id} title={title} count={count} end={end} />
      {children}
    </section>
  );
}

export function Head({
  id,
  title,
  count,
  end,
}: {
  id: string;
  title: string;
  count?: number | undefined;
  end?: ReactNode;
}) {
  return (
    <div className="code-repo-rail__head">
      <h3 id={id} className="code-repo-rail__title">
        {title}
      </h3>
      {count !== undefined && <span className="code-repo-rail__count">{count}</span>}
      {end && <span className="code-repo-rail__end">{end}</span>}
    </div>
  );
}

/** A section's one line: none to list, or why nothing was read. */
export function Note({ children }: { children: ReactNode }) {
  return <p className="code-repo-rail__note">{children}</p>;
}

/** A section's quiet caption at the head's end. */
export function Caption({ title, children }: { title?: string; children: ReactNode }) {
  return (
    <span className="code-repo-rail__caption" title={title}>
      {children}
    </span>
  );
}

/** The facts read on the reader's days: who pushed over PUSHER_DAYS, and a count a day. */
export interface PushActivityRead {
  bounds: readonly string[];
  read: QueryResult<CodeRepoAnswer>;
}

/** [PushActivityRead] for a repository, asked once by the screen that draws it. */
export function usePushActivity(name: string, pollMs: number): PushActivityRead {
  const bounds = useDayBounds(PUSHER_DAYS);
  const read = useCodeQuery(
    "code_repo",
    { name, repo_facts: true, day_bounds: bounds.join(",") },
    { enabled: bounds.length > 0, pollMs },
  );
  return { bounds, read };
}

/**
 * Who pushes here and the spark: one read, drawn as one section where it was not answered.
 * `sparkFirst` draws the spark above the pushers, as the pushes' own rail does.
 */
export function PushActivity({
  activity,
  sparkFirst = false,
}: {
  activity: PushActivityRead;
  sparkFirst?: boolean;
}) {
  const { bounds, read } = activity;
  const now = useNow();
  const titleId = useId();
  const facts = read.data?.repo;
  if (read.loading && !facts) {
    const pushers = (
      <Section key="pushers" title="Who pushes here">
        <Skeleton variant="text" rows={3} label="Loading who pushes here" />
      </Section>
    );
    const spark = (
      <Section key="spark" title="Pushes">
        <Skeleton
          variant="text"
          rows={1}
          rowHeight="var(--spacing-8)"
          label="Loading the pushes by day"
        />
      </Section>
    );
    return <>{sparkFirst ? [spark, pushers] : [pushers, spark]}</>;
  }
  if (read.error && !facts) {
    return (
      <Section title="Pushes">
        <Note>
          {unreadLine(read, "Pushes by day are unavailable", "Pushes by day could not be read.")}
        </Note>
      </Section>
    );
  }
  if (!facts?.pushers || !facts.spark) {
    return (
      <Section title="Pushes">
        <Note>Who pushes here and pushes by day are not counted on this fleet yet.</Note>
      </Section>
    );
  }
  const counts = facts.spark.slice(-SPARK_DAYS);
  const starts = bounds.slice(facts.spark.length - counts.length, facts.spark.length);
  const pushers = <Pushers key="pushers" pushers={facts.pushers} />;
  const spark = (
    <section key="spark" className="code-repo-rail__sec" aria-labelledby={titleId}>
      {counts.every((n) => n === 0) ? (
        <>
          <Head id={titleId} title="Pushes" end={<Caption>last 14 days</Caption>} />
          <Note>No push in the last 14 days.</Note>
        </>
      ) : (
        <PushSpark
          counts={counts}
          starts={starts}
          now={now}
          caption={<Head id={titleId} title="Pushes" end={<Caption>last 14 days</Caption>} />}
        />
      )}
    </section>
  );
  return <>{sparkFirst ? [spark, pushers] : [pushers, spark]}</>;
}

/** The pushers on whole UTC days, most pushes first, and one line for the rest. */
export function Pushers({ pushers }: { pushers: CodePushers }) {
  const top = (pushers.top ?? []).slice(0, PUSHER_ROWS);
  const rest = pusherRest(pushers, PUSHER_ROWS);
  const from = Date.parse(pushers.from);
  const until = Date.parse(pushers.until) - DAY_MS;
  const window =
    Number.isFinite(from) && Number.isFinite(until)
      ? `Counted on whole UTC days, ${utcDay(from)} to ${utcDay(until)}. `
      : "Counted on whole UTC days. ";
  const caption = (
    <Caption
      title={`${window}A merge, a queue landing, an import and a mirror's sync count as Gitlet's; an expiry is not a push.`}
    >
      {`${PUSHER_DAYS} days, UTC`}
    </Caption>
  );
  return (
    <Section title="Who pushes here" end={caption}>
      {top.length === 0 ? (
        <Note>Nobody pushed in the last {PUSHER_DAYS} days.</Note>
      ) : (
        <ul className="code-repo-rail__list">
          {top.map((p) => (
            <li key={`${p.kind}:${p.handle ?? ""}`} className="code-repo-rail__pusher">
              <PusherName pusher={p} />
              <span className="code-repo-rail__pushes">{p.pushes.toLocaleString()}</span>
            </li>
          ))}
          {rest && (
            <li className="code-repo-rail__rest">
              and {plural(rest.pushers, "more", "more")}, {plural(rest.pushes, "push", "pushes")}
            </li>
          )}
        </ul>
      )}
    </Section>
  );
}

/** A pusher's face and name: Gitlet by its mark, a seat or a person by their seat. */
export function PusherName({ pusher }: { pusher: CodePusher }) {
  const whoName = useWhoName();
  const who = pusher.kind === "gitlet" ? GITLET : (pusher.handle ?? "");
  if (pusher.kind !== "gitlet" && pusher.kind !== "agent" && pusher.kind !== "person") {
    return <span className="code-repo-rail__who">{pusher.handle ?? pusher.kind}</span>;
  }
  const name = whoName(who);
  return (
    <span className="code-repo-rail__who">
      <Face who={who} size="sm" />
      <span className="code-repo-rail__name" title={name}>
        {name}
      </span>
    </span>
  );
}
