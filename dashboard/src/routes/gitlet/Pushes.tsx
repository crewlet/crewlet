/**
 * Gitlet › Pushes and a repository's Pushes tab: one repository's pushes, newest first, as a feed
 * of sentences on the reader's days, each day counted by the engine (`push_days`).
 */

import { Fragment, memo, useId, useMemo, useState, type ReactNode } from "react";
import {
  AsideLayout,
  AvatarStack,
  Button,
  Callout,
  EmptyState,
  IconTile,
  RelativeTime,
  Select,
  Skeleton,
  Tag,
  type SelectOption,
} from "@crewlethq/ui";
import {
  BotGlyph,
  CalendarGlyph,
  GitBranchGlyph,
  KeyGlyph,
  PackageGlyph,
  ShieldGlyph,
  TagGlyph,
  UploadGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { AgoTime } from "~/components/AgoTime.tsx";
import { QueryState } from "~/components/common.tsx";
import { useClock, useNow } from "~/lib/clock.ts";
import {
  PUSH_HISTORY_DAYS,
  commandWord,
  isRepoName,
  omittedOf,
  refName,
  shortOID,
  type OutcomeCount,
} from "~/lib/code.ts";
import { fmtBytes, fmtCount, fmtDate, fmtDateTime, plural, readerDay } from "~/lib/format.ts";
import { zone } from "~/lib/prefs.ts";
import { useOrgIndex, useSeatBadgeOf } from "~/lib/seats.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import {
  queryErrorCode,
  type CodePushCommand,
  type CodePushers,
  type CodePushRow,
  type CodePushesAnswer,
} from "~/protocol/index.ts";
import { spellPosition } from "~/protocol/session.ts";
import { seatBadge } from "~/ui/SeatAvatar.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { ChangeRefPill } from "./ChangeRow.tsx";
import { CodeList, useDayGroups, type ListDay } from "./CodeList.tsx";
import { REPO_PARAM, commitHref, compareHref, repoHref } from "./paths.ts";
import {
  credentialTag,
  dayAside,
  dayCounts,
  pusherParams,
  pushSubject,
  pushWords,
  type PushPart,
} from "./pushWords.ts";
import { PUSHER_DAYS } from "./activity.tsx";
import { PushesRail } from "./PushesRail.tsx";
import { usePushActivity } from "./railParts.tsx";
import { REFUSED_CODES, refusalSentence } from "./repo/marks.ts";
import { RepoPicker } from "./RepoPicker.tsx";
import { useGitletSectionCounts } from "./sectionCounts.ts";
import { BranchChip, Face, GITLET, useWhoName } from "./ui.tsx";
import { WHEN, dayBoundsFor, sinceOf } from "./when.ts";
import "~/styles/gitlet.css";

/** How often the newest page is read again: the code log has no push to the dashboard. */
const PUSHES_POLL_MS = 60_000;

const pushesOf = (a: CodePushesAnswer) => a.pushes ?? [];
const pushIdOf = (p: CodePushRow) => p.push_id;
const pushesAfter = (a: CodePushesAnswer) => a.next ?? "";
const landedAt = (p: CodePushRow) => p.at;

/** How each push was built, in a chip's words (`gitlet.PushVias`); a newer way said as it is. */
const VIA_WORDS: Readonly<Record<string, string>> = {
  git: "with git",
  engine: "via the engine",
  merge: "a change merged",
  queue: "its queue landed it",
  import: "imported",
  mirror: "a mirror's sync",
  expiry: "an idle branch expired",
};

export function Pushes() {
  useGitletSectionCounts();
  const [repo, setRepo] = useParam(REPO_PARAM, "", "filter");
  const picker = <RepoPicker size="sm" repo={repo} onChoose={setRepo} />;
  if (repo !== "" && isRepoName(repo)) {
    return <PushHistory repo={repo} toolbarLead={picker} rail />;
  }

  return (
    <>
      <PageToolbar>{picker}</PageToolbar>
      {repo === "" ? (
        <EmptyState
          icon={<UploadGlyph size={32} />}
          title="Choose a repository"
          description="A push history is kept with its repository: choose one above, or open one from Repositories."
        />
      ) : (
        <Callout variant="warning">
          “{repo}” is not a repository’s name: one is lowercase letters, digits and hyphens,
          beginning and ending with a letter or a digit, at most 63 in all.
        </Callout>
      )}
    </>
  );
}

