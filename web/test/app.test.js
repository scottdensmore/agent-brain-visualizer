import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  filterAndSortConversations,
  loadConversations,
  loadMoreConversations,
  renderConversationsList,
  selectConversation,
} from "../public/app.js";

// The browse→select cascade chains several un-awaited promises (renderConversationsList ->
// click -> selectConversation -> transcript fetch). Flushing the microtask queue a number of times
// lets that chain settle. The detached progress-polling loop in triggerAnalysis uses a (faked)
// setTimeout that never fires; it stays pending on purpose and is harmless to the assertions.
const flush = async () => {
  for (let i = 0; i < 15; i++) await Promise.resolve();
};

describe("filterAndSortConversations", () => {
  const convs = [
    { id: "aaa111", summary: "Fix the parser" },
    { id: "bbb222", summary: "Add a feature" },
  ];

  it("filters by summary, case-insensitively", () => {
    const out = filterAndSortConversations(convs, "PARSER", true);
    expect(out).toHaveLength(1);
    expect(out[0].id).toBe("aaa111");
  });

  it("filters by id", () => {
    const out = filterAndSortConversations(convs, "bbb", true);
    expect(out).toHaveLength(1);
    expect(out[0].summary).toBe("Add a feature");
  });

  it("returns nothing when no conversation matches", () => {
    expect(filterAndSortConversations(convs, "nothing-here", true)).toHaveLength(0);
  });

  it("reverses order when not descending and preserves it when descending", () => {
    expect(filterAndSortConversations(convs, "", true).map((c) => c.id)).toEqual([
      "aaa111",
      "bbb222",
    ]);
    expect(filterAndSortConversations(convs, "", false).map((c) => c.id)).toEqual([
      "bbb222",
      "aaa111",
    ]);
  });
});

