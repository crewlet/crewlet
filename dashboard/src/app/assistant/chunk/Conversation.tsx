/**
 * One conversation with Ask Crewlet, as every place that holds one draws it:
 * the drawer beside a screen and Ask Crewlet's own page. Both are views of
 * the one store (`session.ts`), so a conversation opened in one is the one in
 * the other, and each draws it from the parts here, never a copy of them.
 *
 * # Anatomy
 *
 * [AskProvider] holds what a conversation needs beside the store: the draft,
 * the images attached to the next question, whether the page under it goes
 * with that question, the question put back after a refusal, and every
 * gesture that moves the conversation. The place that holds it draws:
 *
 * - [AskThread], the one scroller: a log with its live region off, the home
 *   until a question is asked, then the conversation;
 * - [AskFoot], the composer, with the page attached to the next question as a
 *   chip inside its edge (removing it sends the question with no page, until
 *   the reader adds it back or opens another page, which attaches itself),
 *   and the one line that says the assistant can be wrong. Images join the
 *   page in that row, picked with Attach image, dropped on the composer or
 *   pasted into it (a screenshot): each is uploaded at once and drawn as a
 *   chip with its thumbnail, and one the engine would refuse is refused above
 *   the composer in words that say what to do. Where no question can carry
 *   an image, Attach image is drawn disabled with that reason and a dropped
 *   or pasted image is refused in words that say what works. Above it, while
 *   the engine knows the model cannot answer (signed out, its plan spent, its
 *   harness unable to start), a notice says so in the engine's words and Send
 *   stays off with that reason: the reader is told before they ask, never
 *   after a wait. An administrator's notice also opens the models' settings;
 * - [AskDeleteConfirm], the confirmation Delete asks for, of the open
 *   conversation or of any other the holder lists.
 *
 * Ask Crewlet's own page attaches no page: it is the conversation, not a
 * screen the conversation is about.
 *
 * # Keys
 *
 * Enter sends and Shift with Enter adds a line (the kit's composer, IME
 * safe). Escape in the composer stops a running answer before anything else
 * hears it. Up in an empty composer brings back the last question.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ClipboardEvent,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
} from "react";
import {
  AttachmentChip,
  Button,
  Callout,
  ChatThread,
  EmptyState,
  FileDrop,
  FilePickButton,
  RefusalStrip,
  SendButton,
  Tag,
  Text,
  announce,
  useToast,
  type ChatThreadHandle,
} from "@crewlethq/ui";
import { ConfirmModal } from "~/components/Modal.tsx";
import {
  LayoutDashboardGlyph,
  LockGlyph,
  PaperclipGlyph,
  PlusGlyph,
} from "@crewlethq/icons/glyphs";
import { ASSISTANT_INPUT_MAX_RUNES, type AssistantFileRef } from "~/contract/assistant.ts";
import { questionFits } from "~/protocol/assistantWire.ts";
import { fmtWait } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { rereadViewer } from "~/lib/viewer.ts";
import { keyRow, matchesRow, pressText } from "../../keymap.ts";
import { useKeyHints } from "../../layout.ts";
import { on as onBus, emit } from "../bus.ts";
import { usePageSnapshot, type PageSnapshot } from "../context.ts";
import { briefQuestion } from "../brief.ts";
import {
  ASSISTANT_COMPOSER_ID,
  takeQueuedConversation,
  takeQueuedQuestion,
  useQueuedConversation,
  useQueuedQuestion,
} from "../control.ts";
import type { AssistantModelProblem, AssistantUnavailable, AttachClosed } from "../availability.ts";
import type { InAppTarget } from "./openInApp.ts";
import {
  approve,
  decline,
  deleteConversation,
  deleteRefusal,
  openConversation,
  pageMoved,
  reconnect,
  recordRestore,
  restoreOf,
  send,
  stop,
  undoAction,
  useConversation,
} from "./session.ts";
import { AskComposer, useDraftBox, type DraftBox } from "./AskComposer.tsx";
import { ATTACH_ACCEPT, ATTACH_HINT, useAttachments } from "./attachments.ts";
import { prepareAgainQuestion } from "./blocks.tsx";
import { Home } from "./Home.tsx";
import { Thread } from "./Thread.tsx";
import { bouncedOf, locksTheBox, waitLeft } from "./bounce.ts";
import { openTurn, type ConversationView } from "./turn.ts";

/** Send's words, with Stop named for what it ends: the answer, not the conversation. */
const SEND_LABELS = { send: "Send", stop: "Stop answering" } as const;