/** A day's heading: Today, Yesterday, or its date. */
const dayHeading = (day: ListDay | null, first: CodePushRow) =>
  day === null
    ? "Undated"
    : day.ago === 0
      ? "Today"
      : day.ago === 1
        ? "Yesterday"
        : fmtDate(first.at);

/** The reader's day a push landed on, or "" where its instant does not parse. */
function dayOf(p: CodePushRow): string {
  const at = Date.parse(p.at);
  return Number.isNaN(at) ? "" : readerDay(at);
}

/** The history refused for good (an older fleet refusing a key it lacks), in the engine's words. */
function historyRefused(error: string | null, refusal: unknown, detail?: string): string | null {
  if (!error || !REFUSED_CODES.has(error) || error === "unauthorized" || error === "not_found") {
    return null;
  }
  // A NODE CATCHING UP is QueryState's to say: waiting clears it.
  const retry = (refusal as { retryAfter?: number } | null)?.retryAfter ?? 0;
  if (error === "unavailable" && retry > 0) return null;
  const sentence = refusalSentence({
    detail,
    refusal: refusal as Parameters<typeof refusalSentence>[0]["refusal"],
  });
  const lead = "Pushes are unavailable on this fleet yet";
  return sentence ? `${lead}: ${sentence.replace(/\.$/, "")}.` : `${lead}.`;
}

/**
 * One repository's pushes, newest first, a page at a time, under day headings, narrowed on the
 * engine by who pushed (`by=`) and when (`when=`). `repo` is a name a repository can have; `rail`
 * draws the pushers and protection beside it, and the total in the toolbar.
 */
