/**
 * The Dashboard Assistant's panel: a head, the conversation, and the
 * composer. Composed only from the design system's parts; what is here is
 * which part draws what, and the gestures that move the conversation.
 *
 * # Anatomy
 *
 * The HEAD stands at the page bar's height so the two lines meet: the Orbit
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
 * the one line that says the assistant can be wrong. Above it, while the
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
  type KeyboardEvent,
} from "react";
import {
  Button,
  Callout,
  ChatThread,
  Composer,
  ConfirmModal,
  EmptyState,
  IconButton,
  Menu,
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
  PlusGlyph,
  RotateCcwClockGlyph,
  SquarePenGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { ASSISTANT_INPUT_MAX_RUNES } from "~/contract/assistant.ts";
import { questionFits } from "~/protocol/assistantWire.ts";
import { relTime } from "~/lib/format.ts";
import { rereadViewer } from "~/lib/viewer.ts";
import { keyRow, matchesRow, pressText } from "../../keymap.ts";
import { on as onBus, emit } from "../bus.ts";
import { usePageSnapshot } from "../context.ts";
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
} from "./session.ts";
import { prepareAgainQuestion } from "./blocks.tsx";
import { Home } from "./Home.tsx";
import { Thread } from "./Thread.tsx";
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
   * Where the panel stands. As an overlay or the whole window it is a modal,
   * where the page's own keys stand aside and the panel answers ⌘. itself;
   * as the whole window its foot is where a toast would otherwise land.
   */
  shape: AssistantShape;
}

/** Send's words, with Stop named for what it ends: the answer, not the conversation. */
const SEND_LABELS = { send: "Send", stop: "Stop answering" } as const;

/** A clock for the run chip and the message feet: each second while a turn runs, each minute otherwise. */
function useNow(fast: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), fast ? 1000 : 60_000);
    return () => clearInterval(timer);
  }, [fast]);
  return now;
}

/** One recent conversation, as the Conversations menu lists it. */
interface Recent {
  id: string;
  title: string;
  updated_at: string;
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
  const modal = shape !== "column";
  const view = useConversation();
  const page = usePageSnapshot();
  const toast = useToast();
  const composer = useRef<HTMLTextAreaElement>(null);
  const [draft, setDraft] = useState("");
  const [removed, setRemoved] = useState<string | null>(null);
  const [recents, setRecents] = useState<Recent[] | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [clearFailed, setClearFailed] = useState(false);
  const [opened, setOpened] = useState<ReadonlySet<string>>(() => new Set());
  const running = openTurn(view);
  const now = useNow(running !== null);

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
  useEffect(() => onBus("assistant.context.invalidate", ({ revision }) => pageMoved(revision)), []);

  // THE PAGE GOES WITH THE NEXT QUESTION until the reader removes it, and a
  // removal lasts until they open ANOTHER page, which attaches itself.
  const attached = page && page.key !== removed ? page : null;
  const crumb = page?.title ?? "";

  // A QUESTION THE ENGINE TURNED AWAY FOR ITS LIMIT is put back in the box,
  // so the reader's words are not lost to the refusal.
  const last = view.turns.at(-1);
  const bounced =
    last?.phase === "refused" && last.refusal?.code === "assistant_busy" ? last : null;
  useEffect(() => {
    if (bounced) setDraft((d) => (d === "" ? bounced.question : d));
  }, [bounced]);

  const busyUntil =
    bounced && bounced.refusal?.retryAfterSeconds
      ? Date.parse(bounced.askedAt) + bounced.refusal.retryAfterSeconds * 1000
      : 0;
  const waitSeconds = Math.max(0, Math.ceil((busyUntil - now) / 1000));
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
    if (view.stream === "reconnecting") return "Offline";
    if (waitSeconds > 0) return `You can ask again in ${waitSeconds} seconds`;
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

  const ask = useCallback(
    (text: string) => {
      const question = text.trim();
      if (question === "" || !questionFits(question)) return;
      setDraft("");
      setAsked((n) => n + 1);
      void send(question, attached);
    },
    [attached],
  );

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
    setDraft(text);
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
    if (event.key === "ArrowUp" && draft === "" && last) {
      event.preventDefault();
      setDraft(last.question);
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

  const menu: MenuEntry[] = [
    ...(recents ?? []).map((r): MenuEntry => ({
      key: r.id,
      label: r.title || "Untitled conversation",
      icon: <MessageSquareGlyph size="sm" />,
      hint: relTime(r.updated_at, now),
      onSelect: () => void openConversation(r.id),
    })),
    ...(recents && recents.length > 0 ? [{ kind: "separator" as const, key: "sep" }] : []),
    {
      key: "clear",
      label: "Clear this conversation",
      icon: <TrashGlyph size="sm" />,
      danger: true,
      disabled: view.id === null,
      onSelect: () => setConfirming(true),
    },
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
              <Composer
                ref={composer}
                id={ASSISTANT_COMPOSER_ID}
                label="Ask the assistant"
                value={draft}
                onChange={setDraft}
                onSubmit={() => ask(draft)}
                onKeyDown={onKeyDown}
                submitOn="enter"
                maxLength={ASSISTANT_INPUT_MAX_RUNES}
                placeholder={
                  view.turns.length === 0 ? "Ask about agents, tasks or spend" : "Ask a follow-up"
                }
                disabledReason={disabledReason}
                attachmentsLabel="Attached to this question"
                attachments={
                  page === null ? undefined : attached ? (
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
                  )
                }
                trailing={
                  <SendButton
                    mode={stoppable ? "stop" : "send"}
                    labels={SEND_LABELS}
                    title={stoppable ? stopTitle : undefined}
                    onPress={stoppable ? () => void stop() : () => ask(draft)}
                    disabledReason={
                      stoppable
                        ? running?.stopping
                          ? "Stopping"
                          : undefined
                        : (disabledReason ??
                          (draft.trim() === "" ? "Type a question first" : undefined))
                    }
                  />
                }
              />
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
              <Home name={name} onAsk={ask} onFill={fill} now={now} />
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
                  undo: undoAction,
                }}
                retryDisabledReason={disabledReason}
                onRetry={(text) => ask(text)}
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
