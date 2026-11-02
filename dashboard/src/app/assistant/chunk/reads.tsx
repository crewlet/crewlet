/**
 * An answer and the records it read: the prose, with the first mention of
 * each record drawn as its chip; the records it read and never named on one
 * "Also looked up" line under it; and every other card (a proposal, a refusal, a
 * notice, a file, a brief) as the shipped card it always was.
 *
 * WHAT A READ'S CARD DID, ITS PEEK DOES: a record's facts, its body and the
 * list under it, a table's rows (the columns the engine chose for the
 * drawer), the downloads of a table and the way to the record's own page,
 * followed only where the engine built one of this dashboard's own addresses.
 * Nothing a read offered is lost; it is one press from where it is named.
 *
 * Which mention is a chip is `mentions.ts`'s rule, on the rendered text.
 */

import { useMemo, type MouseEvent, type ReactNode } from "react";
import {
  ButtonLink,
  DownloadButton,
  EntityList,
  PropertyList,
  RecordChip,
  RecordChipList,
  Stack,
  Table,
} from "@crewlethq/ui";
import {
  ActivityGlyph,
  ArrowUpRightGlyph,
  BotGlyph,
  CircleAlertGlyph,
  CalendarClockGlyph,
  FileTextGlyph,
  FolderGlyph,
  FolderKanbanGlyph,
  LayersGlyph,
  ListFilterGlyph,
  MessageSquareGlyph,
  SquareKanbanGlyph,
  TableGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import type {
  AssistantCardEvent,
  AssistantEntityCard,
  AssistantTableCard,
} from "~/contract/assistant.ts";
import { exportFile } from "~/protocol/assistant.ts";
import { ASSISTANT_WORDS, renderMarkdown } from "~/lib/markdown.ts";
import { fmtExact } from "~/lib/format.ts";
import { useLatestHandlers } from "~/lib/useLatestHandlers.ts";
import { href as hrefOf } from "../../router.tsx";
import { recordWord } from "../brief.ts";
import { cellText, factText, ProseBlock, ReplyProse, type CardHandlers } from "./blocks.tsx";
import { mentionsOf, readsOf, type Mention, type Read } from "./mentions.ts";
import { openInApp } from "./openInApp.ts";

type EntityKind = NonNullable<AssistantEntityCard["kind"]>;

/** Each kind of record as the glyph its chip carries: decoration, the words are read. */
const KIND_GLYPH: Record<EntityKind, () => ReactNode> = {
  task: () => <SquareKanbanGlyph size="xs" />,
  project: () => <FolderKanbanGlyph size="xs" />,
  seat: () => <BotGlyph size="xs" />,
  unit: () => <UsersGlyph size="xs" />,
  page: () => <FileTextGlyph size="xs" />,
  container: () => <FolderGlyph size="xs" />,
  artifact: () => <LayersGlyph size="xs" />,
  turn: () => <ActivityGlyph size="xs" />,
  run: () => <ActivityGlyph size="xs" />,
  schedule: () => <CalendarClockGlyph size="xs" />,
  view: () => <ListFilterGlyph size="xs" />,
  room: () => <MessageSquareGlyph size="xs" />,
};

/** What a record's kind is called, at the head of its peek: "Task", "Agent". */
function kindWord(entity: AssistantEntityCard): string | undefined {
  if (!entity.kind) return undefined;
  const word = recordWord({ kind: entity.kind, id: entity.id });
  return `${word.charAt(0).toUpperCase()}${word.slice(1)}`;
}

/** Words joined as a sentence says them: "project", "project or status", "project, status or assignee". */
function listWords(words: readonly string[]): string {
  if (words.length <= 1) return words.join("");
  return `${words.slice(0, -1).join(", ")} or ${words[words.length - 1]}`;
}

/** "1 row", "8 rows". */
function rowCount(n: number): string {
  return `${fmtExact(n)} ${n === 1 ? "row" : "rows"}`;
}

/** Where a link the engine built opens: in place for a plain press, as a link for any other. */
function follow(href: string, on: CardHandlers) {
  const target = openInApp(href);
  if (!target) return null;
  return {
    href: hrefOf(target.path, target.query),
    onClick: (event: MouseEvent<HTMLAnchorElement>) => {
      if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey) return;
      event.preventDefault();
      on.openHere(target);
    },
  };
}

/**
 * A read's way to its own page, as the peek's foot draws it: "Open", named for
 * what it opens, so the key is said once (in the peek's head), never again on
 * the link. Nothing where the address is not one of this dashboard's own.
 */