export function PushHistory({
  repo,
  toolbarLead,
  rail = false,
}: {
  repo: string;
  toolbarLead?: ReactNode;
  rail?: boolean;
}) {
  const [by, setBy] = useParam("by", "", "filter");
  const [when, setWhen] = useParam("when", "", "filter");
  const nav = useNavigator();
  const whoName = useWhoName();
  // THE READER'S DAY, read again only when it turns, so the first page's key holds all day.
  const today = useClock((t) => readerDay(t));
  const inZone = zone();
  const activity = usePushActivity(repo, PUSHES_POLL_MS);
  const rules = useCodeQuery(
    "code_repo",
    { name: repo, gates: true },
    { enabled: rail, pollMs: PUSHES_POLL_MS },
  );
  const total = rail ? rules.data?.repo?.pushes : undefined;
  const seats = usePusherChoices(activity.read.data?.repo?.pushers, by);
  const kind = by === "" ? "" : (pusherParams(by, seats.kindOf).pusher_kind ?? "");
  const since = sinceOf(when, today);
  const base = useMemo(
    () => ({
      name: repo,
      brief: true,
      ...(kind ? { pusher_kind: kind } : {}),
      ...(kind && kind !== "gitlet" ? { pusher: by } : {}),
      ...(since ? { since } : {}),
    }),
    [repo, kind, by, since],
  );
  // THE DAY COUNTS RIDE THE FIRST PAGE, one snapshot with its rows; never beside a pusher.
  const bounds = useMemo(
    () => (by === "" ? dayBoundsFor(when, today, inZone) : []),
    [by, when, today, inZone],
  );
  const params = useMemo(
    () => (bounds.length > 0 ? { ...base, day_bounds: bounds.join(",") } : base),
    [base, bounds],
  );
  const first = useCodeQuery("code_pushes", params, { pollMs: PUSHES_POLL_MS });
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_pushes", { ...base, cursor }),
    pushesOf,
    pushIdOf,
    pushesAfter,
  );
  const rows = paged.items;
  const pushDays = first.data?.push_days;
  const counted = useMemo(() => dayCounts(bounds, pushDays), [bounds, pushDays]);
  const runs = useDayGroups(rows, landedAt, dayHeading);
  const groups = useMemo(() => {
    const loaded = new Map<string, number>();
    for (const p of rows) loaded.set(dayOf(p), (loaded.get(dayOf(p)) ?? 0) + 1);
    return runs.map((g) => {
      const day = dayOf(g.rows[0]!);
      if (day === "") return g;
      const aside = dayAside(loaded.get(day) ?? 0, counted.get(day), fmtDate(g.rows[0]!.at));
      return { ...g, aside: <span title={aside.title}>{aside.text}</span> };
    });
  }, [rows, runs, counted]);
  const refused = historyRefused(paged.error, paged.refusal, first.detail);
  const filtered = by !== "" || when !== "";
  const narrowed = `${by ? ` by ${by === "gitlet" ? "Gitlet" : whoName(by)}` : ""}${
    when ? ` ${WHEN.find((w) => w.value === when)?.words ?? when}` : ""
  }`;
  const scannedTo = paged.more ? paged.answers.at(-1)?.push_scan?.to : undefined;
  const clear = (
    <Button size="small" variant="secondary" onClick={() => nav.filter({ by: null, when: null })}>
      Clear filters
    </Button>
  );

  const toolbar = (
    <PageToolbar>
      {toolbarLead}
      <Select
        size="sm"
        width="auto"
        value={by}
        onChange={(value) => setBy(String(value))}
        options={seats.options}
        ariaLabel="Pushed by"
        searchable
        active={by !== ""}
      />
      <span title="By the time each push landed.">
        <Select
          size="sm"
          width="auto"
          value={when}
          onChange={(value) => setWhen(String(value))}
          options={WHEN.map((w) => ({
            value: w.value,
            label: w.label,
            leading: <CalendarGlyph size="sm" />,
          }))}
          ariaLabel="Pushed"
          active={when !== ""}
        />
      </span>
      {total !== undefined && (
        <span
          className="code-pushes-total"
          title={`Every push it has had; the history keeps the last ${PUSH_HISTORY_DAYS} days.`}
        >
          <strong>{fmtCount(total)}</strong> {total === 1 ? "push" : "pushes"} to {repo}
        </span>
      )}
    </PageToolbar>
  );

  if (first.error === "not_found") {
    return (
      <>
        {toolbar}
        <EmptyState
          icon={<UploadGlyph size={32} />}
          title={`No repository named “${repo}”`}
          description={first.detail ?? "It is not declared, or it has been purged."}
        />
      </>
    );
  }
  const feed = (
    <>
      {paged.loading && rows.length === 0 && refused === null && (
        <Skeleton variant="text" rows={6} label={`Loading the pushes to ${repo}`} />
      )}
      {refused !== null ? (
        <div className="code-pushes-refused">
          <Callout variant="warning">{refused}</Callout>
          {filtered && clear}
        </div>
      ) : (
        <QueryState
          error={paged.error}
          refusal={paged.refusal}
          detail={first.detail}
          loading={paged.loading}
          empty={
            rows.length > 0 || paged.more
              ? undefined
              : filtered
                ? {
                    title: `No push${narrowed}`,
                    hint: `Clear the filters to read every push to ${repo}.`,
                    action: clear,
                  }
                : {
                    title: `Nobody has pushed to ${repo} yet`,
                    hint: "A push is listed once its record applies: the refs it moved, who pushed and what it brought.",
                  }
          }
        >
          <div className="code-pushes-feed">
            <CodeList<CodePushRow>
              groups={groups}
              look="timeline"
              spine={UploadGlyph}
              rowKey={pushIdOf}
              aria-label={`Pushes to ${repo}`}
              lead={(p) => <PushFace push={p} />}
              title={(p) => <PushSentenceLine push={p} />}
              titleAfter={(p) => <ChangeTitle push={p} />}
              meta={(p) => <PushChips push={p} />}
              facts={(p) => <PushFacts push={p} />}
              below={(p) => <PushBelow push={p} repo={repo} />}
              paged={paged}
              moreLabel="Load older pushes"
              foot={footOf(rows.length, paged.more, narrowed, scannedTo)}
            />
          </div>
        </QueryState>
      )}
    </>
  );
  return (
    <>
      {toolbar}
      {rail ? (
        <AsideLayout
          fit="page"
          sticky
          asideLabel={`Pushes to ${repo}`}
          aside={<PushesRail repo={repo} activity={activity} rules={rules} />}
        >
          {feed}
        </AsideLayout>
      ) : (
        feed
      )}
    </>
  );
}

