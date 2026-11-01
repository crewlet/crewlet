/**
 * Catch up (or Summarize), beside a room: Ask Crewlet's brief of it.
 *
 * THE ASSISTANT'S OWN TURN, never a second model path. The brief is the
 * existing room brief (`app/assistant/roomBrief.ts`): the turn reads only what
 * the reader may read, posts nothing, and checks every point it cites against
 * its own reads and the reader's access before it is drawn. This pane starts
 * it, shows its live status while it works, and draws what it answered: the
 * gist, the points each with the message it came from, and what waits on the
 * reader.
 *
 * Catch up reads what the reader has not; Summarize, offered where nothing is
 * unread, reads the latest. The conversation continues in Ask Crewlet, which
 * holds the history this pane does not.
 */

import { BriefCard, Button, Callout, ThinkingIndicator } from "@crewlethq/ui";
import { ProductMark } from "@crewlethq/icons/product-mark";

import { useRoomBrief } from "~/app/assistant/roomBrief.ts";
import { useNavigator } from "~/app/router.tsx";
import type { AssistantBriefPoint } from "~/contract/assistant.ts";
import { appRoute } from "~/lib/appRoute.ts";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";

import { useChatNav, useOpenRoom, type CatchUpMode } from "./nav.tsx";
import { ChatSidePanel } from "./PanePanel.tsx";

const TITLE: Record<CatchUpMode, string> = { catchup: "Catch up", summarize: "Summarize" };

export function CatchUpPane({
  room,
  mode,
  onClose,
}: {
  room: string;
  mode: CatchUpMode;
  onClose: () => void;
}) {
  const nav = useChatNav();
  const open = useOpenRoom();
  const now = useNow();
  // ONE TURN, the engine's: it reads the unread messages, or the latest when
  // nothing is unread, so Catch up and Summarize differ only in what the
  // reader pressed, which the frame chose from the room's unread count.
  const brief = useRoomBrief(room);
  const where = open?.id === room ? open.where : "";
  // A CITED REPLY OPENS ITS THREAD; any other message lands the room on it,
  // with this pane still beside it.
  const go = (ref: AssistantBriefPoint["source"]) =>
    ref.thread_root
      ? nav.openThread(ref.channel_id, ref.thread_root)
      : nav.openRoom(ref.channel_id, { message: ref.message_id });

  const router = useNavigator();
  const card = brief.brief;

  return (
    <ChatSidePanel
      title={TITLE[mode]}
      subtitle={where || undefined}
      icon={<ProductMark product="assistant" size="md" />}
      onClose={onClose}
      footer={
        <div className="chat-pane-foot">
          <p className="chat-pane-note">
            Ask Crewlet reads only what you can read, and posts nothing. Each point links to the
            message it came from.
          </p>
          <Button size="small" variant="secondary" onClick={brief.openInAssistant}>
            Continue in Ask Crewlet
          </Button>
        </div>
      }
    >
      <div className="chat-pane-section">
        {brief.phase === "asking" && (
          <ThinkingIndicator
            status={brief.status}
            label={`Ask Crewlet is reading ${where || "the room"}`}
          />
        )}
        {brief.phase === "failed" && (
          <Callout
            variant="warning"
            action={
              <Button size="small" variant="secondary" onClick={brief.again}>
                Ask again
              </Button>
            }
          >
            {brief.error}
          </Callout>
        )}
        {brief.phase === "done" && card && (
          // THE KIT'S BRIEF, the one Ask Crewlet draws: the gist, what waits on
          // the reader (an engine-built address opens in place, a question is
          // asked in this catch up's own conversation), and what was said,
          // each point with the message it came from.
          <BriefCard
            scope={where}
            asOf={`Drafted ${relTime(card.asOf, now)}`}
            about={card.about}
            needs={card.needs.map((need, index) => ({
              key: `${index}:${need.ref.id}`,
              text: need.text,
              ...(need.detail ? { detail: need.detail } : {}),
              ...(need.tone === "danger" ? { tone: "danger" as const } : {}),
              action: {
                label: need.action.label,
                onSelect: () => {
                  // AN ADDRESS THE ENGINE BUILT opens here, by the
                  // dashboard's one reading of an in-app address; a question
                  // goes on in Ask Crewlet.
                  const target = need.action.link ? appRoute(need.action.link.href) : null;
                  if (target) router.to(target.path, target.query);
                  else brief.openInAssistant();
                },
              },
            }))}
            points={card.points.map((point, index) => ({
              key: `${index}:${point.source.message_id}`,
              text: point.text,
              source: { label: "Source", onSelect: () => go(point.source) },
            }))}
            followUps={card.followUps.map((question, index) => ({
              key: `${index}`,
              question,
              onSelect: brief.openInAssistant,
            }))}
            labels={{ heading: TITLE[mode] }}
          />
        )}
      </div>
    </ChatSidePanel>
  );
}
