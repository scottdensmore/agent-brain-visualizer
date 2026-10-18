import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  renderEval,
  showEval,
  renderComparison,
  scoreSparkline,
  sparkline,
} from "../public/modules/eval.js";

const REPORT = {
  flavor: "antigravity-cli",
  sessionCount: 20,
  sampledSessions: 20,
  evaluatedSessions: 8,
  avgScore: 72.5,
  modelLabel: "gemini · gemini-3.5-flash",
  checkPassRates: [
    { name: "schema-complete", count: 8 },
    { name: "has-recommendations", count: 6 },
    { name: "not-degenerate", count: 4 },
  ],
  worstCases: [
    { sessionId: "s-bad", title: "Broken run", score: 33, passed: [], failed: ["schema-complete", "not-degenerate"] },
  ],
  judge: {
    ran: false,
    note: "Run the LLM judge to add faithfulness / actionability / clarity scores.",
    judgedSessions: 0,
    cases: [],
  },
};

const JUDGED = {
  ...REPORT,
  judge: {
    ran: true,
    note: "",
    judgedSessions: 2,
    avgFaithfulness: 4.5,
    avgActionability: 3,
    avgClarity: 4,
    cases: [
      {
        sessionId: "s1",
        title: "Parser fix",
        faithfulness: 4.7,
        actionability: 3,
        clarity: 4,
        samples: 3,
        panelMin: 3.3,
        panelMax: 4.7,
        comment: "Accurate and specific.",
      },
    ],
  },
};

