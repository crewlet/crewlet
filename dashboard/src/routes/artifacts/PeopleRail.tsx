/**
 * The People rail (`rail=people`): who is in the artifact now, what each is
 * doing and where, one entry a seat, with Follow beside everybody else and,
 * for an agent, the turn it is working in (plan 05, 5.1).
 *
 * THE SAME FACTS AS THE PRESENCE STACK, IN WORDS: a ring, a cursor and a cell
 * outline are decorative to assistive technology, so this list is where a
 * screen reader user learns who is here and where (5.6).
 */

import { Button, ButtonLink, IconButton, PrincipalAvatar } from "@crewlethq/ui";
import { XGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import type { ArtifactPresenceMember } from "~/protocol/index.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";
import { memberLocation } from "~/lib/artifacts/people.ts";
import { bySeat } from "~/lib/artifacts/follow.ts";

/** What a member is doing, in words: an agent reads where a person views. */
function doing(m: ArtifactPresenceMember): string {
  if (m.activity === "idle") return "Idle";
  if (m.activity === "editing") return "Editing";
  return m.kind === "agent" ? "Reading" : "Viewing";
}

export function PeopleRail({
  members,
  self,
  people,
  following,
  onFollow,
  onClose,
}: {
  /** Everybody in the room but the reader. */
  members: readonly ArtifactPresenceMember[];
  /** The reader's own seat, listed first as "You". */
  self: string;
  people: ArtifactPeople;
  /** The seat the reader follows, if any. */
  following: string;
  onFollow: (seat: string | null) => void;
  onClose: () => void;
}) {
  const others = bySeat(members).filter((m) => m.seat !== self);
  return (
    <aside className="artifact-people" aria-label="People here">
      <div className="artifact-people-head">
        <h2 className="artifact-people-title">People here</h2>
        <IconButton
          size="sm"
          variant="ghost"
          label="Close people"
          icon={<XGlyph size="sm" />}
          onClick={onClose}
        />
      </div>
      <ul className="artifact-people-list" role="list">
        {self && (
          <li className="artifact-person">
            <PrincipalAvatar principal={people.seatPrincipal(self)} size="sm" />
            <span className="artifact-person-text">
              <span className="artifact-person-name">You</span>
            </span>
          </li>
        )}
        {others.map((m) => {
          const name = people.seatName(m.seat);
          const where = memberLocation(m, (doc) => doc);
          const followed = following === m.seat;
          return (
            <li key={m.seat || m.session} className="artifact-person">
              <PrincipalAvatar principal={people.seatPrincipal(m.seat)} size="sm" />
              <span className="artifact-person-text">
                <span className="artifact-person-name">{name}</span>
                <span className="artifact-person-where">
                  {doing(m)}
                  {where ? ` ${where}` : ""}
                </span>
              </span>
              <span className="artifact-person-actions">
                {m.kind === "agent" && m.turn && (
                  <ButtonLink size="small" variant="ghost" href={href(["live", "turns", m.turn])}>
                    Open turn
                  </ButtonLink>
                )}
                <Button
                  size="small"
                  variant={followed ? "primary" : "secondary"}
                  pressed={followed}
                  aria-label={followed ? `Stop following ${name}` : `Follow ${name}`}
                  onClick={() => onFollow(followed ? null : m.seat)}
                >
                  {followed ? "Following" : "Follow"}
                </Button>
              </span>
            </li>
          );
        })}
      </ul>
      {others.length === 0 && <p className="artifact-note">Nobody else is here right now.</p>}
    </aside>
  );
}
