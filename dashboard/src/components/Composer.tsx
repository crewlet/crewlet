/**
 * Writing on a task as the person signed in (ADR-0024): from the
 * Inbox's pane, and from the task's own page.
 *
 * TWO MODES, and neither closes an ask (an answer is `AnswerAsk`'s, the one
 * write every surface answers an ask with):
 *
 *  - a REPLY (`comment_on_work_item{item, body, reply_to}`) is said in a
 *    thread and answers nothing — a person asking the asker a question back
 *    leaves the question open;
 *  - a COMMENT is the task page's: said on the task, or — through "Ask" — put
 *    to one seat as a question it owes an answer to (`ask`), optionally with
 *    the options it is to choose between (`decision`). An ask does not hand the
 *    task over; it wakes the person asked, and their answer comes back to the
 *    asker's inbox.
 *
 * `@` MENTIONS OVER THE ORG. The engine resolves a mention from the body at
 * write, so the picker only writes `@handle` into the text; it never sends a
 * list beside it that could disagree with what the text says.
 *
 * AND THE TWO THINGS A COMMENT CANNOT CARRY: a link to another task (the item's
 * `linked` relation) and a page — an existing one, or what was written here
 * saved as a new one (`write_page`) — attached to the item (`linked_pages`).
 * Both are writes of their own, made with the same person's token, so a
 * reader sees them land in the item's links rather than in the prose.
 */

import { forwardRef, useImperativeHandle, useMemo, useRef, useState } from "react";
import {
  Button,
  Callout,
  Checkbox,
  FormField,
  IconButton,
  Input,
  Modal,
  Select,
  Textarea,
  type SelectOption,
} from "@crewlethq/ui";
import {
  CircleQuestionMarkGlyph,
  FileTextGlyph,
  HashGlyph,
  PaperclipGlyph,
  PlusGlyph,
  SendGlyph,
  XGlyph,
} from "@crewlethq/icons/glyphs";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { QueryState } from "~/components/common.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useSearchQuery } from "~/lib/useSearchQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { DOCLET, needsWords } from "~/lib/access.ts";
import { matchesRow } from "~/app/keymap.ts";
import { handleLabel, type OrgIndex } from "~/lib/seats.ts";
import { firstLine, textBudget } from "~/lib/format.ts";
import { DECISION_QUESTION_MAX_BYTES } from "~/contract/work.ts";
import { itemAddress } from "~/lib/work.ts";
import type { WorkRanked } from "~/protocol/index.ts";
import { PAGE_TITLE_MAX_BYTES } from "~/contract/pages.ts";

/** How the composer posts. */
export type ComposeMode = { kind: "reply"; replyTo?: string } | { kind: "comment" };

export interface ComposerHandle {
  focus: () => void;
}

/** How many seats the `@` picker offers at once. */
const MENTION_CHOICES = 6;

export const Composer = forwardRef<
  ComposerHandle,
  {
    /**
     * The task's address ([itemAddress]) — what every write here names it
     * by: its key, or its id where another task claimed that key first.
     */
    item: string;
    /**
     * What the composer's own words call the task — its key — where `item`
     * is its id. Defaults to `item`.
     */
    label?: string;
    mode: ComposeMode;
    /** Whose words the reader is replying to, for the placeholder. */
    to: string;
    index: OrgIndex;
    /** What the empty field says, where the mode's own wording does not fit. */
    placeholder?: string;
    /** Told once what was written has been posted, so a reply target can close. */
    onSent?: () => void;
  }
