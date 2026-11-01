/**
 * A room's details, beside it: About, Members, Shared and Notifications.
 *
 * # Facts in cards, and Edit beside the two a member may change
 *
 * About groups the room's facts the way a reader looks for them: its name
 * (which never changes: the name is the address its create claimed, and this
 * build has no record that moves one), then what it is for (topic and
 * description, each with Edit where the engine would take the change), who it
 * belongs to, who made it, and how long it keeps what is said. An empty topic
 * reads as an invitation only to a reader who may set one; anybody else is
 * told plainly there is none.
 *
 * NOTHING HERE DECIDES WHO MAY CHANGE WHAT. Each control is held by the
 * room's own rule for its tool (`scope.tsx`'s `rules`, the engine's rule
 * mirrored), so Edit is offered where the engine would take the change and
 * nowhere else; the write itself is judged by the engine.
 *
 * # Members are the room's answer
 *
 * The list is the room's own detail (`chat_channel`), read once by the frame
 * for the open room. A unit room's members are the org chart's, so the pane
 * offers to add nobody there; a direct conversation IS its people, so adding
 * somebody would name a different conversation, and it offers nothing either.
 *
 * # Which agents get every message
 *
 * An agent member either gets every message in the room (`follow_all`) or
 * answers when a message names it, and its row says which. Its switch moves
 * that through `set_channel_members`, stated against the membership the pane
 * read, so a change somebody made meanwhile is refused rather than written
 * back over. Where the room does not take the change (a team's room, whose
 * following is the org chart's), the switch shows the state, stays put and
 * says why. A person is never woken by a message, so a person's row has no
 * switch; neither does a direct conversation, where every message is for
 * everybody in it.
 *
 * # A tab is the pane's own, and the address only opens it
 *
 * The pane's address names the tab it opened on (More, then Shared in this
 * room). Moving between tabs inside the pane is not a place, so it does not
 * fill the history with every tab looked at; a new address naming another tab
 * moves the pane to it.
 */

import { useEffect, useId, useState, type ReactNode } from "react";