/**
 * Under the rows: how many are loaded (and narrowed how), how far a narrowed read looked, and at
 * the end how long the history keeps.
 */
function footOf(count: number, more: boolean, narrowed: string, scannedTo?: string): ReactNode {
  const loaded =
    count === 0 ? "" : `${plural(count, "push", "pushes")}${narrowed} loaded, newest first`;
  if (scannedTo) {
    return (
      <>
        {loaded && `${loaded}. `}Looked back to <AgoTime value={scannedTo} />; Load older looks
        further.
      </>
    );
  }
  if (count === 0) return null;
  if (more || narrowed) return loaded;
  return `${loaded}. That is every push the history keeps: the last ${PUSH_HISTORY_DAYS} days.`;
}

/** Whom the seat filter offers: who pushed here, most first, then every other seat of the chart. */
function usePusherChoices(pushers: CodePushers | undefined, by: string) {
  const index = useOrgIndex();
  const badge = useSeatBadgeOf();
  return useMemo(() => {
    const top = pushers?.top ?? [];
    const kinds = new Map(
      top.map((p) => [p.kind === "gitlet" ? "gitlet" : (p.handle ?? ""), p.kind]),
    );
    const kindOf = (handle: string) =>
      kinds.get(handle) ??
      (index.byHandle.has(handle)
        ? index.byHandle.get(handle)!.kind === "human"
          ? "person"
          : "agent"
        : undefined);
    const faces = top
      .filter((p) => p.kind !== "gitlet" && p.handle)
      .slice(0, 3)
      .map((p) => {
        const seat = badge(p.handle!);
        return { id: p.handle!, ...seatBadge(seat.name || p.handle!, seat.kind, seat.avatar) };
      });
    const option = (value: string, group?: string, hint?: number): SelectOption => {
      const name = value === "gitlet" ? "Gitlet" : badge(value).name || value;
      return {
        value,
        label: name,
        text: `${name} ${value}`,
        leading: <Face who={value === "gitlet" ? GITLET : value} />,
        ...(group ? { group } : {}),
        ...(hint !== undefined ? { hint: fmtCount(hint) } : {}),
      };
    };
    const all: SelectOption = {
      value: "",
      label: "All seats",
      ...(faces.length > 0
        ? { leading: <AvatarStack members={faces} max={3} size="xs" decorative /> }
        : {}),
    };
    const seats = [...index.seats].sort((a, b) => a.name.localeCompare(b.name));
    let options: SelectOption[];
    if (pushers) {
      const named = top.filter((p) => p.kind === "gitlet" || p.handle);
      const pushed = new Set(named.map((p) => (p.kind === "gitlet" ? "gitlet" : p.handle!)));
      const here = `Pushed here in ${PUSHER_DAYS} days`;
      options = [
        all,
        ...named.map((p) => option(p.kind === "gitlet" ? "gitlet" : p.handle!, here, p.pushes)),
        ...(pushers.omitted
          ? [
              {
                // NO HANDLE HOLDS A SPACE, so this line can never be a seat's value.
                value: " more",
                label: `and ${fmtCount(pushers.omitted)} more who pushed less`,
                description: "Type a name to choose one",
                disabled: true,
                group: here,
              },
            ]
          : []),
        ...seats
          .filter((s) => !pushed.has(s.handle))
          .map((s) => option(s.handle, "Every other seat")),
      ];
    } else {
      options = [all, option("gitlet"), ...seats.map((s) => option(s.handle))];
    }
    if (by !== "" && !options.some((o) => o.value === by)) options.push({ value: by, label: by });
    return { options, kindOf };
  }, [pushers, by, index, badge]);
}

