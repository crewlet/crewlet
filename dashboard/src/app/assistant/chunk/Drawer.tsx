/**
 * Ask Crewlet beside a screen: a head, the conversation, and the composer.
 * The conversation is the one Ask Crewlet's page holds too (`Conversation.tsx`
 * draws it for both); what is here is the panel round it.
 *
 * # Anatomy
 *
 * The HEAD stands at the page bar's height so the two lines meet: the spark
 * mark, "Ask Crewlet", then New conversation, Conversations (a menu of recent
 * conversations, Rename, and Delete behind a confirmation), Open in full page (Ask
 * Crewlet's page, on this conversation, with every other listed beside it)
 * and Close. No status tag and no help: what the assistant may do is said
 * once, on the home.
 *
 * The BODY is the conversation's one scroller and the FOOT its composer, as
 * `Conversation.tsx` draws them, with the screen under the panel attached to
 * the next question.
 *
 * # Keys
 *
 * The conversation's own (`Conversation.tsx`), and: Escape, once no answer
 * is running, closes the panel, never a peek under it; ⌘. pressed in the
 * composer of the whole-window panel closes it, since the frame's own binding
 * stands aside while a modal holds the keyboard.
 */

import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent } from "react";
import { IconButton, Menu, SidePanel, announce, type MenuEntry } from "@crewlethq/ui";
import { ProductMark } from "@crewlethq/icons/product-mark";
import {
  Maximize2Glyph,
  MessageSquareGlyph,
  PencilGlyph,
  RotateCcwClockGlyph,
  SquarePenGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { matchesRow } from "../../keymap.ts";
import { emit } from "../bus.ts";
import { takeReopened, type AssistantFocus } from "../control.ts";
import { usePageSnapshot, pageSnapshot } from "../context.ts";
import { readTab, reopenChoice, writeTab } from "../tab.ts";
import type { AssistantModelProblem, AssistantUnavailable, AttachClosed } from "../availability.ts";
import { useToastInset } from "../../toastInset.tsx";
import type { AssistantShape } from "../../layout.ts";
import {
  conversation,
  drawerDraft,
  newConversation,
  openConversation,
  recentConversations,
} from "./session.ts";
import {
  ASK_PAGE_PATH,
  AskDeleteConfirm,
  AskFoot,
  AskProvider,
  AskRename,
  AskThread,
  useAskGestures,
} from "./Conversation.tsx";

export interface DrawerProps {
  /** Why the reader may not ask, or null where they may. */
  unavailable: AssistantUnavailable | null;
  /** Why the assistant's model cannot answer now, or null when nothing is known to be wrong. */
  model: AssistantModelProblem | null;
  /** Why a question cannot carry images, or null where it can. */
  attach: AttachClosed | null;
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

/**
 * One recent conversation, as the Conversations menu lists it: shown at the
 * moment the list is ordered by, so the times read down the menu in order.
 */
interface Recent {
  id: string;
  title: string | null;
  last_activity_at: string;
}

export function Drawer(props: DrawerProps) {
  const { unavailable, model, attach, admin, name, shape, onClose } = props;
  const modal = shape === "sheet";
  return (
    <AskProvider
      box={drawerDraft}
      place={{
        unavailable,
        model,
        attach,
        admin,
        name,
        withPage: true,
        onComposerKey: (event: KeyboardEvent<HTMLTextAreaElement>) => {
          if (!modal || !matchesRow("assistant.open", event.nativeEvent)) return false;
          event.preventDefault();
          onClose();
          return true;
        },
      }}
    >
      <Panel {...props} />
    </AskProvider>
  );
}

function Panel({ unavailable, onClose, initialFocus, shape }: DrawerProps) {
  const { askDelete, askRename, resetPage, view, deletable, composer } = useAskGestures();
  const [recents, setRecents] = useState<Recent[] | null>(null);
  // THE SHELL'S ONE CLOCK, as every relative time on screen reads it.
  const now = useNow();

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

  // OPENED BY THE PERSON, the drawer resumes the conversation it last showed
  // on this page (or one with work in flight) and starts fresh on any other
  // (`reopenChoice`). Before the paint, so the last one never flashes.
  useLayoutEffect(() => {
    const tab = readTab();
    if (takeReopened() && reopenChoice(tab, pageSnapshot()?.key ?? null) === "fresh") {
      newConversation();
      drawerDraft.write("");
      return;
    }
    // THE CONVERSATION THIS TAB WAS ON comes back after a reload, read again
    // from the engine under this session.
    if (tab.conversation && conversation().id === null && conversation().turns.length === 0) {
      void openConversation(tab.conversation).catch(() => {
        // A conversation that is gone (deleted in another tab, purged) is
        // simply not reopened: the drawer goes back to its home.
        if (conversation().opening?.state === "missing") newConversation();
      });
    }
  }, []);

  // WHILE OPEN, the conversation goes with whatever page is under it.
  const page = usePageSnapshot()?.key ?? null;
  const shownId = view.id ?? view.opening?.id ?? null;
  useEffect(() => {
    writeTab({ shown: { conversation: shownId, page } });
  }, [shownId, page]);

  // THE LISTED CONVERSATIONS ARE ONE CHOICE whose answer is the one open
  // now: each says whether it is, so the open one carries the menu's check
  // and a screen reader hears which of them is current.
  const listed = (recents ?? []).map((r): MenuEntry => ({
    key: r.id,
    label: r.title || "Untitled conversation",
    icon: <MessageSquareGlyph size="sm" />,
    hint: relTime(r.last_activity_at, now),
    checked: r.id === view.id,
    // WHAT CAME OF IT is drawn from the store (`turn.ts`'s `Opening`).
    onSelect: () => void openConversation(r.id).catch(() => {}),
  }));
  const menu: MenuEntry[] = [
    ...listed,
    ...(listed.length > 0 && deletable ? [{ kind: "separator" as const, key: "sep" }] : []),
    ...(deletable
      ? [
          {
            key: "rename",
            label: "Rename this conversation",
            icon: <PencilGlyph size="sm" />,
            onSelect: () => askRename(),
          },
          {
            key: "delete",
            label: "Delete this conversation",
            icon: <TrashGlyph size="sm" />,
            danger: true,
            onSelect: () => askDelete(),
          },
        ]
      : []),
  ];

  const offline = unavailable !== null;
  const startNew = () => {
    newConversation();
    resetPage();
    announce("New conversation");
  };

  return (
    <>
      <SidePanel
        title="Ask Crewlet"
        icon={<ProductMark product="assistant" size="md" />}
        // THE THREAD IS THE ONE SCROLLER and insets its own items, so the
        // panel's body gives up its inset rather than doubling it. A flush
        // body clips rather than scrolls, so the offline notice, which does
        // not scroll itself, keeps the ordinary body and can still be read
        // in a short window.
        flush={!offline}
        onClose={onClose}
        labels={{ close: () => "Close Ask Crewlet" }}
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
              onClick={startNew}
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
            <IconButton
              size="sm"
              variant="ghost"
              icon={<Maximize2Glyph size="sm" />}
              label="Open in full page"
              title="Open in full page"
              onClick={() => openPage(view.id)}
            />
          </>
        }
        footer={
          offline ? undefined : (
            <div ref={foot}>
              <AskFoot />
            </div>
          )
        }
      >
        <AskThread onNew={startNew} />
      </SidePanel>
      <AskDeleteConfirm />
      <AskRename />
    </>
  );
}

/**
 * Open Ask Crewlet's page on the conversation open here, with every other
 * listed beside it. The frame closes the panel as the page opens: the page
 * holds the same conversation (`Shell.tsx`).
 */
function openPage(id: string | null): void {
  const path = id === null ? [...ASK_PAGE_PATH] : [...ASK_PAGE_PATH, id];
  emit("assistant.navigation.open", { path, query: {}, mode: "here" });
}