/** Ask Crewlet's page, where every conversation is listed. */
export const ASK_PAGE_PATH: readonly string[] = ["ask"];

/** What a place holding a conversation says about where it is. */
export interface AskPlace {
  /** Why the reader may not ask, or null where they may. */
  unavailable: AssistantUnavailable | null;
  /** Why the assistant's model cannot answer now, or null when nothing is known to be wrong. */
  model: AssistantModelProblem | null;
  /** Why a question cannot carry images, or null where it can. */
  attach: AttachClosed | null;
  /** The reader holds admin: the model notice also opens the models' settings. */
  admin: boolean;
  /** The reader's own name, for the home's greeting. */
  name: string;
  /**
   * Whether the screen under the conversation goes with a question: the
   * drawer's screen does; Ask Crewlet's own page is the conversation itself.
   */
  withPage: boolean;
  /**
   * A key the composer hears before the conversation's own, true where it
   * took it: the modal drawer's ⌘., which closes it from inside.
   */
  onComposerKey?: ((event: KeyboardEvent<HTMLTextAreaElement>) => boolean) | undefined;
  /** A conversation was deleted from the person's history, the open one or another. */
  onDeleted?: ((id: string) => void) | undefined;
}

/** A conversation as its parts read it: the store's view and every gesture on it. */
interface Ask {
  place: AskPlace;
  view: ConversationView;
  now: number;
  draft: DraftBox;
  composer: RefObject<HTMLTextAreaElement | null>;
  thread: RefObject<ChatThreadHandle | null>;
  /** The screen under the conversation, where it goes with questions. */
  page: PageSnapshot | null;
  /** The page as the next question carries it, or null where the reader removed it. */
  attached: PageSnapshot | null;
  attachments: ReturnType<typeof useAttachments>;
  /** Why no question can be sent now, or undefined where one can. */
  disabledReason: string | undefined;
  stoppable: boolean;
  stopTitle: string;
  /** The key of the refused turn whose question is back in the box, or null. */
  inBox: string | null;
  opened: ReadonlySet<string>;
  deleteFailed: string | null;
  /** The conversation Delete is asking about, or null. */
  deleting: string | null;
  ask: (text: string, again?: readonly AssistantFileRef[]) => void;
  brief: () => void;
  fill: (text: string) => void;
  removePage: () => void;
  addPage: () => void;
  openHere: (target: InAppTarget, cardId?: string) => void;
  onKeyDown: (event: KeyboardEvent<HTMLTextAreaElement>) => void;
  onPaste: (event: ClipboardEvent<HTMLTextAreaElement>) => void;
  /** Ask the reader to confirm deleting a conversation: the open one, where none is named. */
  askDelete: (id?: string) => void;
  setDeleting: (id: string | null) => void;
  setDeleteFailed: (words: string | null) => void;
  /** Forget a page the reader removed: the next one goes with questions again. */
  resetPage: () => void;
}

const AskContext = createContext<Ask | null>(null);

/** The conversation the nearest [AskProvider] holds. */
function useAsk(): Ask {
  const ask = useContext(AskContext);
  if (ask === null) throw new Error("useAsk needs an AskProvider above it");
  return ask;
}

/** The gestures a holder of the conversation calls from its own controls. */
export function useAskGestures(): Pick<Ask, "askDelete" | "resetPage" | "view" | "composer"> & {
  deletable: boolean;
} {
  const { askDelete, resetPage, view, composer } = useAsk();
  // DELETE IS OFFERED ONLY FOR A CONVERSATION WITH SOMETHING IN IT: an empty
  // one has nothing to delete, and a disabled entry was an action that looked
  // available and did nothing.
  return {
    askDelete,
    resetPage,
    view,
    composer,
    deletable: view.id !== null && view.turns.length > 0,
  };
}