>(function Composer({ item, label, mode, to, index, placeholder: said, onSent }, ref) {
  const named = label || item;
  const write = useAct("comment_on_work_item");
  const [text, setText] = useState("");
  const [picker, setPicker] = useState<"link" | "page" | "ask" | null>(null);
  const field = useRef<HTMLTextAreaElement | null>(null);
  useImperativeHandle(ref, () => ({ focus: () => field.current?.focus() }), []);

  // THE `@` BEING TYPED, from the caret back to the sign.
  const [caret, setCaret] = useState(0);
  const [active, setActive] = useState(0);
  // ESCAPE PUTS THE PICKER AWAY until the text changes again.
  const [dismissed, setDismissed] = useState(false);
  const typing = /(?:^|\s)@([\w.-]*)$/.exec(text.slice(0, caret));
  const query = typing && !dismissed ? typing[1]!.toLowerCase() : null;
  const choices = useMemo(() => {
    if (query === null) return [];
    return index.seats
      .filter((s) => s.handle.toLowerCase().includes(query) || s.name.toLowerCase().includes(query))
      .slice(0, MENTION_CHOICES);
  }, [index.seats, query]);
  const listId = `mention-${item}`;

  const pick = (handle: string) => {
    const before = text.slice(0, caret).replace(/@[\w.-]*$/, `@${handle} `);
    const next = before + text.slice(caret);
    setText(next);
    setCaret(before.length);
    requestAnimationFrame(() => {
      field.current?.focus();
      field.current?.setSelectionRange(before.length, before.length);
    });
  };

  const blocked = text.trim() ? undefined : "Write something first.";
  const send = async () => {
    // THE BUTTON'S OWN GATE, because ⌘Enter sends from inside the field
    // without touching the button: a second ⌘Enter while the first answer
    // was out posted the same comment twice.
    if (!pressable(write, blocked)) return;
    const body = text.trim();
    const args =
      mode.kind === "reply"
        ? { item, body, ...(mode.replyTo ? { reply_to: mode.replyTo } : {}) }
        : { item, body };
    const result = await write.run(args, {
      done: mode.kind === "reply" ? `Replied on ${named}` : `Commented on ${named}`,
    });
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      setText("");
      onSent?.();
    }
  };

  const placeholder =
    said ??
    (mode.kind === "reply"
      ? `Reply to ${to}… @mention to bring someone in`
      : `Leave a comment… @mention to bring someone in`);
  const fieldLabel = mode.kind === "reply" ? `Reply to ${to}` : `Comment on ${named}`;

  return (
    <div className="composer" data-mode={mode.kind}>
      <div className="composer-field">
        <Textarea
          ref={field}
          rows={mode.kind === "comment" ? 1 : 2}
          value={text}
          aria-label={fieldLabel}
          placeholder={placeholder}
          role="combobox"
          aria-autocomplete="list"
          aria-expanded={choices.length > 0}
          aria-controls={choices.length > 0 ? listId : undefined}
          aria-activedescendant={choices.length > 0 ? `${listId}-${active}` : undefined}
          onChange={(event) => {
            setText(event.target.value);
            setCaret(event.target.selectionStart ?? event.target.value.length);
            setActive(0);
            setDismissed(false);
          }}
          onSelect={(event) => setCaret(event.currentTarget.selectionStart ?? 0)}
          onKeyDown={(event) => {
            if (choices.length > 0) {
              if (event.key === "ArrowDown" || event.key === "ArrowUp") {
                event.preventDefault();
                const step = event.key === "ArrowDown" ? 1 : -1;
                setActive((i) => (i + step + choices.length) % choices.length);
                return;
              }
              if (event.key === "Enter" || event.key === "Tab") {
                event.preventDefault();
                pick(choices[active]!.handle);
                return;
              }
              if (event.key === "Escape") {
                event.preventDefault();
                event.stopPropagation();
                setDismissed(true);
                return;
              }
            }
            if (matchesRow("compose.send", event.nativeEvent)) {
              event.preventDefault();
              void send();
            }
          }}
        />
        {choices.length > 0 && (
          <ul
            className="composer-mentions"
            id={listId}
            role="listbox"
            aria-label="Mention somebody"
          >
            {choices.map((seat, i) => (
              <li
                key={seat.handle}
                id={`${listId}-${i}`}
                role="option"
                aria-selected={i === active}
                onMouseDown={(event) => {
                  event.preventDefault();
                  pick(seat.handle);
                }}
              >
                <strong>{seat.name}</strong>
                <span className="t-caption">{handleLabel(seat.handle)}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
      <div className="composer-foot">
        <IconButton
          size="sm"
          variant="ghost"
          label="Attach a page"
          icon={<PaperclipGlyph size="sm" />}
          onClick={() => setPicker("page")}
        />
        <IconButton
          size="sm"
          variant="ghost"
          label="Link a task"
          icon={<HashGlyph size="sm" />}
          onClick={() => setPicker("link")}
        />
        {mode.kind === "comment" && (
          // AN ASK IS A COMMENT PUT TO ONE SEAT, so it starts from what was
          // written here: the dialog adds who owes the answer and, if they
          // are to choose, between what.
          <WriteButton
            write={write}
            size="small"
            variant="ghost"
            leadingIcon={<CircleQuestionMarkGlyph size="sm" />}
            showRefusal={false}
            blocked={text.trim() ? undefined : "Write the question first."}
            onPress={() => setPicker("ask")}
          >
            Ask…
          </WriteButton>
        )}
        <span className="spacer" />
        <WriteButton
          write={write}
          size="small"
          // THE TASK PAGE'S COMMENT IS SECONDARY, as the artboard draws it: on
          // a page whose every other control is a field, a filled button under
          // the thread outshouts the work. The Inbox's reply is the pane's one
          // action and keeps the fill.
          variant={mode.kind === "comment" ? "secondary" : "primary"}
          leadingIcon={<SendGlyph size="sm" />}
          showRefusal={false}
          blocked={blocked}
          onPress={() => void send()}
        >
          {mode.kind === "comment" ? "Comment" : "Send"}
        </WriteButton>
      </div>
      <RefusalNote write={write} />
      {picker === "link" && (
        <LinkTaskDialog item={item} named={named} onClose={() => setPicker(null)} />
      )}
      {picker === "page" && (
        <AttachPageDialog item={item} named={named} draft={text} onClose={() => setPicker(null)} />
      )}
      {picker === "ask" && (
        <AskDialog
          item={item}
          named={named}
          body={text}
          index={index}
          write={write}
          onClose={(sent) => {
            setPicker(null);
            if (sent) setText("");
          }}
        />
      )}
    </div>
  );
});

/** The fewest and most options an ask may offer: `decision.MinOptions`
 *  and `MaxOptions`, which refuse anything outside them. */
const MIN_OPTIONS = 2;
const MAX_OPTIONS = 6;

/**
 * An option's id, from its words: what an answer names in `choice`, 1 to 32 of
 * a-z, 0-9, `_` and `-` — the engine's own rule — made unique among the ask's
 * options by a suffix.
 */
export function optionIds(labels: readonly string[]): string[] {
  const taken = new Set<string>();
  return labels.map((label, i) => {
    const base =
      label
        .toLowerCase()
        .normalize("NFKD")
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-+|-+$/g, "")
        .slice(0, 28) || `option-${i + 1}`;
    let id = base;
    for (let n = 2; taken.has(id); n++) id = `${base.slice(0, 28)}-${n}`;
    taken.add(id);
    return id;
  });
}

/**
 * Put what was written to one seat as a question it owes an answer to — and,
 * where they are to CHOOSE, the options, the one recommended and whether their
 * answer is the decision (`approver`) or an input to one (`contributor`).
 *
 * THE QUESTION IS ONE SENTENCE and the comment is its context, which is the
 * shape the engine holds a structured ask to: the question on the card, the
 * body under it.
 */
function AskDialog({
  item,
  named,
  body,
  index,
  write,
  onClose,
}: {
  /** The task's address, which the ask is posted on. */
  item: string;
  /** What the dialog's words call the task. */
  named: string;
  body: string;
  index: OrgIndex;
  write: ReturnType<typeof useAct<"comment_on_work_item">>;
  onClose: (sent: boolean) => void;
}) {
  const [who, setWho] = useState("");
  const [choose, setChoose] = useState(false);
  // THE FIRST LINE WHOLE. It was sliced to 200 characters — not the
  // engine's figure, which is bytes — so a long opening line arrived as half a
  // sentence nobody wrote. A line past the cap is said below the field, and
  // the person shortens it.
  const [question, setQuestion] = useState(firstLine(body));
  const [labels, setLabels] = useState<string[]>(["", ""]);
  const [recommended, setRecommended] = useState(-1);
  const [role, setRole] = useState<"approver" | "contributor">("approver");
  const seats = useMemo<SelectOption[]>(
    () =>
      index.seats.map((seat) => ({
        value: seat.handle,
        label: seat.name,
        description: handleLabel(seat.handle),
        text: `${seat.name} ${seat.handle}`,
        group: seat.kind === "human" ? "People" : "Agents",
      })),
    [index],
  );
  const filled = labels.map((l) => l.trim()).filter(Boolean);
  const name = index.byHandle.get(who)?.name ?? who;
  const questionBudget = textBudget(question, DECISION_QUESTION_MAX_BYTES, "a question");
  const blocked = !who
    ? "Choose who owes the answer."
    : choose && !question.trim()
      ? "Say what is being decided, in one sentence."
      : choose && questionBudget.over
        ? `Shorten the question: it is ${questionBudget.bytes} bytes and a question holds at most ${questionBudget.limit}.`
        : choose && filled.length < MIN_OPTIONS
          ? `Offer at least ${MIN_OPTIONS} options.`
          : undefined;
  const submit = async () => {
    // THE BUTTON'S OWN GATE, for the dialog's submit: an ask is a comment,
    // and a second one is a second ask.
    if (!pressable(write, blocked)) return;
    const ids = optionIds(filled);
    const pick = recommended >= 0 ? labels[recommended]?.trim() : "";
    const decision = choose
      ? {
          question: question.trim(),
          options: filled.map((label, i) => ({ id: ids[i]!, label })),
          role,
          ...(pick ? { recommended: ids[filled.indexOf(pick)] } : {}),
        }
      : undefined;
    const result = await write.run(
      { item, body: body.trim(), ask: who, ...(decision ? { decision } : {}) },
      { done: `Asked ${name} on ${named}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose(true);
  };
  return (
    <Modal
      open
      size="md"
      title={`Ask on ${named}`}
      icon={<CircleQuestionMarkGlyph />}
      onClose={() => onClose(false)}
      onSubmit={() => void submit()}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={() => onClose(false)} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            blocked={blocked}
            onPress={() => void submit()}
          >
            Ask
          </WriteButton>
        </>
      }
    >
      <div className="col gap-4">
        <FormField
          label="Who owes the answer"
          htmlFor="composer-ask-who"
          helper="They start following the task. It stays yours."
        >
          <Select
            id="composer-ask-who"
            ariaLabel="Who owes the answer"
            searchable
            searchPlaceholder="Find a seat"
            value={who}
            options={seats}
            onChange={(next) => setWho(String(next))}
          />
        </FormField>
        <Checkbox
          label="They are to choose between options"
          description="You’re notified of their choice."
          checked={choose}
          onCheckedChange={setChoose}
        />
        {choose && (
          <div className="col gap-3">
            <FormField
              label="What is being decided"
              htmlFor="composer-ask-q"
              helper={questionBudget.over ? undefined : questionBudget.line}
              error={questionBudget.over ? questionBudget.line : undefined}
            >
              <Input
                id="composer-ask-q"
                value={question}
                error={questionBudget.over}
                onChange={(event) => setQuestion(event.target.value)}
              />
            </FormField>
            <fieldset className="col gap-2 composer-ask-fieldset">
              <legend className="t-label">Options. Mark the one you recommend.</legend>
              {labels.map((label, i) => (
                <div key={i} className="row gap-2">
                  <input
                    type="radio"
                    name="composer-ask-recommended"
                    aria-label={`Recommend option ${i + 1}`}
                    checked={recommended === i}
                    onChange={() => setRecommended(i)}
                  />
                  <Input
                    aria-label={`Option ${i + 1}`}
                    value={label}
                    onChange={(event) =>
                      setLabels((all) => all.map((l, j) => (j === i ? event.target.value : l)))
                    }
                  />
                  {labels.length > MIN_OPTIONS && (
                    <IconButton
                      size="sm"
                      variant="ghost"
                      label={`Remove option ${i + 1}`}
                      icon={<XGlyph size="sm" />}
                      onClick={() => {
                        setLabels((all) => all.filter((_, j) => j !== i));
                        setRecommended((r) => (r === i ? -1 : r > i ? r - 1 : r));
                      }}
                    />
                  )}
                </div>
              ))}
              {labels.length < MAX_OPTIONS && (
                <Button
                  size="small"
                  variant="ghost"
                  leadingIcon={<PlusGlyph size="sm" />}
                  onClick={() => setLabels((all) => [...all, ""])}
                >
                  Add an option
                </Button>
              )}
            </fieldset>
            <FormField label="They are answering as" htmlFor="composer-ask-role">
              <Select
                id="composer-ask-role"
                ariaLabel="They are answering as"
                value={role}
                options={[
                  {
                    value: "approver",
                    label: "The approver",
                    description: "Their answer is the decision.",
                  },
                  {
                    value: "contributor",
                    label: "A contributor",
                    description: "Their answer informs someone else’s decision.",
                  },
                ]}
                onChange={(next) => setRole(next === "contributor" ? "contributor" : "approver")}
              />
            </FormField>
          </div>
        )}
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/**
 * Link another task to this one — a `linked` relation on the item.
 *
 * A HIT IS CHOSEN BY ITS ID AND LINKED BY ITS ADDRESS ([itemAddress]): two
 * tasks a search finds under one key are two hits, and linking the one the
 * engine flags `key_collision` by that key would link the task that claimed
 * it instead. Keyed on the key, the pair were one radio and choosing either
 * chose the first.
 */
function LinkTaskDialog({
  item,
  named,
  onClose,
}: {
  /** The task's address, which the link is written on. */
  item: string;
  /** What the dialog's words call the task. */
  named: string;
  onClose: () => void;
}) {
  const write = useAct("update_work_item");
  const [q, setQ] = useState("");
  const [chosen, setChosen] = useState<WorkRanked | null>(null);
  // THE BOUND IS THE HOOK'S: a phrase past it is not sent, and the list's
  // place says why (`useSearchQuery`).
  const search = useSearchQuery(
    "work_search",
    { q: q.trim(), mode: "hybrid", limit: 8 },
    { enabled: q.trim().length >= 2 },
  );
  // A REFUSED SEARCH LISTS NOTHING: `useQuery` keeps its last answer across a
  // failure, which here is the hits of a term the box no longer holds.
  // NOT THIS TASK, matched by address: by key, a duplicate's dialog left out
  // the claimant too, and the claimant's left out the duplicate.
  const hits = search.error ? [] : (search.data?.hits ?? []).filter((h) => itemAddress(h) !== item);
  const blocked = chosen ? undefined : "Choose a task first.";
  const submit = async () => {
    // THE BUTTON'S OWN GATE: the dialog is a form its search field submits
    // on Enter.
    if (!chosen || !pressable(write, blocked)) return;
    const result = await write.run(
      { item, linked: { add: [itemAddress(chosen)] } },
      { done: `Linked ${chosen.key} to ${named}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Link a task to ${named}`}
      icon={<HashGlyph />}
      onClose={onClose}
      onSubmit={() => void submit()}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            blocked={blocked}
            onPress={() => void submit()}
          >
            Link
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <FormField
          label="Find a task"
          htmlFor="composer-link-q"
          helper="A key like ENG-42, or words from its title."
        >
          <Input id="composer-link-q" value={q} onChange={(event) => setQ(event.target.value)} />
        </FormField>
        {search.tooLong ? (
          <Callout variant="warning">{search.tooLong}</Callout>
        ) : search.withheld ? (
          <Callout variant="info">{search.withheld}</Callout>
        ) : search.error ? (
          <QueryState
            error={search.error}
            refusal={search.refusal}
            detail={search.detail}
            loading={false}
          />
        ) : (
          <PickList
            label="Tasks"
            items={hits.map((h) => ({ id: h.id, title: h.title, meta: h.key }))}
            chosen={chosen?.id ?? ""}
            onChoose={(id) => {
              const hit = hits.find((h) => h.id === id);
              if (hit) setChosen(hit);
            }}
            empty={
              q.trim().length < 2
                ? "Type two characters to search."
                : search.loading
                  ? "Searching…"
                  : "No task matches."
            }
          />
        )}
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/**
 * Attach a page to the item: one that exists, or what the reader wrote saved
 * as a new page first. The long form a comment cannot hold belongs on a page,
 * and the engine's own refusal of an oversized comment says exactly that.
 */
function AttachPageDialog({
  item,
  named,
  draft,
  onClose,
}: {
  /** The task's address, which the page is attached to. */
  item: string;
  /** What the dialog's words call the task. */
  named: string;
  draft: string;
  onClose: () => void;
}) {
  const link = useAct("update_work_item");
  const save = useAct("write_page");
  const [q, setQ] = useState("");
  const [chosen, setChosen] = useState<{ id: string; title: string } | null>(null);
  // THE FIRST LINE WHOLE, for the Ask dialog's reason: a slice put half a
  // sentence in as the page's address. A line past the cap is said below the
  // field instead.
  const [title, setTitle] = useState(firstLine(draft));
  const titleBudget = textBudget(title, PAGE_TITLE_MAX_BYTES, "a page's title");
  // DOCLET'S LISTING, asked only of a reader holding Doclet: anybody else is
  // told what would let them find a page, rather than "no page has that in
  // its title" about a question never answered.
  const { doclet, loading: asking } = useViewer();
  const pagesLocked = !asking && !doclet;
  const found = useQuery(
    "pages",
    { title: q.trim(), limit: 8 },
    { enabled: q.trim().length >= 2 && doclet },
  );
  const busy = link.busy || save.busy;

  const attach = async (page: { id: string; title: string }) => {
    const result = await link.run(
      { item, linked_pages: { add: [page.id] } },
      { done: `Attached “${page.title}” to ${named}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  const saveThenAttach = async () => {
    const body = draft.trim();
    const name = title.trim();
    if (!body || !name) return;
    const saved = await save.run({ title: name, body }, { done: `Saved “${name}” as a page` });
    if (!saved || (saved.kind !== "applied" && saved.kind !== "pending")) return;
    const id = (saved.receipt as { id?: unknown } | null)?.id;
    if (typeof id === "string" && id) await attach({ id, title: name });
  };

  return (
    <Modal
      open
      size="md"
      title={`Attach a page to ${named}`}
      icon={<PaperclipGlyph />}
      onClose={onClose}
      closeDisabledReason={busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <WriteButton
            write={link}
            variant="primary"
            showRefusal={false}
            blocked={chosen ? undefined : "Choose a page first."}
            onPress={() => chosen && void attach(chosen)}
          >
            Attach
          </WriteButton>
        </>
      }
    >
      <div className="col gap-4">
        <FormField
          label="An existing page"
          htmlFor="composer-page-q"
          helper="Words from its title."
        >
          <Input id="composer-page-q" value={q} onChange={(event) => setQ(event.target.value)} />
        </FormField>
        <PickList
          label="Pages"
          items={(found.data?.pages ?? []).map((p) => ({
            id: p.id,
            title: p.title,
            meta: p.container,
          }))}
          chosen={chosen?.id ?? ""}
          onChoose={(id) => {
            const page = found.data?.pages.find((p) => p.id === id);
            if (page) setChosen({ id: page.id, title: page.title });
          }}
          empty={
            pagesLocked
              ? needsWords("Finding a page", [DOCLET])
              : q.trim().length < 2
                ? "Type two characters to search."
                : found.loading
                  ? "Searching…"
                  : "No page has that in its title."
          }
        />
        <RefusalNote write={link} />
        <div className="composer-attach-save col gap-2">
          <FormField
            label="Or save what you wrote as a new page"
            htmlFor="composer-page-title"
            helper={
              titleBudget.over
                ? undefined
                : (titleBudget.line ?? "Saved in your team’s space and attached.")
            }
            error={titleBudget.over ? titleBudget.line : undefined}
          >
            <Input
              id="composer-page-title"
              value={title}
              error={titleBudget.over}
              onChange={(event) => setTitle(event.target.value)}
            />
          </FormField>
          <WriteButton
            write={save}
            variant="secondary"
            leadingIcon={<FileTextGlyph size="sm" />}
            blocked={
              !draft.trim()
                ? "Write the reply first."
                : !title.trim()
                  ? "Give the page a title."
                  : titleBudget.over
                    ? `Shorten the title: it is ${titleBudget.bytes} bytes and a page's title holds at most ${titleBudget.limit}.`
                    : undefined
            }
            onPress={() => void saveThenAttach()}
          >
            Save as a page and attach
          </WriteButton>
        </div>
      </div>
    </Modal>
  );
}

/** A short list of things to choose one of, as radio buttons. */
function PickList({
  label,
  items,
  chosen,
  onChoose,
  empty,
}: {
  label: string;
  items: { id: string; title: string; meta: string }[];
  chosen: string;
  onChoose: (id: string) => void;
  empty: string;
}) {
  if (items.length === 0) return <p className="t-caption">{empty}</p>;
  return (
    <div className="composer-picklist" role="radiogroup" aria-label={label}>
      {items.map((it) => (
        <button
          key={it.id}
          type="button"
          role="radio"
          aria-checked={it.id === chosen}
          className="composer-pick"
          onClick={() => onChoose(it.id)}
        >
          <span className="mono t-caption">{it.meta}</span>
          <span className="truncate">{it.title}</span>
        </button>
      ))}
    </div>
  );
}
