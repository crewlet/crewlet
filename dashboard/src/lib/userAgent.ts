/**
 * A browser and its system as a person names them ("Firefox on macOS"), read
 * from the User-Agent a session was opened with.
 *
 * FOR TELLING ONE'S OWN SESSIONS APART, NOTHING MORE. A User-Agent is a claim
 * any client makes about itself, so nothing here decides anything: it labels
 * a row a person reads to choose which session to sign out. The order of the
 * checks is the one the strings force: every Chromium browser says "Chrome"
 * and "Safari", Edge and Opera say Chrome too, and Chrome on iOS says Safari,
 * so the most specific name is asked first.
 */

/** The browser a User-Agent names, or "" where it names none this knows. */
function browserOf(ua: string): string {
  const named: [RegExp, string][] = [
    [/\bEdg(e|A|iOS)?\//, "Edge"],
    [/\bOPR\/|\bOpera\b/, "Opera"],
    [/\bSamsungBrowser\//, "Samsung Internet"],
    [/\bFirefox\/|\bFxiOS\//, "Firefox"],
    [/\bCriOS\/|Chrome\//, "Chrome"],
    [/\bVersion\/[\d.]+.*\bSafari\//, "Safari"],
    [/\bcurl\//, "curl"],
  ];
  return named.find(([pattern]) => pattern.test(ua))?.[1] ?? "";
}

/** The system a User-Agent names, or "" where it names none this knows. */
function systemOf(ua: string): string {
  const named: [RegExp, string][] = [
    [/\biPhone\b/, "iPhone"],
    [/\biPad\b/, "iPad"],
    [/\bAndroid\b/, "Android"],
    [/\bCrOS\b/, "ChromeOS"],
    [/\bMac OS X\b|\bMacintosh\b/, "macOS"],
    [/\bWindows\b/, "Windows"],
    [/\bLinux\b/, "Linux"],
  ];
  return named.find(([pattern]) => pattern.test(ua))?.[1] ?? "";
}

/**
 * The device a session was opened from, as a person names it: "Firefox on
 * macOS", "Safari on iPhone", the browser or the system alone where only one
 * is known, and "Unknown browser" where the session named one this cannot
 * read. Empty for a session that named none (a token's sign-in with no
 * browser behind it, or one an earlier engine opened).
 */
export function deviceOf(ua: string | undefined): string {
  const text = (ua ?? "").trim();
  if (!text) return "";
  const browser = browserOf(text);
  const system = systemOf(text);
  if (browser && system) return `${browser} on ${system}`;
  return browser || system || "Unknown browser";
}