/** Holds one conversation's state for the parts drawn under it. */
export function AskProvider({ place, children }: { place: AskPlace; children: ReactNode }) {
  const view = useConversation();
  const screen = usePageSnapshot();
  const page = place.withPage ? screen : null;
  const composer = useRef<HTMLTextAreaElement>(null);
  // THE DRAFT IS NOT THIS COMPONENT'S STATE: a keystroke renders the composer
  // alone, never the thread under it (`AskComposer.tsx` says why).
  const draft = useDraftBox();
  const [removed, setRemoved] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [deleteFailed, setDeleteFailed] = useState<string | null>(null);
  const [opened, setOpened] = useState<ReadonlySet<string>>(() => new Set());
  const running = openTurn(view);
  // THE SHELL'S ONE CLOCK, as every relative time on screen reads it: an
  // answer that ended between two ticks of a clock of its own read "in 1s".
  const now = useNow();
  const attachments = useAttachments(view.id, place.attach?.body ?? null);

  // THE MODEL'S STATE IS ASKED AGAIN AS THE CONVERSATION IS SHOWN, rather
  // than read from the frame's slow poll: a model signed in again since is
  // not refused, and one that went out since is said before the reader asks.
  useEffect(() => rereadViewer(), []);

  // A PAGE CHANGE PREPARED AGAINST THE SCREEN UNDER THE CONVERSATION is
  // stale once that screen moves; the screen says so on the bus.
  useEffect(
    () =>
      onBus("assistant.context.invalidate", ({ revision, revisions }) =>
        pageMoved(revision, revisions),
      ),
    [],
  );

  // THE PAGE GOES WITH THE NEXT QUESTION until the reader removes it, and a
  // removal lasts until they open ANOTHER page, which attaches itself.
  const attached = page && page.key !== removed ? page : null;
  const crumb = page?.title ?? "";

  // A QUESTION THE ENGINE TURNED AWAY UNSENT is put back in the box, so the
  // reader's words are not lost to the refusal (`bounce.ts`, the one rule the
  // notice and the box's wait read too). Into an EMPTY box only, never over
  // what the reader typed since, and ONCE per turn (`recordRestore`): a
  // conversation shown again does not put back a question the reader deleted.
  // The notice says the question is back only where it went.
  const last = view.turns.at(-1);
  const bounced = bouncedOf(view.turns);
  // The decision is the session's; this only draws again once made.
  const [, decided] = useState(0);
  useEffect(() => {
    if (!bounced || restoreOf(bounced.key) !== undefined) return;
    const held = draft.read();
    if (held === "") draft.write(bounced.question);
    recordRestore(bounced.key, held === "" || held === bounced.question);
    decided((n) => n + 1);
  }, [bounced, draft]);
  const inBox = bounced !== null && restoreOf(bounced.key) === true ? bounced.key : null;

  const waitSeconds =
    bounced?.refusal && locksTheBox(bounced.refusal) ? (waitLeft(bounced, now) ?? 0) : 0;
  const stoppable =
    running !== null && running.phase !== "awaiting_approval" && running.phase !== "unconfirmed";
  // STOP SAYS ITS KEY in its tooltip, as the Ask button says its own, for a
  // reader with keys: Escape in the composer stops a running answer.
  const keyHints = useKeyHints();
  const stopTitle = keyHints
    ? `${SEND_LABELS.stop} (${pressText(keyRow("layer.close").presses[0]!)})`
    : SEND_LABELS.stop;

  const { model } = place;
  const disabledReason = useMemo(() => {
    if (view.fault === "protocol_mismatch") return "Reload to keep asking";
    if (model !== null) return model.short;
    if (view.stream === "reconnecting") return "Reconnecting to the answer";
    if (waitSeconds > 0) return `You can ask again ${fmtWait(waitSeconds)}`;
    return undefined;
  }, [view.fault, model, view.stream, waitSeconds]);

  // THE PERSON'S OWN QUESTION IS ALWAYS SHOWN: the thread follows only while
  // it is read at its end, so a question sent while scrolled up would land out
  // of view behind "Jump to latest". Each ask brings the thread to its end once
  // the question is drawn.
  const thread = useRef<ChatThreadHandle>(null);
  const [asked, setAsked] = useState(0);
  useEffect(() => {
    if (asked > 0) thread.current?.scrollToLatest();
  }, [asked]);

  // A QUESTION CARRIES THE IMAGES IN THE COMPOSER, or, asked again by Try
  // again, the ones it was first sent with.
  const { take } = attachments;
  const ask = useCallback(
    (text: string, again?: readonly AssistantFileRef[]) => {
      const question = text.trim();
      if (question === "" || !questionFits(question)) return;
      draft.write("");
      setAsked((n) => n + 1);
      void send(question, attached, again ?? take(), "ask");
    },
    [attached, take, draft],
  );
  // A BRIEF IS OF THE PAGE AS IT IS NOW, attached whatever the chip says: it
  // is what the brief is about. What the reader was typing stays in the box.
  const brief = useCallback(() => {
    if (page === null) return;
    setAsked((n) => n + 1);
    void send(briefQuestion(page.context), page, [], "brief");
  }, [page]);

  const fill = useCallback(
    (text: string) => {
      draft.write(text);
      requestAnimationFrame(() => {
        const field = composer.current;
        if (!field) return;
        field.focus();
        field.setSelectionRange(text.length, text.length);
      });
    },
    [draft],
  );

  const onPaste = (event: ClipboardEvent<HTMLTextAreaElement>) => {
    const files = Array.from(event.clipboardData.files);
    if (files.length === 0) return;
    // A SCREENSHOT IS PASTED AS AN IMAGE ALONE; text copied with a picture
    // still lands in the box as text.
    if (event.clipboardData.getData("text/plain") === "") event.preventDefault();
    attachments.add(files);
  };

  // A CONVERSATION STARTED ELSEWHERE (a room's catch up, continued here) is
  // opened as this one, read again from the engine under this session.
  const queuedConversation = useQueuedConversation();
  useEffect(() => {
    if (queuedConversation === null) return;
    const id = takeQueuedConversation();
    if (id === null) return;
    void openConversation(id).catch(() => {
      // A conversation that is gone (cleared in another tab, purged) is not
      // opened: the conversation stays where it was.
    });
  }, [queuedConversation]);

  // A QUESTION TYPED ELSEWHERE (the palette's row) is asked at once where
  // this conversation could ask it now, and otherwise waits in the box for
  // the person to send when the running answer ends.
  const queued = useQueuedQuestion();
  useEffect(() => {
    if (queued === null) return;
    const question = takeQueuedQuestion();
    if (question === null) return;
    if (running === null && disabledReason === undefined && questionFits(question.trim())) {
      ask(question);
    } else {
      fill(question);
    }
  }, [queued, running, disabledReason, ask, fill]);

  const { onComposerKey } = place;
  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (onComposerKey?.(event)) return;
    // ESCAPE STOPS A RUNNING ANSWER FIRST, and is claimed, so the holder and
    // a peek under it stay open; the next Escape is the holder's.
    if (stoppable && matchesRow("layer.close", event.nativeEvent)) {
      event.preventDefault();
      void stop();
      return;
    }
    // UP IN AN EMPTY BOX brings back the last question to edit.
    if (event.key === "ArrowUp" && draft.read() === "" && last) {
      event.preventDefault();
      draft.write(last.question);
    }
  };

  const openHere = (target: InAppTarget, cardId?: string) => {
    if (cardId) setOpened((s) => new Set(s).add(cardId));
    emit("assistant.navigation.open", { path: target.path, query: target.query, mode: "here" });
  };

  const value: Ask = {
    place,
    view,
    now,
    draft,
    composer,
    thread,
    page,
    attached,
    attachments,
    disabledReason,
    stoppable,
    stopTitle,
    inBox,
    opened,
    deleteFailed,
    deleting,
    ask,
    brief,
    fill,
    removePage: () => {
      if (!page) return;
      setRemoved(page.key);
      announce(`${crumb} removed from this question`);
    },
    addPage: () => {
      setRemoved(null);
      announce(`${crumb} added to this question`);
    },
    openHere,
    onKeyDown,
    onPaste,
    askDelete: (id) => {
      const target = id ?? view.id;
      if (target !== null) setDeleting(target);
    },
    setDeleting,
    setDeleteFailed,
    resetPage: () => setRemoved(null),
  };
  return <AskContext.Provider value={value}>{children}</AskContext.Provider>;
}