beforeEach(() => {
  document.body.innerHTML = '<div id="transcript-container"></div>';
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("renderEval", () => {
  it("renders the scoreboard, pass-rates, and worst cases", () => {
    const c = document.getElementById("transcript-container");
    renderEval(REPORT, c);
    const html = c.innerHTML;
    expect(html).toContain("Analysis Eval");
    expect(html).toContain("Antigravity CLI");
    expect(html).toContain("gemini · gemini-3.5-flash");
    expect(html).toContain("72.5"); // avg score
    expect(html).toContain("Has title, summary &amp; flow"); // check label
    expect(html).toContain("6/8"); // pass-rate fraction
    expect(html).toContain("Broken run"); // worst case
    expect(html).toContain("33"); // its score
  });

  it("shows a prompt to analyze when nothing has been evaluated", () => {
    const c = document.getElementById("transcript-container");
    renderEval({ ...REPORT, evaluatedSessions: 0, checkPassRates: [], worstCases: [] }, c);
    const html = c.innerHTML;
    expect(html).toContain("No analyzed sessions yet");
    // Overview cards still render.
    expect(html).toContain("AVG SCORE");
  });

  it("shows a Run-LLM-judge button and note when the judge has not run", () => {
    const c = document.getElementById("transcript-container");
    renderEval(REPORT, c);
    const html = c.innerHTML;
    expect(c.querySelector("#run-judge-btn")).not.toBeNull();
    expect(html).toContain("Run the LLM judge");
    // No rubric section yet.
    expect(html).not.toContain("FAITHFULNESS");
  });

  it("renders the rubric averages and per-case scores when the judge ran", () => {
    const c = document.getElementById("transcript-container");
    renderEval(JUDGED, c);
    const html = c.innerHTML;
    expect(html).toContain("FAITHFULNESS");
    expect(html).toContain("4.5"); // avg faithfulness
    expect(html).toContain("2 judged");
    expect(html).toContain("Parser fix");
    expect(html).toContain("Accurate and specific.");
    // Per-case shows the panel size and the ensembled (fractional) faithfulness.
    expect(html).toContain("3-judge panel");
    expect(html).toContain("F 4.7");
    // The panel spread (min–max of the lenses' overall scores) is shown.
    expect(html).toContain("panel 3.3–4.7");
    // And the aggregate agreement indicator (avg spread = 4.7 − 3.3 = 1.4).
    expect(html).toContain("Panel agreement");
    expect(html).toContain("±1.4");
  });

  it("omits the agreement indicator when no case has a multi-lens panel", () => {
    const c = document.getElementById("transcript-container");
    const single = {
      ...JUDGED,
      judge: {
        ...JUDGED.judge,
        cases: [{ ...JUDGED.judge.cases[0], samples: 1, panelMin: 4, panelMax: 4 }],
      },
    };
    renderEval(single, c);
    expect(c.innerHTML).not.toContain("Panel agreement");
    // The button is gone once the judge has run.
    expect(c.querySelector("#run-judge-btn")).toBeNull();
  });

  it("clicking Run-LLM-judge re-fetches with judge=true", async () => {
    const c = document.getElementById("transcript-container");
    renderEval(REPORT, c);
    global.fetch = vi.fn(() =>
      Promise.resolve({ ok: true, json: () => Promise.resolve(JUDGED) })
    );
    c.querySelector("#run-judge-btn").click();
    await Promise.resolve();
    await Promise.resolve();
    expect(global.fetch).toHaveBeenCalledWith(expect.stringContaining("judge=true"), expect.any(Object));
  });

  it("renders run history with a delta vs the previous run", () => {
    const c = document.getElementById("transcript-container");
    const history = [
      { savedAt: "2026-07-02T10:00:00Z", flavor: "antigravity-cli", modelLabel: "gemini · v2", evaluatedSessions: 8, avgScore: 75, judged: false },
      { savedAt: "2026-07-01T10:00:00Z", flavor: "antigravity-cli", modelLabel: "gemini · v1", evaluatedSessions: 8, avgScore: 70, judged: false },
    ];
    renderEval(REPORT, c, history);
    const html = c.innerHTML;
    expect(html).toContain("Run history");
    expect(html).toContain("gemini · v2");
    expect(html).toContain("+5"); // 75 - 70
    // A CSV export is offered once there is history.
    expect(c.querySelector("#history-csv-btn")).not.toBeNull();
    // The trend sparkline renders once there are two runs.
    expect(c.querySelector("svg polyline")).not.toBeNull();
  });

  it("has no CSV button when there is no history", () => {
    const c = document.getElementById("transcript-container");
    renderEval(REPORT, c, []);
    expect(c.querySelector("#history-csv-btn")).toBeNull();
  });

  it("compares two ticked runs (A older, B newer) with deltas", () => {
    const c = document.getElementById("transcript-container");
    const history = [
      // newest first
      { savedAt: "2026-07-02T10:00:00Z", flavor: "codex", modelLabel: "gemini · v2", evaluatedSessions: 8, avgScore: 80, judged: false, checkPassRates: [{ name: "schema-complete", count: 8 }] },
      { savedAt: "2026-07-01T10:00:00Z", flavor: "codex", modelLabel: "gemini · v1", evaluatedSessions: 8, avgScore: 70, judged: false, checkPassRates: [{ name: "schema-complete", count: 6 }] },
    ];
    renderEval(REPORT, c, history);
    const boxes = c.querySelectorAll(".cmp-check");
    expect(boxes).toHaveLength(2);

    boxes[0].checked = true;
    boxes[0].dispatchEvent(new Event("change"));
    // Only one ticked → no comparison yet.
    expect(c.querySelector("#run-compare").innerHTML).toBe("");

    boxes[1].checked = true;
    boxes[1].dispatchEvent(new Event("change"));
    const cmp = c.querySelector("#run-compare").innerHTML;
    // A = older (v1), B = newer (v2); score went 70 -> 80 => +10.
    expect(cmp).toContain("gemini · v1");
    expect(cmp).toContain("gemini · v2");
    expect(cmp).toContain("Avg score");
    expect(cmp).toContain("+10");
    // Per-check row present with its +2 delta (6 -> 8).
    expect(cmp).toContain("Has title, summary &amp; flow");
    expect(cmp).toContain("+2");
  });

  it("clears the comparison when not exactly two runs are ticked", () => {
    const c = document.getElementById("transcript-container");
    const history = [
      { savedAt: "t2", flavor: "codex", modelLabel: "b", avgScore: 80 },
      { savedAt: "t1", flavor: "codex", modelLabel: "a", avgScore: 70 },
    ];
    renderEval(REPORT, c, history);
    const boxes = c.querySelectorAll(".cmp-check");
    boxes[0].checked = true;
    boxes[0].dispatchEvent(new Event("change"));
    boxes[1].checked = true;
    boxes[1].dispatchEvent(new Event("change"));
    expect(c.querySelector("#run-compare").innerHTML).not.toBe("");
    // Untick one → comparison clears.
    boxes[0].checked = false;
    boxes[0].dispatchEvent(new Event("change"));
    expect(c.querySelector("#run-compare").innerHTML).toBe("");
  });

  it("clicking a run's × deletes it then refreshes the history", async () => {
    const c = document.getElementById("transcript-container");
    const history = [
      { savedAt: "2026-07-02T10:00:00Z", flavor: "antigravity-cli", modelLabel: "b", avgScore: 80 },
      { savedAt: "2026-07-01T10:00:00Z", flavor: "antigravity-cli", modelLabel: "a", avgScore: 70 },
    ];
    renderEval(REPORT, c, history);
    const calls = [];
    global.fetch = vi.fn((url, opts) => {
      calls.push({ url, opts });
      const isDelete = opts && opts.method === "DELETE";
      return Promise.resolve({
        ok: true,
        json: () =>
          Promise.resolve(isDelete ? { deleted: 1 } : []), // history refetch returns []
      });
    });

    c.querySelector(".run-del-btn").click();
    await new Promise((r) => setTimeout(r, 0));

    const del = calls.find((x) => x.opts && x.opts.method === "DELETE");
    expect(del).toBeTruthy();
    expect(del.url).toContain("savedAt=2026-07-02T10%3A00%3A00Z");
    // History refetched and re-rendered (now empty).
    expect(c.innerHTML).toContain("No saved runs yet");
  });

  it("clicking CSV downloads the history as a .csv file", () => {
    const c = document.getElementById("transcript-container");
    const history = [
      { savedAt: "t", flavor: "antigravity-cli", modelLabel: "gemini", avgScore: 75 },
    ];
    renderEval(REPORT, c, history);
    global.URL.createObjectURL = vi.fn(() => "blob:x");
    global.URL.revokeObjectURL = vi.fn();
    let name = null;
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function () {
      name = this.download;
    });

    c.querySelector("#history-csv-btn").click();

    expect(name).toBe("eval-history-antigravity-cli.csv");
    expect(global.URL.createObjectURL).toHaveBeenCalledWith(expect.any(Blob));
  });

  it("shows an empty-history note and a Save button when nothing is saved", () => {
    const c = document.getElementById("transcript-container");
    renderEval(REPORT, c, []);
    expect(c.innerHTML).toContain("No saved runs yet");
    expect(c.querySelector("#save-run-btn")).not.toBeNull();
  });

  it("clicking Save run POSTs the report then refreshes history", async () => {
    const c = document.getElementById("transcript-container");
    renderEval(REPORT, c, []);
    const calls = [];
    global.fetch = vi.fn((url, opts) => {
      calls.push({ url, opts });
      const isPost = opts && opts.method === "POST";
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve(isPost ? { savedAt: "x" } : []),
      });
    });
    c.querySelector("#save-run-btn").click();
    await new Promise((r) => setTimeout(r, 0));
    const post = calls.find((c) => c.opts && c.opts.method === "POST");
    expect(post).toBeTruthy();
    expect(post.url).toBe("/api/eval/runs");
    expect(JSON.parse(post.opts.body).flavor).toBe("antigravity-cli");
  });

  it("escapes dynamic values so markup can't be injected", () => {
    const c = document.getElementById("transcript-container");
    renderEval(
      { ...REPORT, worstCases: [{ sessionId: "x", title: "<script>x</script>", score: 0, failed: [] }] },
      c
    );
    expect(c.querySelector("script")).toBeNull();
    expect(c.innerHTML).toContain("&lt;script&gt;");
  });
});

