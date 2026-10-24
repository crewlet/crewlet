/**
 * The page every sign-in screen is drawn on: `@crewlethq/ui`'s `AuthLayout`,
 * the product's lockup over one card.
 *
 * OUTSIDE THE FRAME, deliberately. See `FRAMELESS` in `app/nav.ts`: a browser
 * here holds no session the frame could use, so the rail, the sidebars and
 * the state bar would each be chrome over questions the engine will refuse.
 *
 * THE TAB SAYS WHERE YOU ARE, as the frame's own title does: a person with a
 * sign-in open in one tab and the product in another needs to tell them
 * apart. That is the one thing this wrapper adds.
 */

import { useEffect, type ReactNode } from "react";
import { AuthLayout } from "@crewlethq/ui";

export function SignInPage({
  title,
  lede,
  children,
}: {
  title: string;
  /** One or two sentences under the title saying what this screen is for. */
  lede?: ReactNode;
  /** The form, or a message's one action. A card whose title and lede say it all has none. */
  children?: ReactNode;
}) {
  useEffect(() => {
    document.title = `${title} · Crewlet`;
  }, [title]);
  return (
    <AuthLayout title={title} description={lede}>
      {children}
    </AuthLayout>
  );
}