function OpenLink({ read, on }: { read: Read; on: CardHandlers }) {
  const card = read.event.card;
  const link =
    card.type === "table"
      ? card.table?.open
      : card.type === "oversize"
        ? card.oversize?.open
        : card.entity?.open;
  const to = link ? follow(link.href, on) : null;
  if (!link || !to) return null;
  // A LIST OPENS IN ITS SCREEN, NAMED: "Open in Search". A record opens
  // itself, "Open", its key already said in the peek's head.
  const screen = card.type === "table" || card.type === "oversize";
  const words = screen ? `Open in ${link.label}` : "Open";
  return (
    <ButtonLink
      size="small"
      variant="ghost"
      href={to.href}
      onClick={to.onClick}
      aria-label={screen ? undefined : `Open ${read.label}`}
      trailingIcon={<ArrowUpRightGlyph size="sm" />}
    >
      {words}
    </ButtonLink>
  );
}

/** A table read's downloads, fetched from the turn's stored snapshot on the press. */
function Downloads({ table, fallback }: { table: AssistantTableCard; fallback: string }) {
  if (!table.exports) return null;
  const base =
    table.title
      .replace(/[^a-z0-9]+/gi, "-")
      .replace(/^-|-$/g, "")
      .toLowerCase() || fallback;
  const { csv, xlsx } = table.exports;
  return (
    <>
      <DownloadButton label="CSV" filename={`${base}.csv`} file={() => exportFile(csv)} />
      <DownloadButton label="XLSX" filename={`${base}.xlsx`} file={() => exportFile(xlsx)} />
    </>
  );
}

/** A table read's rows: the columns the engine chose for the drawer, sized by their words. */
function TableRows({ table }: { table: AssistantTableCard }) {
  const drawn = table.columns.flatMap((c, i) => (c.detail ? [] : [i]));
  return (
    <Table
      framed={false}
      columns="content"
      caption={table.title}
      captionHidden
      headers={drawn.map((i) => table.columns[i]!.label)}
      data={table.rows.map((row) =>
        drawn.map((i) => cellText(table.columns[i]!.type, row[i] ?? null)),
      )}
      align={drawn.map((i) => {
        const c = table.columns[i]!;
        return c.align ?? (c.type === "text" || c.type === "label" ? "start" : "end");
      })}
      // A LABEL IS A SHORT TOKEN, a status or a key, and keeps its line.
      nowrap={drawn.map((i) => table.columns[i]!.type === "label")}
      footer={
        table.total
          ? drawn.map((i) => cellText(table.columns[i]!.type, table.total![i] ?? null))
          : undefined
      }
      emptyMessage="No rows match."
    />
  );
}

/** A record read's facts, body and list, as its card drew them. */
function EntityBody({ entity, on }: { entity: AssistantEntityCard; on: CardHandlers }) {
  const facts = (entity.facts ?? []).filter((fact) => !fact.detail);
  const items = entity.items ?? [];
  return (
    <>
      {facts.length > 0 ? (
        <PropertyList
          groups={[
            {
              items: facts.map((fact) => ({
                label: fact.label,
                value: fact.mono ? <code>{factText(fact)}</code> : factText(fact),
              })),
            },
          ]}
        />
      ) : null}
      {entity.text ? <ReplyProse text={entity.text} /> : null}
      {entity.items_total && entity.items_total > items.length ? (
        <p>{`Showing the first ${fmtExact(items.length)} of ${fmtExact(entity.items_total)}.`}</p>
      ) : entity.items_more && items.length > 0 ? (
        <p>{`Showing the first ${fmtExact(items.length)}. Its page shows the rest.`}</p>
      ) : null}
      {items.length > 0 ? (
        <EntityList
          label={entity.title}
          items={items.map((item) => {
            const to = item.open ? follow(item.open.href, on) : null;
            return {
              id: `${item.kind ?? ""}:${item.id}`,
              name: item.label,
              ...(item.fact === undefined ? {} : { fact: item.fact }),
              ...(item.open && to
                ? {
                    action: (
                      <ButtonLink
                        size="small"
                        variant="ghost"
                        href={to.href}
                        onClick={to.onClick}
                        trailingIcon={<ArrowUpRightGlyph size="sm" />}
                      >
                        {item.open.label}
                      </ButtonLink>
                    ),
                  }
                : {}),
            };
          })}
        />
      ) : null}
    </>
  );
}

/**
 * One read as its chip: what the answer named it with (or its key, else its
 * title, on the Also looked up line), opening its peek.
 */