describe("sparkline", () => {
  it("draws a polyline with one point per value", () => {
    const svg = sparkline([0.2, 0.5, 0.8], 1);
    const points = svg.match(/points="([^"]+)"/)[1].trim().split(/\s+/);
    expect(points).toHaveLength(3);
    expect(svg).not.toContain("<circle"); // no marker by default
  });

  it("is empty below two values and clamps to the domain (no NaN)", () => {
    expect(sparkline([], 1)).toBe("");
    expect(sparkline([0.5], 1)).toBe("");
    expect(sparkline([2, -1], 1)).not.toContain("NaN");
  });

  it("adds a marker dot when requested", () => {
    expect(sparkline([1, 2], 5, { marker: true })).toContain("<circle");
  });
});

describe("per-check pass-rate trend", () => {
  it("renders a sparkline on each check row once there are two saved runs", () => {
    const c = document.getElementById("transcript-container");
    const report = {
      flavor: "codex",
      sessionCount: 10,
      evaluatedSessions: 8,
      avgScore: 75,
      checkPassRates: [{ name: "schema-complete", count: 8 }],
      worstCases: [],
      judge: { ran: false, note: "", cases: [] },
    };
    const history = [
      { savedAt: "t2", flavor: "codex", evaluatedSessions: 8, avgScore: 80, checkPassRates: [{ name: "schema-complete", count: 8 }] },
      { savedAt: "t1", flavor: "codex", evaluatedSessions: 8, avgScore: 70, checkPassRates: [{ name: "schema-complete", count: 4 }] },
    ];
    renderEval(report, c, history);
    // The pass-rate section's check row carries an inline (non-marker) sparkline.
    const inlineSparks = c.querySelectorAll('svg[style*="inline-block"]');
    expect(inlineSparks.length).toBeGreaterThan(0);
  });
});

