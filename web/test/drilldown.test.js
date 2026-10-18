import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { drillRow, wireDrilldown } from "../public/modules/drilldown.js";

beforeEach(() => {
  document.body.innerHTML = '<div id="c"></div>';
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("drillRow", () => {
  it("wraps content with the category, key, bar, and sessions container", () => {
    const c = document.getElementById("c");
    c.innerHTML = drillRow("workflow", "Read → Edit", "<span>hi</span>");
    const row = c.querySelector(".drill-row");
    expect(row.dataset.drillCategory).toBe("workflow");
    expect(row.dataset.drillKey).toBe("Read → Edit");
    expect(row.querySelector(".drill-bar").innerHTML).toContain("hi");
    expect(row.querySelector(".drill-sessions")).not.toBeNull();
  });

  it("escapes the key so it can't break out of the attribute", () => {
    const c = document.getElementById("c");
    c.innerHTML = drillRow("issue", '"><img src=x onerror=alert(1)>', "x");
    expect(c.querySelector("img")).toBeNull();
    // The value round-trips intact via the dataset.
    expect(c.querySelector(".drill-row").dataset.drillKey).toBe(
      '"><img src=x onerror=alert(1)>'
    );
  });
});

describe("wireDrilldown", () => {
  it("expands a row into its sessions and opens one on click", async () => {
    const c = document.getElementById("c");
    const conv = document.createElement("div");
    conv.className = "conv-item";
    conv.dataset.id = "sess-1";
    const convClick = vi.fn();
    conv.addEventListener("click", convClick);
    document.body.appendChild(conv);

    c.innerHTML = drillRow("workflow", "Read → Edit → Bash", "<span>seq</span>");
    wireDrilldown(c, "codex");
    global.fetch = vi.fn(() =>
      Promise.resolve({
        ok: true,
        json: () =>
          Promise.resolve({
            category: "workflow",
            key: "Read → Edit → Bash",
            totalMatches: 1,
            sessions: [{ id: "sess-1", title: "Refactor" }],
          }),
      })
    );

    c.querySelector(".drill-bar").click();
    await new Promise((r) => setTimeout(r, 0));

    expect(global.fetch).toHaveBeenCalledWith(expect.stringContaining("category=workflow"), expect.any(Object));
    const sub = c.querySelector(".drill-sessions");
    expect(sub.innerHTML).toContain("Refactor");
    sub.querySelector(".drill-session").click();
    expect(convClick).toHaveBeenCalled();
  });

  it("expands a row with the keyboard (Enter on the focusable bar)", async () => {
    const c = document.getElementById("c");
    c.innerHTML = drillRow("tool", "Bash", "<span>bar</span>");
    wireDrilldown(c, "codex");
    global.fetch = vi.fn(() =>
      Promise.resolve({ ok: true, json: () => Promise.resolve({ sessions: [] }) })
    );

    const bar = c.querySelector(".drill-bar");
    expect(bar.getAttribute("role")).toBe("button");
    expect(bar.getAttribute("tabindex")).toBe("0");

    bar.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await new Promise((r) => setTimeout(r, 0));
    expect(global.fetch).toHaveBeenCalledTimes(1);
    expect(c.querySelector(".drill-sessions").innerHTML).toContain("No sessions");
  });

  it("toggles visibility without refetching on a second click", async () => {
    const c = document.getElementById("c");
    c.innerHTML = drillRow("tool", "Bash", "<span>bar</span>");
    wireDrilldown(c, "codex");
    global.fetch = vi.fn(() =>
      Promise.resolve({ ok: true, json: () => Promise.resolve({ sessions: [] }) })
    );
    const bar = c.querySelector(".drill-bar");

    bar.click();
    await new Promise((r) => setTimeout(r, 0));
    bar.click(); // collapse
    bar.click(); // expand again
    await new Promise((r) => setTimeout(r, 0));
    expect(global.fetch).toHaveBeenCalledTimes(1);
  });

  it("shows an inline error when the request fails", async () => {
    const c = document.getElementById("c");
    c.innerHTML = drillRow("error", "boom", "<span>e</span>");
    wireDrilldown(c, "codex");
    global.fetch = vi.fn(() => Promise.resolve({ ok: false, status: 500 }));

    c.querySelector(".drill-bar").click();
    await new Promise((r) => setTimeout(r, 0));
    expect(c.querySelector(".drill-sessions").innerHTML).toContain(
      "Failed to load sessions"
    );
  });
});