/** The subject's face: a seat's or a person's, Gitlet's mark, or a credential bound to no seat. */
function PushFace({ push }: { push: CodePushRow }) {
  const subject = pushSubject(push);
  if (subject.kind === "credential") {
    return (
      <IconTile size="sm" aria-hidden="true">
        <KeyGlyph />
      </IconTile>
    );
  }
  return <Face who={subject.kind === "gitlet" ? GITLET : subject.handle} size="sm" />;
}

/** The sentence: the subject's name in the strong weight, then what the push did. */
function PushSentenceLine({ push }: { push: CodePushRow }) {
  const whoName = useWhoName();
  const subject = pushSubject(push);
  const sentence = pushWords(push);
  return (
    <span className="code-push-sentence">
      {subject.kind === "credential" ? (
        <strong className="mono">{subject.login}</strong>
      ) : (
        <strong>{whoName(subject.kind === "gitlet" ? GITLET : subject.handle)}</strong>
      )}{" "}
      {sentence.parts.map((part, i) => (
        <Part key={i} part={part} />
      ))}
      {sentence.refused > 0 && (
        <>
          {" "}
          <Tag variant="danger" size="xs">{`${fmtCount(sentence.refused)} refused`}</Tag>
        </>
      )}
    </span>
  );
}

function Part({ part }: { part: PushPart }) {
  if (typeof part === "string") return <>{part}</>;
  if ("change" in part) return <ChangeRefPill change={part.change} />;
  if ("gone" in part) return <span className="mono">{part.gone}</span>;
  return <RefChip gitRef={part.ref} />;
}

/** A ref in a sentence: a branch's chip, a tag's, or any other ref in the code face. */
function RefChip({ gitRef }: { gitRef: string }) {
  const ref = refName(gitRef);
  if (ref.kind === "branch") return <BranchChip name={ref.name} />;
  if (ref.kind === "tag") {
    return (
      <Tag appearance="outline" size="xs" leadingIcon={<TagGlyph size="xs" />}>
        {ref.name}
      </Tag>
    );
  }
  return <span className="mono">{ref.name}</span>;
}

/** A merge's or a landing's change title after its sentence, and the changes beside it. */
function ChangeTitle({ push }: { push: CodePushRow }) {
  const landing = push.via === "merge" || push.via === "queue";
  if (!push.change || (!landing && !push.changes_more)) return null;
  return (
    <span className="code-push-change" title={push.change.title}>
      {landing && `· ${push.change.title}`}
      {push.changes_more ? ` and ${plural(push.changes_more, "more change")}` : ""}
    </span>
  );
}

/** The turn's page, from a chip. */
function TurnLink({ turn }: { turn: string }) {
  return (
    <>
      {" · "}
      <a className="t-link" href={href(["live", "turns", turn])}>
        turn
      </a>
    </>
  );
}

/** The chips under the sentence: the run or who asked, the credential, and how it was built. */
function PushChips({ push }: { push: CodePushRow }) {
  const whoName = useWhoName();
  const run = push.run;
  const asker = push.actor_seat || push.actor;
  const landing = push.via === "merge" || push.via === "queue";
  const model = run?.model
    ? `Model ${run.model}${run.harness ? ` in ${run.harness}` : ""}`
    : undefined;
  return (
    <>
      {landing ? (
        <Tag
          appearance="outline"
          size="xs"
          title={model}
          leadingIcon={<Face who={asker} size="text" />}
        >
          {push.via === "queue" ? "Queued by" : "Asked by"} <strong>{whoName(asker)}</strong>
          {run?.turn_id && <TurnLink turn={run.turn_id} />}
        </Tag>
      ) : (
        run?.turn_id && (
          <Tag appearance="outline" size="xs" leadingIcon={<BotGlyph size="xs" />} title={model}>
            {run.work_key ? (
              <>
                Run for{" "}
                <strong className="mono" title={run.work_backend}>
                  {run.work_key}
                </strong>
              </>
            ) : (
              "Run"
            )}
            <TurnLink turn={run.turn_id} />
          </Tag>
        )
      )}
      {push.actor_kind === "operator" && !landing && <CredentialTag login={push.actor} />}
      <Tag appearance="outline" size="xs">
        {VIA_WORDS[push.via] ?? push.via}
      </Tag>
    </>
  );
}

