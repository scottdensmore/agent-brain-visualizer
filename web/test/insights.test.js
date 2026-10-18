import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderInsights, showInsights } from "../public/modules/insights.js";

const REPORT = {
  flavor: "codex",
  sessionCount: 10,
  sampledSessions: 10,
  analyzedSessions: 3,
  toolCallTotal: 42,
  sessionsWithErrors: 4,
  cleanSessions: 6,
  avgToolsPerSession: 4.2,
  avgDurationSeconds: 150,
  topTools: [
    { name: "Bash", count: 20 },
    { name: "Read", count: 10 },
  ],
  topErrors: [{ name: "NullPointerException at X", count: 3 }],
  topRecommendations: [{ name: "Add a lint rule", count: 2 }],
  topIssues: [{ name: "Flaky test", count: 1 }],
};

beforeEach(() => {
  document.body.innerHTML = '<div id="transcript-container"></div>';
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("renderInsights", () => {
  it("renders overview metrics, charts, and the backlog", () => {
    const c = document.getElementById("transcript-container");
    renderInsights(REPORT, c);
    const html = c.innerHTML;
    expect(html).toContain("Fleet Insights");
    expect(html).toContain("OpenAI Codex");
    expect(html).toContain("42"); // tool calls total
    expect(html).toContain("Bash");
    expect(html).toContain("NullPointerException at X");
    expect(html).toContain("Add a lint rule");
    expect(html).toContain("Flaky test");
    expect(html).toContain("2m 30s"); // avg duration
  });

  it("shows placeholders for an empty report without crashing", () => {
    const c = document.getElementById("transcript-container");
    renderInsights({ flavor: "codex", sessionCount: 0, sampledSessions: 0 }, c);
    const html = c.innerHTML;
    expect(html).toContain("No tool calls recorded");
    expect(html).toContain("No errors detected");
    expect(html).toContain("No recommendations yet");
  });

  it("escapes dynamic names so markup can't be injected", () => {
    const c = document.getElementById("transcript-container");
    renderInsights(
      { ...REPORT, topErrors: [{ name: "<script>x</script>", count: 1 }] },
      c
    );
    // No element is created from the malicious name; it stays inert text.
    expect(c.querySelector("script")).toBeNull();
    expect(c.innerHTML).toContain("&lt;script&gt;");
  });

  it("tags each tally row with its drill-down category and key", () => {
    const c = document.getElementById("transcript-container");
    renderInsights(REPORT, c);
    const toolRow = c.querySelector('.drill-row[data-drill-category="tool"]');
    expect(toolRow).not.toBeNull();
    expect(toolRow.dataset.drillKey).toBe("Bash");
    expect(c.querySelector('.drill-row[data-drill-category="error"]').dataset.drillKey).toBe(
      "NullPointerException at X"
    );
  });
});

describe("drill-down", () => {
  it("expands a row into the sessions behind it and can open one", async () => {
    const c = document.getElementById("transcript-container");
    // A stand-in sidebar item the drill-down should click to open the session.
    const conv = document.createElement("div");
    conv.className = "conv-item";
    conv.dataset.id = "sess-1";
    const convClick = vi.fn();
    conv.addEventListener("click", convClick);
    document.body.appendChild(conv);

    renderInsights(REPORT, c);
    global.fetch = vi.fn(() =>
      Promise.resolve({
        ok: true,
        json: () =>
          Promise.resolve({
            category: "error",
            key: "NullPointerException at X",
            totalMatches: 1,
            sessions: [{ id: "sess-1", title: "Parser fix" }],
          }),
      })
    );

    c.querySelector('.drill-row[data-drill-category="error"] .drill-bar').click();
    await new Promise((r) => setTimeout(r, 0));

    expect(global.fetch).toHaveBeenCalledWith(expect.stringContaining("/api/insights/sessions?flavor=codex&category=error&key="), expect.any(Object));
    const sub = c.querySelector('.drill-row[data-drill-category="error"] .drill-sessions');
    expect(sub.innerHTML).toContain("Parser fix");

    sub.querySelector(".drill-session").click();
    expect(convClick).toHaveBeenCalled();
  });

  it("shows an error in the row when the drill-down request fails", async () => {
    const c = document.getElementById("transcript-container");
    renderInsights(REPORT, c);
    global.fetch = vi.fn(() => Promise.resolve({ ok: false, status: 500 }));

    c.querySelector('.drill-row[data-drill-category="tool"] .drill-bar').click();
    await new Promise((r) => setTimeout(r, 0));

    const sub = c.querySelector('.drill-row[data-drill-category="tool"] .drill-sessions');
    expect(sub.innerHTML).toContain("Failed to load sessions");
  });
});

describe("showInsights", () => {
  it("fetches the report for a flavor and renders it", async () => {
    global.fetch = vi.fn(() =>
      Promise.resolve({ ok: true, json: () => Promise.resolve(REPORT) })
    );
    await showInsights("codex");
    expect(global.fetch).toHaveBeenCalledWith(expect.stringContaining("/api/insights?flavor=codex"), expect.any(Object));
    expect(document.getElementById("transcript-container").innerHTML).toContain("Bash");
  });

  it("shows an error message when the request fails", async () => {
    global.fetch = vi.fn(() => Promise.reject(new Error("boom")));
    await showInsights("codex");
    expect(document.getElementById("transcript-container").innerHTML).toContain(
      "Failed to load insights"
    );
  });
});
