import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderTranscript } from "../../main/resources/public/modules/timeline.js";

let container;

beforeEach(() => {
  document.body.innerHTML = '<div id="transcript-container"></div>';
  container = document.getElementById("transcript-container");
});

// Step bodies render lazily on first expansion; tests that assert body content must open the card
// the way a user would.
function expandCard(index = 0) {
  container.querySelectorAll(".step-card .step-header")[index].click();
}

describe("renderTranscript", () => {
  it("shows an empty state when there are no steps", () => {
    renderTranscript([], container);
    expect(container.querySelector(".empty-state")).not.toBeNull();
  });

  it("classifies cards by role via data attributes", () => {
    const steps = [
      { source: "USER_EXPLICIT", type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
      { source: "MODEL", type: "VIEW_FILE", content: "file body", created_at: "2026-06-19T10:00:01Z" },
      { source: "MODEL", type: "PLANNER_RESPONSE", content: "the answer", created_at: "2026-06-19T10:00:02Z" },
      { type: "ERROR_MESSAGE", status: "ERROR", content: "boom", created_at: "2026-06-19T10:00:03Z" },
    ];

    renderTranscript(steps, container);
    const cards = container.querySelectorAll(".step-card");
    expect(cards).toHaveLength(4);

    expect(cards[0].dataset.isUser).toBe("true");
    expect(cards[1].dataset.isTool).toBe("true");
    expect(cards[2].dataset.isModel).toBe("true");
    expect(cards[3].dataset.isError).toBe("true");
  });

  it("starts a new sequence wrapper at each user step", () => {
    const steps = [
      { source: "USER_EXPLICIT", type: "USER_INPUT", content: "q1", created_at: "2026-06-19T10:00:00Z" },
      { source: "MODEL", type: "VIEW_FILE", content: "x", created_at: "2026-06-19T10:00:01Z" },
      { source: "USER_EXPLICIT", type: "USER_INPUT", content: "q2", created_at: "2026-06-19T10:01:00Z" },
      { source: "MODEL", type: "PLANNER_RESPONSE", content: "done", created_at: "2026-06-19T10:01:05Z" },
    ];

    renderTranscript(steps, container);
    expect(container.querySelectorAll(".sequence-wrapper")).toHaveLength(2);
  });

  it("renders tool calls with their name and arguments", () => {
    const steps = [
      {
        source: "MODEL",
        type: "RUN_TOOL",
        tool_calls: [{ name: "edit_file", args: { path: "/a.txt" } }],
        created_at: "2026-06-19T10:00:00Z",
      },
    ];
    renderTranscript(steps, container);
    expandCard();
    const toolCall = container.querySelector(".tool-call");
    expect(toolCall).not.toBeNull();
    expect(toolCall.querySelector(".tool-name").textContent).toContain("edit_file");
    expect(toolCall.textContent).toContain("/a.txt");
  });

  it("renders an explicit error box for steps carrying an error", () => {
    const steps = [
      {
        type: "ERROR_MESSAGE",
        status: "ERROR",
        error: "Boom: stack trace here",
        created_at: "2026-06-19T10:00:00Z",
      },
    ];
    renderTranscript(steps, container);
    expandCard();
    const errorBox = container.querySelector(".code-block");
    expect(errorBox).not.toBeNull();
    expect(errorBox.innerHTML).toContain("ERROR:");
    expect(errorBox.textContent).toContain("Boom: stack trace here");
  });

  it("splits a tagged user request into request and system-context blocks", () => {
    const steps = [
      {
        type: "USER_INPUT",
        source: "USER_EXPLICIT",
        content:
          "<USER_REQUEST>\nPlease do X\n</USER_REQUEST><CURRENT_FILE>secret.txt</CURRENT_FILE>",
        created_at: "2026-06-19T10:00:00Z",
      },
    ];
    renderTranscript(steps, container);
    expandCard();
    const requestBlock = container.querySelector(".user-request-block");
    const contextBlock = container.querySelector(".system-context-block");
    expect(requestBlock.textContent).toContain("Please do X");
    expect(contextBlock.textContent).toContain("secret.txt");
  });

  // Transcript text is whatever the agent read, and the content scanners are quadratic on input
  // that opens a construct and never closes it. Without a bound, one such step pins the main
  // thread for minutes — and since the newest session is auto-selected, on every page load.
  it("renders a pathological step body within a time bound instead of scanning it", () => {
    // Unclosed `@[` mention openings, which the mention scanner rescans to end-of-string from once
    // per opening. ~64 KB costs about 4s uncapped and grows with the square of the content. This
    // payload renders as plain text, so what the bound below measures is the scan, not the DOM
    // building. The same cap bounds the tag, metadata and citation scanners.
    const hostile = "@[".repeat(32_000); // ~64 KB
    const steps = [
      {
        type: "USER_INPUT",
        content: `<ADDITIONAL_METADATA>\n${hostile}\n</ADDITIONAL_METADATA>`,
        created_at: "2026-06-19T10:00:00Z",
      },
    ];

    const started = performance.now();
    renderTranscript(steps, container);
    expandCard();
    const elapsed = performance.now() - started;

    expect(elapsed).toBeLessThan(2000);
    // The content is still shown in full — only the enrichment is skipped, and it says so.
    expect(container.querySelector(".oversized-step-note")).not.toBeNull();
    expect(container.querySelector(".markdown-body").textContent).toContain("@[");
  });

  it("still enriches a step body that sits under the scan limit", () => {
    const steps = [
      {
        type: "USER_INPUT",
        source: "USER_EXPLICIT",
        content:
          "<USER_REQUEST>\nPlease do X\n</USER_REQUEST>\n" +
          "<ADDITIONAL_METADATA>\n@[notes.txt] is a [File]:\n/tmp/notes.txt\n</ADDITIONAL_METADATA>",
        created_at: "2026-06-19T10:00:00Z",
      },
    ];

    renderTranscript(steps, container);
    expandCard();

    expect(container.querySelector(".oversized-step-note")).toBeNull();
    expect(container.querySelector(".user-request-block")).not.toBeNull();
    expect(container.innerHTML).toContain("file:///tmp/notes.txt");
  });

  it("appends a bottom timeline marker and fires the transcriptLoaded event", () => {
    let fired = false;
    const handler = () => {
      fired = true;
    };
    window.addEventListener("transcriptLoaded", handler);

    renderTranscript(
      [{ type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" }],
      container
    );

    window.removeEventListener("transcriptLoaded", handler);
    expect(container.querySelector("#timeline-bottom-marker")).not.toBeNull();
    expect(fired).toBe(true);
  });
});

describe("scrollToTime", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("highlights the step card nearest the target time", () => {
    vi.useFakeTimers();
    const steps = [
      { type: "USER_INPUT", content: "q1", created_at: "2026-06-19T10:00:00Z" },
      { type: "USER_INPUT", content: "q2", created_at: "2026-06-19T10:05:00Z" },
    ];
    renderTranscript(steps, container);

    const target = new Date("2026-06-19T10:05:00Z").getTime();
    window.scrollToTime(target);

    const cards = container.querySelectorAll(".step-card");
    // The second card matches the target timestamp and receives the highlight box-shadow.
    expect(cards[1].style.boxShadow).toContain("accent-blue");
    expect(cards[0].style.boxShadow).toBe("");
  });
});