/** The credential a person pushed through: its kind, a name a person gave it, the whole id its title. */
function CredentialTag({ login }: { login: string }) {
  const { words, name } = credentialTag(login);
  return (
    <Tag appearance="outline" size="xs" leadingIcon={<KeyGlyph size="xs" />} title={login}>
      {words}
      {name && (
        <>
          {" "}
          <span className="mono">{name}</span>
        </>
      )}
    </Tag>
  );
}

/** At the row's end: when it landed, and its pack. */
function PushFacts({ push }: { push: CodePushRow }) {
  return (
    <span className="code-push-facts">
      <LandedAt at={push.at} />
      {push.pack_bytes ? (
        <span className="code-push-pack">
          <PackageGlyph size="xs" aria-hidden />
          {fmtBytes(push.pack_bytes)}
        </span>
      ) : (
        <span className="code-push-pack" title="Nothing new to store">
          No pack
        </span>
      )}
    </span>
  );
}

/** When a push landed, against the one clock: its tick re-renders this alone. */
function LandedAt({ at }: { at: string }) {
  const now = useNow();
  return <RelativeTime value={at} now={now} title={fmtDateTime(at)} />;
}

/** Under the row: its refs, and for a push refused whole, that nothing was stored and why. */
function PushBelow({ push, repo }: { push: CodePushRow; repo: string }) {
  const attempt = pushWords(push).attempt;
  const detail = (push.commands ?? []).find((c) => c.result === "ng")?.detail?.replace(/\.$/, "");
  return (
    <>
      <PushRefs push={push} repo={repo} />
      {attempt && (
        <p className="code-push-stored">
          <ShieldGlyph size="xs" aria-hidden />
          <span>
            Nothing was stored{detail ? `: ${detail}` : ""}.{" "}
            <a className="t-link prose-link" href={repoHref(repo, { tab: "settings" })}>
              See its rules
            </a>
          </span>
        </p>
      )}
    </>
  );
}

/** Where the whole of one push is: not asked yet, on its way, read, not to be had, or refused. */
type WholePush =
  | { state: "unread" }
  | { state: "reading" }
  | { state: "read"; commands: readonly CodePushCommand[] }
  | { state: "gone" }
  | { state: "failed"; error: QueryErrorCode };

const UNREAD: WholePush = { state: "unread" };

/**
 * A push's refs as the history in brief carries them, the rest counted, and every ref one read of
 * the push away. Memoised on its row, so the clock's tick draws no ref.
 */
const PushRefs = memo(function PushRefs({ push, repo }: { push: CodePushRow; repo: string }) {
  const [whole, setWhole] = useState<WholePush>(UNREAD);
  const [open, setOpen] = useState(false);
  const listId = useId();
  const shown = push.commands ?? [];
  const omitted = omittedOf(push.omitted);
  if (omitted.total === 0) return <Commands commands={shown} repo={repo} />;

  const read = async () => {
    setWhole({ state: "reading" });
    try {
      const answer = await codeRest.query("code_pushes", {
        name: repo,
        push: push.push_id,
        // AT THE PUSH'S OWN POSITION: a copy behind the page it came from waits for it.
        read_level: "session",
        min_position: [spellPosition(push.position)],
      });
      const row = answer.pushes?.find((p) => p.push_id === push.push_id);
      setWhole(row ? { state: "read", commands: row.commands ?? [] } : { state: "gone" });
    } catch (err) {
      setWhole({
        state: "failed",
        error: queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed",
      });
    }
  };

  const showing = open && whole.state === "read";
  return (
    <span className="col gap-1">
      <span id={listId}>
        <Commands commands={showing ? whole.commands : shown} repo={repo} />
      </span>
      {!showing && <Omitted outcomes={omitted.outcomes} total={omitted.total} />}
      {whole.state === "gone" && (
        <span className="t-caption">The history no longer holds this push.</span>
      )}
      {/* BESIDE THE CONTROL, which asks again: nothing retries on its own. */}
      {whole.state === "failed" && (
        <span className="t-caption" role="alert">
          {`Could not read this push (${whole.error}). Try again.`}
        </span>
      )}
      {whole.state !== "gone" && (
        <span>
          <Button
            size="small"
            variant="ghost"
            aria-expanded={showing}
            aria-controls={listId}
            loading={whole.state === "reading"}
            disabled={whole.state === "reading"}
            onClick={() => {
              if (showing) {
                setOpen(false);
                return;
              }
              setOpen(true);
              if (whole.state !== "read") void read();
            }}
          >
            {showing ? "Show fewer" : `Show all ${plural(shown.length + omitted.total, "ref")}`}
          </Button>
        </span>
      )}
    </span>
  );
});