import {
  Button,
  EmptyState,
  Input,
  Skeleton,
  Switch,
  TabPanel,
  Tabs,
  Tag,
  Tooltip,
  useToast,
} from "@crewlethq/ui";
import { PersonRow } from "@crewlethq/ui";
import { IconTile } from "@crewlethq/ui";
import { LogOutGlyph, NetworkGlyph, SearchGlyph, UserPlusGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";

import { QueryState } from "~/components/common.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { fmtDate } from "~/lib/format.ts";
import { stateWord } from "~/lib/seats.ts";
import { useAct } from "~/lib/useAct.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { ChatChannelDetail, ChatMember } from "~/protocol/index.ts";

import { ChatFace } from "./Face.tsx";
import { DETAILS_TABS, useChatFrame, useChatNav, type DetailsTab } from "./nav.tsx";
import { NotifyLevels } from "./notify.tsx";
import { ChatSidePanel } from "./PanePanel.tsx";
import { useChatPeople, type ChatPerson } from "./people.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { knownKind, roomKindLabel, roomMark, roomTitle, roomWhere } from "./rooms.ts";
import { useRoomDetail } from "./roomDetail.ts";
import { RoomScopeProvider, useRoomScope } from "./scope.tsx";
import { SharedList } from "./Shared.tsx";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";

const TAB_LABEL: Record<DetailsTab, string> = {
  about: "About",
  members: "Members",
  shared: "Shared",
  notifications: "Notifications",
};

export function DetailsPane({
  room,
  tab,
  onClose,
}: {
  room: string;
  tab: DetailsTab;
  onClose: () => void;
}) {
  const detail = useRoomDetail(room);
  return (
    <RoomScopeProvider channelID={room} detail={detail}>
      <DetailsBody detail={detail} tab={tab} onClose={onClose} />
    </RoomScopeProvider>
  );
}

function DetailsBody({
  detail,
  tab: opened,
  onClose,
}: {
  detail: QueryResult<ChatChannelDetail>;
  tab: DetailsTab;
  onClose: () => void;
}) {
  const frame = useChatFrame();
  const panel = useId();
  const [tab, setTab] = useState<DetailsTab>(opened);
  // A NEW ADDRESS NAMING ANOTHER TAB moves the open pane to it.
  useEffect(() => setTab(opened), [opened]);

  const answer = detail.data;
  const channel = answer?.channel;
  if (!answer || !channel) {
    return (
      <ChatSidePanel title="Details" onClose={onClose}>
        <QueryState
          error={detail.error}
          refusal={detail.refusal}
          detail={detail.detail}
          loading={detail.loading}
        />
      </ChatSidePanel>
    );
  }

  const direct = isDirect(channel.kind);
  const summary = frame.channels.find((row) => row.channel.id === channel.id);
  const title = roomTitle(channel, {
    participants: summary?.participants,
    viewer: frame.viewer,
    nameOf: frame.nameOf,
  });
  // WHAT A ROOM OFFERS IS ITS KIND'S: a conversation has no links index of
  // its own to list apart from what its two people said, and only a member
  // has a say in what reaches them.
  const tabs = DETAILS_TABS.filter(
    (value) => (value !== "shared" || !direct) && (value !== "notifications" || answer.member),
  );
  const shown = tabs.includes(tab) ? tab : "about";

  return (
    <ChatSidePanel
      title={title}
      subtitle={roomKindLabel(channel.kind)}
      icon={direct ? undefined : <NamedGlyph name={roomMark(channel.kind)} size="md" />}
      onClose={onClose}
      flush
    >
      <div className="chat-pane-body">
        <Tabs
          className="chat-pane-tabs"
          variant="underline"
          size="sm"
          ariaLabel="Details"
          panelId={panel}
          value={shown}
          onValueChange={(value) => setTab(value as DetailsTab)}
          items={tabs.map((value) => ({
            value,
            label: TAB_LABEL[value],
            ...(value === "members" ? { count: (answer.members ?? []).length } : {}),
          }))}
        />
        <TabPanel id={panel} value={shown}>
          <div className="chat-pane-scroll">
            {shown === "about" && <About answer={answer} />}
            {shown === "members" && <Members answer={answer} />}
            {shown === "shared" && <SharedList channelID={channel.id} />}
            {shown === "notifications" && <NotifyLevels channelID={channel.id} />}
          </div>
        </TabPanel>
      </div>
    </ChatSidePanel>
  );
}

/* About ------------------------------------------------------------------ */

/** How long a room keeps what is said, in words. */
export function keptFor(days: number | null | undefined): string {
  // ABSENT IS THE COMPANY'S ANSWER and zero is for ever: the room's own field
  // is nullable precisely so the two are not one value.
  if (days === 0) return "For ever";
  if (typeof days === "number") return `${days.toLocaleString()} days`;
  return "As long as the company default";
}

function About({ answer }: { answer: ChatChannelDetail }) {
  const nav = useChatNav();
  const scope = useRoomScope();
  const people = useChatPeople();
  const channel = answer.channel;
  const direct = isDirect(channel.kind);
  const edit = (field: "topic" | "description") =>
    scope.rules.settings === ""
      ? () => nav.openDialog({ kind: field, room: channel.id })
      : undefined;

  return (
    <div className="chat-pane-section">
      {!direct && (
        <div className="chat-about">
          <AboutRow label="Room name" note="Rooms keep the name they were made with.">
            <span className="chat-inline-mark">
              <NamedGlyph name={roomMark(channel.kind)} size="xs" />
              {channel.name || channel.id}
            </span>
          </AboutRow>
        </div>
      )}
      <div className="chat-about">
        {!direct && (
          <AboutRow label="Topic" empty="topic" onEdit={edit("topic")}>
            {channel.topic}
          </AboutRow>
        )}
        {!direct && (
          <AboutRow label="Description" empty="description" onEdit={edit("description")}>
            {channel.purpose}
          </AboutRow>
        )}
        {channel.kind === "unit" && channel.unit && (
          <AboutRow label="Team">
            <span className="chat-inline-mark">
              <NetworkGlyph size="xs" /> {channel.unit}. Membership follows the org chart.
            </span>
          </AboutRow>
        )}
        <AboutRow label="Created by">
          {channel.created_by
            ? `${people.nameOf(channel.created_by)} on ${fmtDate(channel.created_at)}`
            : `On ${fmtDate(channel.created_at)}`}
        </AboutRow>
        <AboutRow label="Messages are kept">{keptFor(channel.retention_days)}</AboutRow>
        <LeaveRow answer={answer} />
      </div>
    </div>
  );
}

/**
 * Leave, for a member of a room a person may leave: a public or private one.
 * A unit room's membership is the org chart's and a conversation is its
 * people, so neither is offered; an archived room takes no change at all.
 */
function LeaveRow({ answer }: { answer: ChatChannelDetail }) {
  const nav = useChatNav();
  const toast = useToast();
  const leave = useAct("leave_channel");
  const [report, setReport] = useState<StartReport | null>(null);
  const channel = answer.channel;
  const offered =
    answer.member &&
    !channel.archived_at &&
    (channel.kind === "public" || channel.kind === "private");
  if (!offered) return null;

  async function press() {
    if (!pressable(leave)) return;
    setReport(null);
    const result = await leave.run({ channel: channel.id }, { done: "Left the room", quiet: true });
    if (result === null) return;
    const answered = reportFor("leave", result);
    if (answered.tone === "success") {
      // LEAVING TAKES THE READER OUT OF THE ROOM: a private room just left is
      // one they can no longer read, so staying would turn into "no such
      // room" under them a moment later.
      toast.show({ variant: "success", message: `You left #${channel.name}` });
      nav.go({ kind: "activity" });
      return;
    }
    setReport(answered);
  }

  return (
    <>
      <WriteButton
        variant="ghost"
        className="chat-about__leave"
        leadingIcon={<LogOutGlyph size="sm" />}
        write={leave}
        showRefusal={false}
        onPress={() => void press()}
      >
        {leave.busy ? "Leaving" : "Leave room"}
      </WriteButton>
      {report && (
        <div className="chat-about__row">
          <StartNote report={report} />
        </div>
      )}
    </>
  );
}

/**
 * One row of a details card: a label, its value, and Edit where the reader may
 * change it.
 */
function AboutRow({
  label,
  note,
  empty,
  onEdit,
  children,
}: {
  label: string;
  note?: string;
  /** What the value is called when there is none yet ("topic"). */
  empty?: string;
  onEdit?: (() => void) | undefined;
  children: ReactNode;
}) {
  const blank = empty !== undefined && !children;
  return (
    <div className="chat-about__row">
      <div className="chat-about__head">
        <span className="chat-about__label">{label}</span>
        {onEdit && !blank && (
          <Button
            variant="ghost"
            size="small"
            onClick={onEdit}
            aria-label={`Edit ${label.toLowerCase()}`}
          >
            Edit
          </Button>
        )}
      </div>
      <div className="chat-about__value">
        {!blank ? (
          children
        ) : onEdit ? (
          <Button variant="ghost" size="small" flush onClick={onEdit}>
            Add a {empty}
          </Button>
        ) : (
          <span className="chat-about__empty">No {empty} yet</span>
        )}
      </div>
      {note && <div className="chat-about__note">{note}</div>}
    </div>
  );
}

/* Members ---------------------------------------------------------------- */

/** How an agent member follows a room, as its row says it. */
export function followingWords(followsAll: boolean): string {
  return followsAll ? "Gets every message" : "Answers when named";
}

/**
 * The membership a following switch states: the room's members as the pane
 * read them, every one's following said, with `handle`'s set to `on`. The
 * read itself is the expectation it is stated against.
 */
export function withFollowing(
  members: readonly Pick<ChatMember, "handle" | "follow_all">[],
  handle: string,
  on: boolean,
): {
  members: { handle: string; follow_all: boolean }[];
  expected_members: { handle: string; follow_all: boolean }[];
} {
  const read = members.map((m) => ({ handle: m.handle, follow_all: m.follow_all === true }));
  return {
    members: read.map((m) => (m.handle === handle ? { ...m, follow_all: on } : m)),
    expected_members: read,
  };
}

function Members({ answer }: { answer: ChatChannelDetail }) {
  const nav = useChatNav();
  const frame = useChatFrame();
  const scope = useRoomScope();
  const people = useChatPeople();
  const follow = useAct("set_channel_members");
  const [filter, setFilter] = useState("");
  const [report, setReport] = useState<StartReport | null>(null);
  const channel = answer.channel;
  const direct = isDirect(channel.kind);
  const where = roomWhere(channel.kind, channel.name ?? channel.id);
  const q = filter.trim().toLowerCase();
  const rows = membersOf(answer.members ?? [], people.person).filter(
    ({ person }) => q === "" || person.name.toLowerCase().includes(q),
  );
  const canAdd = scope.rules.members === "" && knownKind(channel.kind);

  async function setFollowing(member: ChatMember, person: ChatPerson, on: boolean) {
    if (!pressable(follow, scope.rules.following || undefined)) return;
    setReport(null);
    const result = await follow.run(
      { channel: channel.id, ...withFollowing(answer.members ?? [], member.handle, on) },
      {
        done: on
          ? `${person.name} gets every message in ${where}`
          : `${person.name} answers when named in ${where}`,
      },
    );
    if (result === null) return;
    const answered = reportFor("following", result);
    if (answered.tone !== "success") setReport(answered);
  }

  return (
    <div className="chat-pane-section">
      <Input
        inputSize="sm"
        width="full"
        leading={<SearchGlyph size="sm" />}
        placeholder="Find members"
        aria-label="Find members"
        value={filter}
        onChange={(event) => setFilter(event.target.value)}
        onClear={() => setFilter("")}
      />
      {(rows.length > 0 || (canAdd && q === "")) && (
        <ul className="chat-member-list">
          {canAdd && q === "" && (
            <li>
              <PersonRow
                avatar={
                  <IconTile tone="accent" size="sm">
                    <UserPlusGlyph size="md" />
                  </IconTile>
                }
                name="Add people"
                detail="People and agents from the company"
                onSelect={() => nav.openDialog({ kind: "people", room: channel.id })}
              />
            </li>
          )}
          {rows.map(({ member, person }) => {
            const follows = !direct && person.kind === "agent";
            const on = member.follow_all === true;
            const who = person.team || (person.kind === "agent" ? "Agent" : "Person");
            return (
              <li key={member.handle}>
                <PersonRow
                  avatar={<ChatFace person={person} size={32} />}
                  name={
                    <>
                      {person.name}
                      {person.handle === frame.viewer && (
                        <span className="chat-muted-text"> (you)</span>
                      )}
                    </>
                  }
                  detail={follows ? `${who} · ${followingWords(on)}` : who}
                  meta={person.status ? stateWord(person.state) : undefined}
                  tag={
                    channel.created_by === person.handle ? <Tag size="xs">Owner</Tag> : undefined
                  }
                  onSelect={() => nav.openPane({ kind: "profile", handle: person.handle })}
                  action={
                    follows ? (
                      <FollowSwitch
                        name={person.name}
                        on={on}
                        rule={scope.rules.following}
                        busy={follow.busy}
                        onChange={(next) => void setFollowing(member, person, next)}
                      />
                    ) : undefined
                  }
                />
              </li>
            );
          })}
        </ul>
      )}
      <StartNote report={report} />
      {rows.length === 0 && q !== "" && (
        <EmptyState
          size="compact"
          title="Nobody matches"
          description={`Nobody in ${direct ? "this conversation" : roomWhere(channel.kind, channel.name ?? channel.id)} matches “${filter.trim()}”.`}
        />
      )}
      {(answer.members ?? []).length === 0 && q === "" && !canAdd && (
        <Skeleton variant="text" rows={2} label="Reading the members" />
      )}
    </div>
  );
}

/**
 * Whether every message in the room reaches one agent member, at the end of
 * its row. Where the room does not take the change, it shows the state, stays
 * put and says why, in a tooltip and after its name; while a change is on its
 * way it holds still too, so a second press cannot be stated against a read
 * the first is about to replace.
 */
function FollowSwitch({
  name,
  on,
  rule,
  busy,
  onChange,
}: {
  name: string;
  on: boolean;
  /** Why the room does not take the change from this reader, or "". */
  rule: string;
  busy: boolean;
  onChange: (on: boolean) => void;
}) {
  const label = `${name} gets every message`;
  if (rule) {
    return (
      <Tooltip content={rule}>
        <Switch label={label} labelHidden checked={on} disabledReason={rule} />
      </Tooltip>
    );
  }
  return (
    <Switch
      label={label}
      labelHidden
      checked={on}
      disabledReason={busy ? "Saving the last change." : undefined}
      onCheckedChange={onChange}
    />
  );
}

/**
 * A room's members as the list draws them: people first, then agents, each by
 * name, the order every roster in chat keeps.
 */
export function membersOf(
  members: readonly ChatMember[],
  person: (handle: string) => ChatPerson,
): { member: ChatMember; person: ChatPerson }[] {
  const rank = (p: ChatPerson) => (p.kind === "human" ? 0 : 1);
  return members
    .map((member) => ({ member, person: person(member.handle) }))
    .sort((a, b) => rank(a.person) - rank(b.person) || a.person.name.localeCompare(b.person.name));
}
