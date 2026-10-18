/**
 * The Dashboard Assistant's panel: a head, the conversation, and the
 * composer. Composed only from the design system's parts; what is here is
 * which part draws what, and the gestures that move the conversation.
 *
 * # Anatomy
 *
 * The HEAD stands at the page bar's height so the two lines meet: the spark
 * mark, "Crewlet Assistant", then New conversation, Conversations (a menu of
 * recent conversations with Clear behind a confirmation) and Close. No status
 * tag and no help: what the assistant may do is said once, on the home.
 *
 * The BODY is the one scroller, a log with its live region off: the home
 * until a question is asked, then the conversation.
 *
 * The FOOT is the composer, with the page attached to the next question as a
 * chip inside its edge (removing it sends the question with no page, until
 * the reader adds it back or opens another page, which attaches itself), and
 * the one line that says the assistant can be wrong. Images join the page in
 * that row, picked with Attach image, dropped on the composer or pasted into
 * it (a screenshot): each is uploaded at once and drawn as a chip with its
 * thumbnail, and one the engine would refuse is refused above the composer
 * in words that say what to do. Above it, while the
 * engine knows the model cannot answer (signed out, its plan spent, its CLI
 * unable to start), a notice says so in the engine's words and Send stays off
 * with that reason: the reader is told before they ask, never after a wait.
 * An administrator's notice also opens the models' settings.
 *
 * # Keys
 *
 * Enter sends and Shift with Enter adds a line (the kit's composer, IME
 * safe). Escape in the composer stops a running answer before anything else
 * hears it, and otherwise closes the panel, never a peek under it. Up in an
 * empty composer brings back the last question.
 */

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ClipboardEvent,
  type KeyboardEvent,
} from "react";
import {
  AttachmentChip,
  Button,
  Callout,
  ChatThread,
  ConfirmModal,
  EmptyState,
  FileDrop,
  FilePickButton,
  IconButton,
  Menu,
  RefusalStrip,
  SendButton,
  SidePanel,
  Tag,
  Text,
  announce,
  useToast,
  type MenuEntry,
  type ChatThreadHandle,
} from "@crewlethq/ui";
import { ProductMark } from "@crewlethq/icons/product-mark";
import {
  LayoutDashboardGlyph,
  LockGlyph,
  MessageSquareGlyph,
  PaperclipGlyph,
  PlusGlyph,
  RotateCcwClockGlyph,
  SquarePenGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { ASSISTANT_INPUT_MAX_RUNES, type AssistantFileRef } from "~/contract/assistant.ts";
import { questionFits } from "~/protocol/assistantWire.ts";
import { fmtWait, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { rereadViewer } from "~/lib/viewer.ts";
import { keyRow, matchesRow, pressText } from "../../keymap.ts";
import { on as onBus, emit } from "../bus.ts";
import { usePageSnapshot } from "../context.ts";
import { briefQuestion } from "../brief.ts";
import {
  ASSISTANT_COMPOSER_ID,
  takeQueuedQuestion,
  useQueuedQuestion,
  type AssistantFocus,
} from "../control.ts";
import type { AssistantModelProblem, AssistantUnavailable } from "../availability.ts";
import { readTab } from "../tab.ts";
import { useToastInset } from "../../toastInset.tsx";
import { useKeyHints, type AssistantShape } from "../../layout.ts";
import type { InAppTarget } from "./openInApp.ts";
import {
  approve,
  clearConversation,
  decline,
  newConversation,
  openConversation,
  pageMoved,
  reconnect,
  recentConversations,
  send,
  stop,
  undoAction,
  useConversation,
  conversation,
  recordRestore,
  restoreOf,
} from "./session.ts";
import { AskComposer, useDraftBox } from "./AskComposer.tsx";
import { ATTACH_ACCEPT, ATTACH_HINT, useAttachments } from "./attachments.ts";
import { prepareAgainQuestion } from "./blocks.tsx";
import { Home } from "./Home.tsx";
import { Thread } from "./Thread.tsx";
import { bouncedOf, locksTheBox, waitLeft } from "./bounce.ts";
import { openTurn } from "./turn.ts";

export interface DrawerProps {
  /** Why the reader may not ask, or null where they may. */
  unavailable: AssistantUnavailable | null;
  /** Why the assistant's model cannot answer now, or null when nothing is known to be wrong. */
  model: AssistantModelProblem | null;
  /** The reader holds admin: the model notice also opens the models' settings. */
  admin: boolean;
  /** Close the panel; focus goes back to the Ask button. */
  onClose: () => void;
  /** Where focus goes as the panel opens. */
  initialFocus: AssistantFocus;
  /** The reader's own name, for the greeting. */
  name: string;
  /**
   * Where the panel stands. As the whole window it is a modal, where the
   * page's own keys stand aside and the panel answers ⌘. itself, and its foot
   * is where a toast would otherwise land. Over the page's end edge it is not
   * a modal (the frame passes `asideModal={false}`): the page beside it stays
   * live, with no veil and no Tab trap, and the frame's own binding answers ⌘.
   * from inside it, as it does beside the column.
   */
  shape: AssistantShape;
}

/** Send's words, with Stop named for what it ends: the answer, not the conversation. */
const SEND_LABELS = { send: "Send", stop: "Stop answering" } as const;

/**
 * One recent conversation, as the Conversations menu lists it: shown at the
 * moment the list is ordered by, so the times read down the menu in order.
 */
interface Recent {
  id: string;
  title: string;
  last_activity_at: string;
}

export function Drawer({
  unavailable,
  model,
  admin,
  onClose,
  initialFocus,
  name,
  shape,
}: DrawerProps) {
  const modal = shape === "sheet";
  const view = useConversation();
  const page = usePageSnapshot();
  const toast = useToast();
  const composer = useRef<HTMLTextAreaElement>(null);
  // THE DRAFT IS NOT THIS PANEL'S STATE: a keystroke renders the composer
  // alone, never the thread under it (`AskComposer.tsx` says why).
  const draft = useDraftBox();
  const [removed, setRemoved] = useState<string | null>(null);
  const [recents, setRecents] = useState<Recent[] | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [clearFailed, setClearFailed] = useState(false);
  const [opened, setOpened] = useState<ReadonlySet<string>>(() => new Set());
  const running = openTurn(view);
  // THE SHELL'S ONE CLOCK, as every relative time on screen reads it. The
  // drawer kept its own, a minute apart once no turn ran, and did not re-read
  // the time when it slowed: an answer that ended between two ticks read
  // "in 1s" beside Copy, and the history "in 5s", for up to a minute.
  const now = useNow();
  const attachments = useAttachments(view.id);

  // ON A PHONE THE PANEL IS THE WHOLE WINDOW, and its foot (the composer and
  // the line under it) is where a toast would land: the stack stands above it.
  const foot = useRef<HTMLDivElement>(null);
  const [footHeight, setFootHeight] = useState(0);
  useLayoutEffect(() => {
    const element = foot.current?.closest<HTMLElement>(".crewlet-side-panel__foot");
    if (shape !== "sheet" || !element) return undefined;
    const measure = () => setFootHeight(Math.round(element.getBoundingClientRect().height));
    measure();
    if (typeof ResizeObserver === "undefined") return undefined;
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, [shape, unavailable]);
  useToastInset("panel", shape === "sheet" && footHeight > 0 ? { block: footHeight } : null);

  // THE MODEL'S STATE IS ASKED AGAIN AS THE PANEL OPENS, rather than read
  // from the frame's slow poll: a model signed in again since is not refused,
  // and one that went out since is said before the reader asks.
  useEffect(() => rereadViewer(), []);

  // THE CONVERSATION THIS TAB WAS ON comes back after a reload, read again
  // from the engine under this session.
  useEffect(() => {
    const kept = readTab().conversation;
    if (kept && conversation().id === null && conversation().turns.length === 0) {
      void openConversation(kept).catch(() => {
        // A conversation that is gone (cleared in another tab, purged) is
        // simply not reopened: the drawer stays on its home.
      });
    }
  }, []);

  // A PAGE CHANGE PREPARED AGAINST THE SCREEN UNDER THE PANEL is stale once
  // that screen moves; the screen says so on the bus.
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
  // what the reader typed since, and ONCE per turn (`recordRestore`): a drawer
  // closed and opened again does not put back a question the reader deleted.
  // The notice says the question is back only where it went.
  const last = view.turns.at(-1);
  const bounced = bouncedOf(view.turns);
  // The decision is the session's; this only draws the drawer again once made.
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
      void send(question, attached, again ?? take());
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

  const onPaste = (event: ClipboardEvent<HTMLTextAreaElement>) => {
    const files = Array.from(event.clipboardData.files);
    if (files.length === 0) return;
    // A SCREENSHOT IS PASTED AS AN IMAGE ALONE; text copied with a picture
    // still lands in the box as text.
    if (event.clipboardData.getData("text/plain") === "") event.preventDefault();
    attachments.add(files);
  };

  // A QUESTION TYPED ELSEWHERE (the palette's row) is asked at once where
  // this panel could ask it now, and otherwise waits in the box for the
  // person to send when the running answer ends.
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
    // `fill` is a fresh closure each render over the same setters.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [queued, running, disabledReason, ask]);

  const fill = (text: string) => {
    draft.write(text);
    requestAnimationFrame(() => {
      const field = composer.current;
      if (!field) return;
      field.focus();
      field.setSelectionRange(text.length, text.length);
    });
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    // ⌘. CLOSES A MODAL PANEL from its composer: the frame's own binding
    // stands aside while a modal holds the keyboard.
    if (modal && matchesRow("assistant.open", event.nativeEvent)) {
      event.preventDefault();
      onClose();
      return;
    }
    // ESCAPE STOPS A RUNNING ANSWER FIRST, and is claimed, so the panel and
    // a peek under it stay open; the next Escape closes the panel.
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

  const removePage = () => {
    if (!page) return;
    setRemoved(page.key);
    announce(`${crumb} removed from this question`);
  };
  const addPage = () => {
    setRemoved(null);
    announce(`${crumb} added to this question`);
  };

  // THE LISTED CONVERSATIONS ARE ONE CHOICE whose answer is the one open
  // now: each says whether it is, so the open one carries the menu's check
  // and a screen reader hears which of them is current.
  const listed = (recents ?? []).map((r): MenuEntry => ({
    key: r.id,
    label: r.title || "Untitled conversation",
    icon: <MessageSquareGlyph size="sm" />,
    hint: relTime(r.last_activity_at, now),
    checked: r.id === view.id,
    onSelect: () => void openConversation(r.id),
  }));
  // CLEAR IS OFFERED ONLY FOR A CONVERSATION WITH SOMETHING IN IT: an empty
  // one has nothing to clear, and a disabled entry there was an action that
  // looked available and did nothing.
  const clearable = view.id !== null && view.turns.length > 0;
  const menu: MenuEntry[] = [
    ...listed,
    ...(listed.length > 0 && clearable ? [{ kind: "separator" as const, key: "sep" }] : []),
    ...(clearable
      ? [
          {
            key: "clear",
            label: "Clear this conversation",
            icon: <TrashGlyph size="sm" />,
            danger: true,
            onSelect: () => setConfirming(true),
          },
        ]
      : []),
  ];

  const offline = unavailable !== null;

  return (
    <>
      <SidePanel
        title="Crewlet Assistant"
        icon={<ProductMark product="assistant" size="md" />}
        // THE THREAD IS THE ONE SCROLLER and insets its own items, so the
        // panel's body gives up its inset rather than doubling it. A flush
        // body clips rather than scrolls, so the offline notice, which does
        // not scroll itself, keeps the ordinary body and can still be read
        // in a short window.
        flush={!offline}
        onClose={onClose}
        labels={{ close: () => "Close the assistant" }}
        initialFocus={initialFocus === "composer" && !offline ? composer : "heading"}
        actions={
          <>
            <IconButton
              size="sm"
              variant="ghost"
              icon={<SquarePenGlyph size="sm" />}
              label="New conversation"
              title="New conversation"
              disabledReason={unavailable?.short}
              onClick={() => {
                newConversation();
                setRemoved(null);
                announce("New conversation");
              }}
            />
            <Menu
              label="Conversations"
              align="end"
              header="Your recent conversations"
              items={menu}
              onOpenChange={(open) => {
                if (open) {
                  void recentConversations()
                    .then(setRecents)
                    .catch(() => setRecents([]));
                }
              }}
              renderTrigger={(trigger) => (
                <IconButton
                  {...trigger}
                  size="sm"
                  variant="ghost"
                  icon={<RotateCcwClockGlyph size="sm" />}
                  label="Conversations"
                  title="Conversations"
                  disabledReason={unavailable?.short}
                />
              )}
            />
          </>
        }
        footer={
          offline ? undefined : (
            <div ref={foot}>
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
              {/* IMAGES DROPPED ANYWHERE ON THE COMPOSER are attached; the
                  button in its foot is the keyboard's way to the same. */}
              <FileDrop
                title="Drop images to attach them"
                hint={ATTACH_HINT}
                disabled={disabledReason !== undefined}
                onFiles={(dropped) => attachments.add(dropped.map((d) => d.file))}
              >
                <AskComposer
                  ref={composer}
                  box={draft}
                  id={ASSISTANT_COMPOSER_ID}
                  label="Ask the assistant"
                  onSubmit={(text) => {
                    if (!attachments.uploading) ask(text);
                  }}
                  onKeyDown={onKeyDown}
                  onPaste={disabledReason === undefined ? onPaste : undefined}
                  submitOn="enter"
                  maxLength={ASSISTANT_INPUT_MAX_RUNES}
                  placeholder={
                    view.turns.length === 0 ? "Ask about agents, tasks or spend" : "Ask a follow-up"
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
                            title="The assistant can see this page"
                            onRemove={removePage}
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
                            onClick={addPage}
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
                      disabledReason={disabledReason}
                      onFiles={(picked) => attachments.add(picked.map((p) => p.file))}
                    >
                      Attach image
                    </FilePickButton>
                  }
                  trailing={(text) => (
                    <SendButton
                      mode={stoppable ? "stop" : "send"}
                      labels={SEND_LABELS}
                      title={stoppable ? stopTitle : undefined}
                      onPress={stoppable ? () => void stop() : () => ask(text)}
                      disabledReason={
                        stoppable
                          ? running?.stopping
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
                The assistant can be wrong. Check before you approve.
              </Text>
            </div>
          )
        }
      >
        {offline ? (
          <EmptyState
            icon={<LockGlyph size="lg" />}
            title={unavailable.title}
            description={unavailable.body}
          />
        ) : (
          <ChatThread ref={thread} label="Conversation" busy={running !== null}>
            {clearFailed ? (
              <Callout variant="warning" role="alert">
                The conversation could not be cleared. Try again from Conversations.
              </Callout>
            ) : null}
            {view.turns.length === 0 ? (
              <Home name={name} page={page} onBrief={brief} onAsk={ask} onFill={fill} now={now} />
            ) : (
              <Thread
                view={view}
                now={now}
                opened={opened}
                on={{
                  approve: (proposal) => void approve(proposal),
                  decline: (proposal) => void decline(proposal),
                  prepareAgain: (proposal) => ask(prepareAgainQuestion(proposal)),
                  openHere: (target) => openHere(target),
                  ask: (question) => ask(question),
                  undo: undoAction,
                }}
                retryDisabledReason={disabledReason}
                inBox={inBox}
                technical={admin}
                onRetry={(text, _attachments, mode) => (mode === "brief" ? brief() : ask(text))}
                onReconnect={reconnect}
              />
            )}
          </ChatThread>
        )}
      </SidePanel>
      <ConfirmModal
        open={confirming}
        destructive
        size="sm"
        title="Clear this conversation?"
        message="It is removed from your history. Any approval still waiting in it is withdrawn."
        confirmLabel="Clear conversation"
        cancelLabel="Cancel"
        onClose={() => setConfirming(false)}
        onConfirm={async () => {
          try {
            await clearConversation();
            setClearFailed(false);
            setConfirming(false);
            toast.show({
              variant: "success",
              title: "Conversation cleared",
              message: "It is no longer in your history.",
            });
          } catch {
            setClearFailed(true);
            setConfirming(false);
          }
        }}
      />
    </>
  );
}