/** The conversation: the home until a question is asked, then its turns; or why the reader may not ask. */
export function AskThread({
  label = "Conversation",
  offlineAction,
}: {
  label?: string;
  /** The one step offered beside why the reader may not ask, where there is one they can take. */
  offlineAction?: ReactNode;
}) {
  const a = useAsk();
  const { unavailable, admin, name } = a.place;
  if (unavailable !== null) {
    return (
      <EmptyState
        icon={<LockGlyph size="lg" />}
        title={unavailable.title}
        description={unavailable.body}
        action={offlineAction}
      />
    );
  }
  return (
    <ChatThread ref={a.thread} label={label} busy={openTurn(a.view) !== null}>
      {a.deleteFailed !== null ? (
        <Callout variant="warning" role="alert">
          {a.deleteFailed}
        </Callout>
      ) : null}
      {a.view.turns.length === 0 ? (
        <Home
          name={name}
          page={a.page}
          onBrief={a.brief}
          onAsk={a.ask}
          onFill={a.fill}
          now={a.now}
        />
      ) : (
        <Thread
          view={a.view}
          now={a.now}
          opened={a.opened}
          on={{
            approve: (proposal) => void approve(proposal),
            decline: (proposal) => void decline(proposal),
            prepareAgain: (proposal) => a.ask(prepareAgainQuestion(proposal)),
            openHere: (target) => a.openHere(target),
            ask: (question) => a.ask(question),
            undo: undoAction,
          }}
          retryDisabledReason={a.disabledReason}
          inBox={a.inBox}
          technical={admin}
          onRetry={(text, _attachments, mode) => (mode === "brief" ? a.brief() : a.ask(text))}
          onReconnect={reconnect}
        />
      )}
    </ChatThread>
  );
}