export function ReadChip({
  read,
  said,
  on,
}: {
  read: Read;
  /** The mention it stands in for, or none on the Also looked up line. */
  said?: Mention | undefined;
  on: CardHandlers;
}) {
  const card = read.event.card;
  const keyed = said ? said.by === "key" : read.key !== undefined;
  const text = said?.text ?? read.label;
  const actions = (
    <>
      <OpenLink read={read} on={on} />
      {card.type === "table" && card.table ? (
        <Downloads table={card.table} fallback={read.event.card_id} />
      ) : null}
    </>
  );
  if (card.type === "oversize" && card.oversize) {
    const o = card.oversize;
    const narrow = o.narrow ?? [];
    return (
      <RecordChip
        label={text}
        title={o.title}
        glyph={<CircleAlertGlyph size="xs" />}
        meta="Too large to show here"
        actions={actions}
      >
        <p>
          {o.open
            ? "This read found more than one answer can hold, so none of it is shown here. Its screen shows all of it."
            : "This read found more than one answer can hold, so none of it is shown here."}
        </p>
        {narrow.length > 0 ? (
          <p>{`To see it here, ask again narrowed by ${listWords(narrow)}.`}</p>
        ) : null}
      </RecordChip>
    );
  }
  if (card.type === "table" && card.table) {
    const t = card.table;
    // HOW MUCH IS HERE, as the peek says it: "8 rows", "First 50 of 210 rows",
    // "First 20 rows" (a page, more past it).
    const rows = t.truncated
      ? `First ${fmtExact(t.rows.length)} of ${rowCount(t.row_count)}`
      : t.more
        ? `First ${rowCount(t.row_count)}`
        : rowCount(t.row_count);
    // THE CARD SAYS IT IS A PAGE, where a notice beside it used to: how much
    // is here, and that its screen holds the rest.
    const partial = t.truncated || t.more;
    return (
      <RecordChip
        label={text}
        title={t.title}
        glyph={<TableGlyph size="xs" />}
        meta={t.window ? `${t.window} · ${rows}` : rows}
        actions={actions}
      >
        <TableRows table={t} />
        {partial ? (
          <p>
            {t.open
              ? `${rows} are shown here; ${t.open.label} shows all of them.`
              : `${rows} are shown here.`}
          </p>
        ) : null}
      </RecordChip>
    );
  }
  const entity = card.entity!;
  return (
    <RecordChip
      {...(keyed ? { code: text } : { label: text })}
      title={entity.title}
      glyph={entity.kind ? KIND_GLYPH[entity.kind]() : <FileTextGlyph size="xs" />}
      meta={kindWord(entity)}
      actions={actions}
    >
      <EntityBody entity={entity} on={on} />
    </RecordChip>
  );
}

/**
 * One answer: its prose with each read record's first mention as its chip,
 * the reads it never named on the Also looked up line, then its other cards.
 */
export function Answer({
  text,
  dimmed,
  cards,
  card,
  on,
}: {
  text: string;
  dimmed: boolean;
  cards: readonly AssistantCardEvent[];
  /** The shipped card for each card that is not a read. */
  card: (event: AssistantCardEvent) => ReactNode;
  on: CardHandlers;
}) {
  // ONE OBJECT OF HANDLERS for the answer's life, however its caller builds
  // them, so the mentions below are worked out once per change of the
  // answer's words or reads, and a chip still runs the newest handler.
  const handlers = useLatestHandlers(on);
  const { reads, others } = useMemo(() => readsOf(cards), [cards]);
  const { nodes, named } = useMemo(() => {
    const mentions = mentionsOf(reads, (mention, key) => (
      <ReadChip key={key} read={mention.read} said={mention} on={handlers} />
    ));
    // RENDERED FIRST, so `named` is whole before the Also looked up line reads it.
    const rendered =
      text.trim() === "" ? [] : renderMarkdown(text, { ...ASSISTANT_WORDS, prose: mentions.prose });
    return { nodes: rendered, named: mentions.named };
  }, [text, reads, handlers]);
  const unnamed = reads.filter((read) => !named.has(read.id));
  return (
    <Stack gap={3}>
      {nodes.length > 0 ? <ProseBlock dimmed={dimmed}>{nodes}</ProseBlock> : null}
      {unnamed.length > 0 ? (
        <RecordChipList label="Also looked up">
          {unnamed.map((read) => (
            <ReadChip key={read.id} read={read} on={handlers} />
          ))}
        </RecordChipList>
      ) : null}
      {others.map(card)}
    </Stack>
  );
}
