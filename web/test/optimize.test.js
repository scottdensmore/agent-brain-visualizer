import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  renderLab,
  renderOptimizeResults,
  showOptimize,
} from "../public/modules/optimize.js";

beforeEach(() => {
  document.body.innerHTML = '<div id="transcript-container"></div>';
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("renderOptimizeResults", () => {
  const REPORT = {
    sampleSize: 3,
    note: "",
    a: { avgScore: 90, scored: 3, checkPassRates: [{ name: "schema-complete", count: 3 }] },
    b: { avgScore: 70, scored: 3, checkPassRates: [{ name: "schema-complete", count: 2 }] },
  };

  it("declares the winner and shows both variants' scores", () => {
    const html = renderOptimizeResults(REPORT);
    expect(html).toContain("Prompt A wins by +20");
    expect(html).toContain("90");
    expect(html).toContain("70");
    expect(html).toContain("Has title, summary &amp; flow");
    expect(html).toContain("3 sessions sampled");
  });

  it("calls a higher B the winner", () => {
    const html = renderOptimizeResults({ ...REPORT, a: { ...REPORT.a, avgScore: 70 }, b: { ...REPORT.b, avgScore: 90 } });
    expect(html).toContain("Prompt B wins by +20");
  });

  it("shows the note when the run couldn't proceed", () => {
    const html = renderOptimizeResults({ sampleSize: 0, note: "Configure an AI provider to run the prompt lab." });
    expect(html).toContain("Configure an AI provider");
  });
});

describe("renderLab", () => {
  it("seeds both prompt editors with the default instruction and escapes it", () => {
    const c = document.getElementById("transcript-container");
    renderLab("codex", { instruction: "Analyze <it>", maxSample: 5 }, c);
    const a = c.querySelector("#opt-a");
    const b = c.querySelector("#opt-b");
    expect(a).not.toBeNull();
    expect(b).not.toBeNull();
    expect(a.value).toBe("Analyze <it>"); // textarea value is the unescaped text
    expect(c.querySelector("script")).toBeNull();
    expect(c.querySelector("#opt-run")).not.toBeNull();
  });

  it("posts both prompts and the sample size when Run is clicked", async () => {
    const c = document.getElementById("transcript-container");
    renderLab("codex", { instruction: "base", maxSample: 5 }, c);
    c.querySelector("#opt-b").value = "my variant";
    c.querySelector("#opt-n").value = "2";
    let posted = null;
    global.fetch = vi.fn((url, opts) => {
      posted = { url, body: JSON.parse(opts.body) };
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ sampleSize: 2, a: { avgScore: 80, scored: 2, checkPassRates: [] }, b: { avgScore: 60, scored: 2, checkPassRates: [] } }),
      });
    });

    c.querySelector("#opt-run").click();
    await new Promise((r) => setTimeout(r, 0));

    expect(posted.url).toBe("/api/optimize");
    expect(posted.body).toMatchObject({ flavor: "codex", sampleSize: 2, instructionA: "base", instructionB: "my variant" });
    expect(c.querySelector("#opt-results").innerHTML).toContain("Prompt A wins");
  });
});

describe("showOptimize", () => {
  it("fetches the default instruction and renders the lab", async () => {
    global.fetch = vi.fn(() =>
      Promise.resolve({ ok: true, json: () => Promise.resolve({ instruction: "Analyze the transcript.", maxSample: 5 }) })
    );
    await showOptimize("antigravity-cli");
    expect(global.fetch).toHaveBeenCalledWith("/api/optimize", expect.any(Object));
    expect(document.getElementById("transcript-container").innerHTML).toContain("Prompt Lab");
    expect(document.getElementById("opt-a").value).toBe("Analyze the transcript.");
  });
});