describe("scoreSparkline", () => {
  it("draws a polyline with one point per run once there are at least two", () => {
    const history = [
      { savedAt: "t3", avgScore: 80 },
      { savedAt: "t2", avgScore: 60 },
      { savedAt: "t1", avgScore: 70 },
    ];
    const svg = scoreSparkline(history);
    expect(svg).toContain("<polyline");
    // 3 runs => 3 "x,y" point pairs.
    const points = svg.match(/points="([^"]+)"/)[1].trim().split(/\s+/);
    expect(points).toHaveLength(3);
    expect(svg).toContain("<circle"); // latest-value marker
  });

  it("is empty with fewer than two runs", () => {
    expect(scoreSparkline([])).toBe("");
    expect(scoreSparkline([{ avgScore: 50 }])).toBe("");
  });

  it("clamps scores into the chart height (no NaN coordinates)", () => {
    const svg = scoreSparkline([{ avgScore: 250 }, { avgScore: -5 }]);
    expect(svg).not.toContain("NaN");
  });
});

describe("renderComparison", () => {
  it("includes rubric rows only when both runs were judged", () => {
    const older = {
      savedAt: "t1", modelLabel: "a", avgScore: 70, evaluatedSessions: 8,
      judged: true, avgFaithfulness: 4, avgActionability: 3, avgClarity: 4,
    };
    const newer = {
      savedAt: "t2", modelLabel: "b", avgScore: 80, evaluatedSessions: 8,
      judged: true, avgFaithfulness: 4.5, avgActionability: 3, avgClarity: 3,
    };
    const html = renderComparison(older, newer);
    expect(html).toContain("Faithfulness");
    expect(html).toContain("+0.5"); // 4 -> 4.5
    expect(html).toContain("Clarity");
    expect(html).toContain("-1"); // 4 -> 3

    // If either run isn't judged, no rubric rows.
    expect(renderComparison(older, { ...newer, judged: false })).not.toContain(
      "Faithfulness"
    );
  });

  it("returns empty when a run is missing", () => {
    expect(renderComparison(null, { avgScore: 1 })).toBe("");
  });
});

describe("showEval", () => {
  it("fetches and renders the report for a flavor", async () => {
    global.fetch = vi.fn(() =>
      Promise.resolve({ ok: true, json: () => Promise.resolve(REPORT) })
    );
    await showEval("antigravity-cli");
    expect(global.fetch).toHaveBeenCalledWith(expect.stringContaining("/api/eval?flavor=antigravity-cli"), expect.any(Object));
    expect(document.getElementById("transcript-container").innerHTML).toContain("Analysis Eval");
  });

  it("also fetches the run history for the flavor", async () => {
    global.fetch = vi.fn((url) =>
      Promise.resolve({
        ok: true,
        json: () => Promise.resolve(url.includes("/runs") ? [] : REPORT),
      })
    );
    await showEval("antigravity-cli");
    expect(global.fetch).toHaveBeenCalledWith(expect.stringContaining("/api/eval/runs?flavor=antigravity-cli"), expect.any(Object));
  });

  it("a superseded (slower, earlier) load does not clobber a newer one", async () => {
    // The first-started flavor resolves LAST; generation gating must discard its stale render.
    global.fetch = vi.fn((url) => {
      const slow = url.includes("slow");
      const isRuns = url.includes("/runs");
      const body = isRuns ? [] : { flavor: slow ? "slow" : "fast" };
      return new Promise((resolve) =>
        setTimeout(
          () => resolve({ ok: true, json: () => Promise.resolve(body) }),
          slow ? 50 : 5
        )
      );
    });
    const slowP = showEval("slow"); // generation N
    const fastP = showEval("fast"); // generation N+1, resolves first
    await Promise.all([slowP, fastP]);
    await new Promise((r) => setTimeout(r, 70)); // let the slow one resolve and be discarded
    const html = document.getElementById("transcript-container").innerHTML;
    expect(html).toContain("fast");
    expect(html).not.toContain("slow");
  });

  it("shows an error message on a non-ok response", async () => {
    global.fetch = vi.fn(() => Promise.resolve({ ok: false, status: 500 }));
    await showEval("codex");
    expect(document.getElementById("transcript-container").innerHTML).toContain(
      "Failed to load the eval"
    );
  });

  it("shows an error message when the request rejects", async () => {
    global.fetch = vi.fn(() => Promise.reject(new Error("boom")));
    await showEval("codex");
    expect(document.getElementById("transcript-container").innerHTML).toContain(
      "Failed to load the eval"
    );
  });
});