/** What a push read in brief left out: how many, and what became of them, refusals first. */
function Omitted({ outcomes, total }: { outcomes: readonly OutcomeCount[]; total: number }) {
  return (
    <span className="t-caption">
      and {fmtCount(total)} more:{" "}
      {outcomes.map(({ outcome, count }, i) => (
        <Fragment key={outcome}>
          {i > 0 && ", "}
          {outcome === "refused" ? (
            <Tag variant="danger">{`${fmtCount(count)} refused`}</Tag>
          ) : (
            `${fmtCount(count)} ${outcome}`
          )}
        </Fragment>
      ))}
    </span>
  );
}

/**
 * Each ref a push asked to move, one block each: its glyph, what became of it, its name, and the
 * commits it moved between (the comparison), was made at, or was deleted from.
 */
function Commands({ commands, repo }: { commands: readonly CodePushCommand[]; repo: string }) {
  return (
    <span className="code-push-refs">
      {commands.map((c) => {
        const ref = refName(c.ref);
        const word = commandWord(c);
        const ok = c.result === "ok";
        const Glyph = ref.kind === "tag" ? TagGlyph : GitBranchGlyph;
        return (
          <span key={c.ref} className="code-push-ref" title={c.ref}>
            <Glyph size="sm" className="code-glyph" />
            {c.result === "ng" ? (
              <Tag variant="danger" size="xs" title={c.detail || c.reason || undefined}>
                {c.reason ? `refused: ${c.reason.replaceAll("_", " ")}` : word}
              </Tag>
            ) : (
              <strong className="code-push-ref__word">{word}</strong>
            )}
            <span className="code-push-ref__name">{ref.name}</span>
            <RefIds command={c} word={ok ? word : ""} repo={repo} />
          </span>
        );
      })}
    </span>
  );
}

/** The ids a ref moved between (the comparison), was made at, or held when deleted or refused. */
function RefIds({
  command: c,
  word,
  repo,
}: {
  command: CodePushCommand;
  word: string;
  repo: string;
}) {
  if (word === "created") {
    return (
      <span className="code-push-ref__ids">
        at{" "}
        <a className="mono t-link prose-link" href={commitHref(repo, c.new)} title={c.new}>
          {shortOID(c.new)}
        </a>
      </span>
    );
  }
  if (word === "deleted") {
    return (
      <span className="mono muted" title={c.old}>
        {shortOID(c.old)}
      </span>
    );
  }
  if (word === "moved" || word === "forced") {
    return (
      <a
        className="mono t-link code-push-ref__ids"
        href={compareHref(repo, { base: c.old, head: c.new })}
        title={`${c.old}..${c.new}`}
      >
        {`${shortOID(c.old)} → ${shortOID(c.new)}`}
      </a>
    );
  }
  // A REFUSED REF STORED NOTHING: its ids are words, never a comparison of commits not kept.
  if (c.result === "ng") {
    return (
      <span className="mono muted code-push-ref__ids" title={`${c.old}..${c.new}`}>
        {`${shortOID(c.old)} → ${shortOID(c.new)}`}
      </span>
    );
  }
  return null;
}
