/**
 * The Google Analytics logo beside what Crewlet read from Google Analytics, as
 * Google's developer branding policy asks: its own file, unaltered, 50px high
 * (the horizontal logo's floor), linked to Google Analytics, one per ground.
 * Shown on Google Analytics' own page only; the kit's TRADEMARKS.md says why.
 */

import onLight from "@crewlethq/icons/attribution/google-analytics/lockup_ic_Analytics_horiz_272px_clr.png";
import onDark from "@crewlethq/icons/attribution/google-analytics/lockup_ic_Analytics_horiz_272px_wht.png";
import "~/styles/growth.css";

/** Into the Google Analytics product, which the policy names as a linkback. */
export const GOOGLE_ANALYTICS_HOME = "https://analytics.google.com/";

/** The logo's 1519 by 402 file drawn at the policy's 50px height. */
const HEIGHT = 50;
const WIDTH = Math.round((HEIGHT * 1519) / 402);

export function AnalyticsAttribution() {
  return (
    <p className="growth-attribution">
      <span>Read from</span>
      <a
        className="growth-attribution__link"
        href={GOOGLE_ANALYTICS_HOME}
        target="_blank"
        rel="noreferrer"
        aria-label="Google Analytics (opens in a new tab)"
      >
        {/* The stylesheet shows the one for the theme's ground; the link carries the name. */}
        <img
          className="growth-attribution__logo growth-attribution__logo--light"
          src={onLight}
          alt=""
          width={WIDTH}
          height={HEIGHT}
        />
        <img
          className="growth-attribution__logo growth-attribution__logo--dark"
          src={onDark}
          alt=""
          width={WIDTH}
          height={HEIGHT}
        />
      </a>
    </p>
  );
}
