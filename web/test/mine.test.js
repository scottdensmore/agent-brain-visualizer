import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderMining, showMining } from "../public/modules/mine.js";

const REPORT = {
  flavor: "claude-code",
  sessionCount: 12,
  sampledSessions: 12,
  analyzedSessions: 5,
  aiGenerated: true,
  note: "",
  toolSequences: [{ name: "Read → Edit → Bash", count: 4 }],
  failureFixes: [
    { error: "Build fails on JDK 21", fix: "Use JDK 25 via mise", count: 3 },
  ],
  recommendations: [{ name: "Pin the JDK with mise", count: 2 }],
  skills: [
    {
      name: "edit-and-verify",
      whenToUse: "When editing then checking a build",
      body: "1. Read 2. Edit 3. Bash",
    },
  ],
  agentsRules: [
    { rule: "Build with JDK 25 via mise", rationale: "JDK 21 cannot load the plugins" },
  ],
  toolingGaps: ["A one-shot build-and-test command"],
};

beforeEach(() => {
  document.body.innerHTML = '<div id="transcript-container"></div>';
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("renderMining", () => {
  it("renders proposals and evidence when AI generated them", () => {
    const c = document.getElementById("transcript-container");
    renderMining(REPORT, c);
    const html = c.innerHTML;
    expect(html).toContain("Mined Improvements");
    expect(html).toContain("Claude Code");
    // Proposals.
    expect(html).toContain("edit-and-verify");
    expect(html).toContain("Build with JDK 25 via mise");
    expect(html).toContain("A one-shot build-and-test command");
    // Evidence.
    expect(html).toContain("Read → Edit → Bash");
    expect(html).toContain("Build fails on JDK 21");
    expect(html).toContain("Pin the JDK with mise");
  });

  it("hides proposal sections and shows the note when AI did not run", () => {
    const c = document.getElementById("transcript-container");
    renderMining(
      {
        ...REPORT,
        aiGenerated: false,
        note: "Configure an AI provider to generate skills and AGENTS.md rules.",
        skills: [],
        agentsRules: [],
        toolingGaps: [],
      },
      c
    );
    const html = c.innerHTML;
    expect(html).toContain("Configure an AI provider");
    // No proposal sections, but the evidence still renders.
    expect(html).not.toContain("Proposed Skills");
    expect(html).toContain("Read → Edit → Bash");
  });

  it("escapes dynamic names so markup can't be injected", () => {
    const c = document.getElementById("transcript-container");
    renderMining(
      { ...REPORT, toolSequences: [{ name: "<script>x</script>", count: 1 }] },
      c
    );
    expect(c.querySelector("script")).toBeNull();
    expect(c.innerHTML).toContain("&lt;script&gt;");
  });

  it("shows placeholders for an empty report without crashing", () => {
    const c = document.getElementById("transcript-container");
    renderMining({ flavor: "codex", sessionCount: 0, aiGenerated: false }, c);
    const html = c.innerHTML;
    expect(html).toContain("No recurring tool sequences yet");
    expect(html).toContain("No failures recorded");
  });

  it("makes the evidence rows drillable by their category", () => {
    const c = document.getElementById("transcript-container");
    renderMining(REPORT, c);
    expect(
      c.querySelector('.drill-row[data-drill-category="workflow"]').dataset.drillKey
    ).toBe("Read → Edit → Bash");
    expect(
      c.querySelector('.drill-row[data-drill-category="issue"]').dataset.drillKey
    ).toBe("Build fails on JDK 21");
    expect(
      c.querySelector('.drill-row[data-drill-category="recommendation"]').dataset.drillKey
    ).toBe("Pin the JDK with mise");
  });

  it("offers download buttons only when the model proposed skills/rules", () => {
    const c = document.getElementById("transcript-container");
    renderMining(REPORT, c);
    expect(c.querySelector(".skill-dl-btn")).not.toBeNull();
    expect(c.querySelector("#agents-dl-btn")).not.toBeNull();

    // Evidence-only (no AI proposals) => no download buttons.
    renderMining({ ...REPORT, aiGenerated: false, skills: [], agentsRules: [] }, c);
    expect(c.querySelector(".skill-dl-btn")).toBeNull();
    expect(c.querySelector("#agents-dl-btn")).toBeNull();
  });

  it("downloading a skill builds a SKILL.md blob named from its slug", () => {
    const c = document.getElementById("transcript-container");
    renderMining(REPORT, c);
    const created = [];
    global.URL.createObjectURL = vi.fn(() => "blob:x");
    global.URL.revokeObjectURL = vi.fn();
    const clickSpy = vi
      .spyOn(HTMLAnchorElement.prototype, "click")
      .mockImplementation(function () {
        created.push({ download: this.download });
      });

    c.querySelector(".skill-dl-btn").click();

    expect(global.URL.createObjectURL).toHaveBeenCalledWith(expect.any(Blob));
    expect(created[0].download).toBe("edit-and-verify.md");
    clickSpy.mockRestore();
  });

  it("downloading AGENTS.md triggers a download", () => {
    const c = document.getElementById("transcript-container");
    renderMining(REPORT, c);
    global.URL.createObjectURL = vi.fn(() => "blob:x");
    global.URL.revokeObjectURL = vi.fn();
    let name = null;
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function () {
      name = this.download;
    });

    c.querySelector("#agents-dl-btn").click();

    expect(name).toBe("AGENTS.md");
    expect(global.URL.createObjectURL).toHaveBeenCalledWith(expect.any(Blob));
  });
});

describe("showMining", () => {
  it("fetches the report for a flavor and renders it", async () => {
    global.fetch = vi.fn(() =>
      Promise.resolve({ ok: true, json: () => Promise.resolve(REPORT) })
    );
    await showMining("claude-code");
    expect(global.fetch).toHaveBeenCalledWith(expect.stringContaining("/api/mine?flavor=claude-code"), expect.any(Object));
    expect(document.getElementById("transcript-container").innerHTML).toContain(
      "edit-and-verify"
    );
  });

  it("shows an error message when the request fails", async () => {
    global.fetch = vi.fn(() => Promise.reject(new Error("boom")));
    await showMining("codex");
    expect(document.getElementById("transcript-container").innerHTML).toContain(
      "Failed to mine improvements"
    );
  });

  it("shows an error message on a non-ok response", async () => {
    global.fetch = vi.fn(() => Promise.resolve({ ok: false, status: 500 }));
    await showMining("codex");
    expect(document.getElementById("transcript-container").innerHTML).toContain(
      "Failed to mine improvements"
    );
  });
});
