/**
 * The page every sign-in screen is drawn on: the mark, and one card.
 *
 * OUTSIDE THE FRAME, deliberately — see `FRAMELESS` in `app/nav.ts`. A
 * browser here holds no session the frame could use, so the rail, the
 * sidebars and the state bar would each be chrome over questions the engine
 * will refuse, and a rail row offering "Admin" to somebody who has not said
 * who they are is a promise the next click breaks.
 *
 * THE TAB SAYS WHERE YOU ARE, as the frame's own title does: a person with a
 * sign-in open in one tab and the product in another needs to tell them
 * apart.
 */

import { useEffect, useId, type ReactNode } from "react";
import { Card } from "@crewlethq/ui";

export function SignInPage({
  title,
  lede,
  children,
}: {
  title: string;
  /** One or two sentences under the title saying what this screen is for. */
  lede?: ReactNode;
  children: ReactNode;
}) {
  const heading = useId();
  useEffect(() => {
    document.title = `${title} · Crewlet`;
  }, [title]);
  return (
    <main className="signin">
      <div className="signin-column">
        {/* DECORATION: the card's heading names the page, and the mark
            beside it is the product's, not a control. */}
        <img className="signin-mark" src="/static/dashboard/crewlet-icon.svg" alt="" />
        <Card as="section" padding="lg" aria-labelledby={heading}>
          <Card.Title as="h1" id={heading}>
            {title}
          </Card.Title>
          {lede && <Card.Description>{lede}</Card.Description>}
          <div className="signin-body">{children}</div>
        </Card>
      </div>
    </main>
  );
}