/** The composer and what stands above it; nothing where the reader may not ask. */
export function AskFoot() {
  const a = useAsk();
  const { model, admin, attach, unavailable } = a.place;
  if (unavailable !== null) return null;
  const { attachments, disabledReason, page, attached } = a;
  const crumb = page?.title ?? "";
  // WHY NO IMAGE IS TAKEN NOW, as the Attach button and the drop overlay say it.
  const dropClosed = disabledReason ?? attach?.short;
  return (
    <>
      {model ? (
        <Callout
          variant="warning"
          className="assistant-model-notice"
          title={model.title}
          action={
            admin ? (
              <Button
                size="small"
                variant="secondary"
                onClick={() =>
                  emit("assistant.navigation.open", {
                    path: ["settings", "models"],
                    query: {},
                    mode: "here",
                  })
                }
              >
                Model settings
              </Button>
            ) : undefined
          }
        >
          {model.body}
        </Callout>
      ) : null}
      {attachments.refusal !== null ? (
        <RefusalStrip className="assistant-attach-refusal" onDismiss={attachments.dismiss}>
          {attachments.refusal}
        </RefusalStrip>
      ) : null}
      {/* IMAGES DROPPED ANYWHERE ON THE COMPOSER are attached; the button in
          its foot is the keyboard's way to the same. THE DROP IS ALWAYS
          TAKEN, so a file never replaces the dashboard in the tab: where none
          may be attached, the overlay and the refusal say why, as the Attach
          button does. */}
      <FileDrop
        title={dropClosed ?? "Drop images to attach them"}
        hint={dropClosed === undefined ? ATTACH_HINT : undefined}
        onFiles={(dropped) =>
          disabledReason === undefined
            ? attachments.add(dropped.map((d) => d.file))
            : attachments.refuse(`${disabledReason}, so nothing was attached.`)
        }
      >
        <AskComposer
          ref={a.composer}
          box={a.draft}
          id={ASSISTANT_COMPOSER_ID}
          label="Ask Crewlet"
          onSubmit={(text) => {
            if (!attachments.uploading) a.ask(text);
          }}
          onKeyDown={a.onKeyDown}
          onPaste={disabledReason === undefined ? a.onPaste : undefined}
          sendHint={false}
          maxLength={ASSISTANT_INPUT_MAX_RUNES}
          placeholder={
            a.view.turns.length === 0 ? "Ask about agents, tasks or spend" : "Ask a follow-up"
          }
          disabledReason={disabledReason}
          attachmentsLabel="Attached to this question"
          attachments={
            page === null && attachments.drafts.length === 0 ? undefined : (
              <>
                {page === null ? null : attached ? (
                  <Tag
                    size="sm"
                    appearance="outline"
                    leadingIcon={<LayoutDashboardGlyph size="xs" />}
                    title="Ask Crewlet can see this page"
                    onRemove={a.removePage}
                    removeAriaLabel={`Remove ${crumb} from this question`}
                  >
                    {crumb}
                  </Tag>
                ) : (
                  <Button
                    size="small"
                    variant="ghost"
                    leadingIcon={<PlusGlyph size="sm" />}
                    aria-label={`Add ${crumb} to this question`}
                    onClick={a.addPage}
                  >
                    Add this page
                  </Button>
                )}
                {attachments.drafts.map((d) => (
                  <AttachmentChip
                    key={d.key}
                    name={d.name}
                    preview={d.preview}
                    state={d.state}
                    onRemove={() => {
                      attachments.remove(d.key);
                      announce(`${d.name} removed from this question`);
                    }}
                  />
                ))}
              </>
            )
          }
          actions={
            <FilePickButton
              size="small"
              variant="ghost"
              leadingIcon={<PaperclipGlyph size="sm" />}
              accept={ATTACH_ACCEPT}
              disabledReason={dropClosed}
              onFiles={(picked) => attachments.add(picked.map((p) => p.file))}
            >
              Attach image
            </FilePickButton>
          }
          trailing={(text) => (
            <SendButton
              mode={a.stoppable ? "stop" : "send"}
              labels={SEND_LABELS}
              title={a.stoppable ? a.stopTitle : undefined}
              onPress={a.stoppable ? () => void stop() : () => a.ask(text)}
              disabledReason={
                a.stoppable
                  ? openTurn(a.view)?.stopping
                    ? "Stopping"
                    : undefined
                  : (disabledReason ??
                    (text.trim() === ""
                      ? "Type a question first"
                      : attachments.uploading
                        ? "Images are still uploading"
                        : undefined))
              }
            />
          )}
        />
      </FileDrop>
      <Text as="p" variant="caption" tone="tertiary" className="assistant-disclaimer">
        Ask Crewlet can be wrong. Check before you approve.
      </Text>
    </>
  );
}

/** Delete's confirmation, for the conversation [Ask.deleting] names. */
export function AskDeleteConfirm() {
  const a = useAsk();
  const toast = useToast();
  const { onDeleted } = a.place;
  const target = a.deleting;
  return (
    <ConfirmModal
      open={target !== null}
      destructive
      size="sm"
      title="Delete this conversation?"
      message="It is removed from your history. Any approval still waiting in it is withdrawn."
      confirmLabel="Delete conversation"
      cancelLabel="Cancel"
      onClose={() => a.setDeleting(null)}
      onConfirm={async () => {
        if (target === null) return;
        try {
          await deleteConversation(target);
          a.setDeleteFailed(null);
          a.setDeleting(null);
          onDeleted?.(target);
          toast.show({
            variant: "success",
            title: "Conversation deleted",
            message: "It is no longer in your history.",
          });
        } catch (err) {
          a.setDeleteFailed(deleteRefusal(err));
          a.setDeleting(null);
        }
      }}
    />
  );
}