describe("session browsing and selection (integration)", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    window.location.hash = "";
    document.body.innerHTML = `
      <select id="flavor-select"><option value="antigravity-cli" selected>cli</option></select>
      <input id="conversation-search" value="" />
      <div id="conversations-list"></div>
      <div id="conv-popover" class="hidden">
        <span id="popover-title"></span><span id="popover-id"></span><span id="popover-time"></span>
      </div>
      <div id="current-session-title"></div>
      <div id="current-session-id"></div>
      <button id="summarize-btn" disabled></button>
      <div id="transcript-container"></div>
      <div id="session-stats-container" class="hidden"></div>
      <div id="ai-summary-container" class="hidden"></div>
      <div id="ai-summary-content" class="collapsed"></div>
      <div id="ai-summary-header"><span class="chevron"></span></div>
      <div id="ai-summary-text"></div>
    `;
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  function mockBackend() {
    global.fetch = vi.fn((url) => {
      if (url.includes("/transcript")) {
        return Promise.resolve({
          ok: true,
          json: () =>
            Promise.resolve([
              { type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
            ]),
        });
      }
      if (url.includes("/progress")) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ progress: 100, phase: "Done" }),
        });
      }
      if (url.includes("/summarize")) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ summary: "ok" }),
        });
      }
      // conversation list (paged: {items, total, limit, offset})
      return Promise.resolve({
        ok: true,
        json: () =>
          Promise.resolve({
            items: [
              { id: "aaa111", summary: "First <b>session", updatedAt: "2" },
              { id: "bbb222", summary: "Second session", updatedAt: "1" },
            ],
            total: 2,
            limit: 200,
            offset: 0,
          }),
      });
    });
  }

  it("loads the list, escapes summaries, and auto-selects the first session", async () => {
    mockBackend();

    await loadConversations();
    await flush();

    const items = document.querySelectorAll("#conversations-list .conv-item");
    expect(items).toHaveLength(2);
    // Summary is HTML-escaped.
    expect(items[0].querySelector(".conv-id").innerHTML).toContain("First &lt;b&gt;session");

    // First session auto-selected and its transcript rendered.
    expect(items[0].classList.contains("active")).toBe(true);
    expect(document.getElementById("current-session-id").dataset.id).toBe("aaa111");
    expect(document.getElementById("summarize-btn").disabled).toBe(false);
    expect(document.querySelector("#transcript-container .step-card")).not.toBeNull();
  });

  it("makes list items focusable buttons and selects one with the keyboard", async () => {
    mockBackend();

    await loadConversations();
    await flush();

    const items = document.querySelectorAll("#conversations-list .conv-item");
    // Announced and reachable by keyboard.
    expect(items[1].getAttribute("role")).toBe("button");
    expect(items[1].tabIndex).toBe(0);

    // Enter on the second (focused) item selects it, same as a click.
    items[1].dispatchEvent(
      new KeyboardEvent("keydown", { key: "Enter", bubbles: true })
    );
    await flush();

    expect(items[1].classList.contains("active")).toBe(true);
    expect(document.getElementById("current-session-id").dataset.id).toBe("bbb222");
  });

  it("offers 'Load more' when the store has more than one page, and appends the next page", async () => {
    // Page 0 returns 1 of 2 sessions; the footer must appear, and loading more appends the rest.
    const pages = {
      0: { items: [{ id: "aaa111", summary: "First", updatedAt: "2" }], total: 2, offset: 0 },
      1: { items: [{ id: "bbb222", summary: "Second", updatedAt: "1" }], total: 2, offset: 1 },
    };
    global.fetch = vi.fn((url) => {
      if (url.includes("/transcript")) {
        return Promise.resolve({ ok: true, json: () => Promise.resolve([]) });
      }
      const offset = Number(new URL(url, "http://x").searchParams.get("offset") || 0);
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve(offset === 0 ? pages[0] : pages[1]),
      });
    });

    await loadConversations();
    await flush();

    expect(document.querySelectorAll("#conversations-list .conv-item")).toHaveLength(1);
    const btn = document.getElementById("load-more-btn");
    expect(btn).not.toBeNull();
    expect(btn.textContent).toContain("1 of 2");

    await loadMoreConversations();
    await flush();

    expect(document.querySelectorAll("#conversations-list .conv-item")).toHaveLength(2);
    // Everything loaded → the footer is gone.
    expect(document.getElementById("load-more-btn")).toBeNull();
  });

  it("keeps 'Load more' and a search hint when nothing loaded matches but more pages exist", async () => {
    global.fetch = vi.fn((url) => {
      if (url.includes("/transcript")) {
        return Promise.resolve({ ok: true, json: () => Promise.resolve([]) });
      }
      return Promise.resolve({
        ok: true,
        json: () =>
          Promise.resolve({
            items: [{ id: "aaa111", summary: "First", updatedAt: "2" }],
            total: 5,
            offset: 0,
          }),
      });
    });

    await loadConversations();
    await flush();

    // A search that matches nothing in the one loaded session, while 4 more remain in the store.
    document.getElementById("conversation-search").value = "zzz-no-match";
    renderConversationsList();

    expect(document.querySelectorAll("#conversations-list .conv-item")).toHaveLength(0);
    expect(document.getElementById("conversations-list").textContent).toContain(
      "load more to keep searching"
    );
    // The footer is still offered so the user can pull more pages to widen the search.
    expect(document.getElementById("load-more-btn")).not.toBeNull();
  });

  it("shows first-run onboarding in the main pane when the source is empty", async () => {
    global.fetch = vi.fn(() =>
      Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ items: [], total: 0, limit: 200, offset: 0 }),
      })
    );

    await loadConversations();
    await flush();

    // Sidebar: no items, a friendly empty note naming the source (option text is "cli").
    expect(document.querySelectorAll("#conversations-list .conv-item")).toHaveLength(0);
    expect(document.getElementById("conversations-list").textContent).toContain(
      "No cli sessions yet — import with agent-ingest."
    );
    // Main pane: the import guide, scoped to the source, with a copyable command and a Copy button.
    const main = document.getElementById("transcript-container").textContent;
    expect(main).toContain("No cli sessions yet");
    expect(main).toContain("agent-ingest --server");
    expect(document.getElementById("onboarding-copy")).not.toBeNull();
    // Header no longer says "select a session" when there are none to select.
    expect(document.getElementById("current-session-title").textContent).toBe("Getting started");
  });

  it("replaces the onboarding with a transcript once the source has sessions on reload", async () => {
    let empty = true;
    global.fetch = vi.fn((url) => {
      if (url.includes("/transcript")) {
        return Promise.resolve({
          ok: true,
          json: () =>
            Promise.resolve([
              { type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
            ]),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () =>
          Promise.resolve(
            empty
              ? { items: [], total: 0 }
              : { items: [{ id: "aaa111", summary: "First", updatedAt: "2" }], total: 1 }
          ),
      });
    });

    await loadConversations();
    await flush();
    expect(document.getElementById("transcript-container").textContent).toContain("sessions yet");

    // The user imports sessions and refreshes.
    empty = false;
    await loadConversations();
    await flush();

    expect(document.getElementById("transcript-container").textContent).not.toContain("sessions yet");
    expect(document.querySelector("#transcript-container .step-card")).not.toBeNull();
  });

  it("shows a failure message when the transcript request errors", async () => {
    global.fetch = vi.fn((url) => {
      if (url.includes("/transcript")) {
        return Promise.reject(new Error("boom"));
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ summary: "ok" }) });
    });

    const element = document.createElement("div");
    element.className = "conv-item";
    element.innerHTML = '<div class="conv-id">Some session</div>';
    document.getElementById("conversations-list").appendChild(element);

    await selectConversation("zzz999", element);
    await flush();

    expect(document.getElementById("transcript-container").innerHTML).toContain(
      "Failed to load transcript"
    );
  });
});
