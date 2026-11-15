import { describe, expect, it } from "vitest";
import { companyDayOf, fmtCompanyDay } from "./companyDay.ts";
import { fmtCivilDay } from "./format.ts";

/**
 * A DAY FIELD is a calendar date, never an instant: a due or start date travels
 * as the company's midnight, and its day is the company's for every reader.
 * This suite reads in Berlin, so a Tokyo company's midnight (15:00 the day
 * before, UTC) is the afternoon before here: read as an instant, Nov 30 read
 * Nov 29.
 */
describe("a day field", () => {
  const tokyoMidnight = "2026-11-29T15:00:00Z"; // 30 November in Tokyo

  // Mutation: read the instant on the reader's clock and Nov 29 comes back.
  it("is the company's date of the instant it travels as", () => {
    expect(companyDayOf(tokyoMidnight, "Asia/Tokyo")).toBe("2026-11-30");
    expect(companyDayOf("2026-11-30T05:00:00Z", "America/Los_Angeles")).toBe("2026-11-29");
    expect(fmtCompanyDay(tokyoMidnight, "Asia/Tokyo")).toBe("Nov 30, 2026");
  });

  it("is a bare date as itself, in every zone", () => {
    expect(companyDayOf("2026-11-30", "America/Los_Angeles")).toBe("2026-11-30");
    expect(fmtCompanyDay("2026-11-30", "Pacific/Kiritimati")).toBe("Nov 30, 2026");
    expect(fmtCivilDay("2026-11-30")).toBe("Nov 30, 2026");
  });

  it("drops the year inside the company's current year, given now", () => {
    const now = Date.parse("2026-06-01T12:00:00Z");
    expect(fmtCompanyDay(tokyoMidnight, "Asia/Tokyo", now)).toBe("Nov 30");
    expect(fmtCompanyDay("2027-01-04", "Asia/Tokyo", now)).toBe("Jan 04, 2027");
  });

  it("is nothing where there is no date, and the value where it is not one", () => {
    expect(companyDayOf("", "UTC")).toBe("");
    expect(companyDayOf(undefined, "UTC")).toBe("");
    expect(companyDayOf("2026-02-30", "UTC")).toBe("");
    expect(companyDayOf("soon", "UTC")).toBe("");
    expect(fmtCompanyDay("soon", "UTC")).toBe("soon");
  });
});
