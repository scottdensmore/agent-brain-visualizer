import { beforeEach, describe, expect, it } from "vitest";
import { renderStats } from "../public/modules/stats.js";

beforeEach(() => {
  document.body.innerHTML = '<div id="session-stats-container" class="hidden"></div>';
});

function statValue(id) {
  return document
    .querySelector(`#${id} .stat-value`)
    .textContent.trim();
}

describe("renderStats", () => {
  it("hides the container when there are no steps", () => {
    renderStats([]);
    const container = document.getElementById("session-stats-container");
    expect(container.classList.contains("hidden")).toBe(true);
  });

  it("counts user queries, tool calls, model responses and errors", () => {
    const steps = [
      { type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
      {
        source: "MODEL",
        type: "PLANNER_RESPONSE",
        content: "here is the answer",
        created_at: "2026-06-19T10:00:05Z",
      },
      {
        source: "MODEL",
        type: "PLANNER_RESPONSE",
        tool_calls: [{ name: "edit" }],
        created_at: "2026-06-19T10:00:10Z",
      },
      {
        type: "ERROR_MESSAGE",
        status: "ERROR",
        content: "boom",
        created_at: "2026-06-19T10:00:20Z",
      },
    ];

    renderStats(steps);

    expect(statValue("user-queries-stat-card")).toBe("1");
    expect(statValue("tools-stat-card")).toBe("1");
    expect(statValue("model-responses-stat-card")).toBe("1");
    expect(document.getElementById("session-stats-container").classList.contains("hidden")).toBe(
      false
    );
    // OUTCOME card reflects the error.
    expect(statValue("errors-stat-card")).toBe("Issues Detected");
  });

  it("reports success when there are no errors", () => {
    const steps = [
      { type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
    ];
    renderStats(steps);
    expect(statValue("errors-stat-card")).toBe("Succeeded");
  });

  it("counts tool steps that use a tool-like type without a tool_calls array", () => {
    const steps = [
      { type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
      { type: "VIEW_FILE", content: "...", created_at: "2026-06-19T10:00:01Z" },
      { type: "RUN_COMMAND", content: "...", created_at: "2026-06-19T10:00:02Z" },
    ];
    renderStats(steps);
    expect(statValue("tools-stat-card")).toBe("2");
  });

  it("treats a failed RUN_COMMAND as an error", () => {
    const steps = [
      { type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
      {
        type: "RUN_COMMAND",
        content: "The command failed with exit code: 1",
        created_at: "2026-06-19T10:00:01Z",
      },
    ];
    renderStats(steps);
    expect(statValue("errors-stat-card")).toBe("Issues Detected");
  });

  it("groups steps into sequences, starting a new one at each user query", () => {
    const steps = [
      { type: "USER_INPUT", content: "q1", created_at: "2026-06-19T10:00:00Z" },
      { source: "MODEL", type: "PLANNER_RESPONSE", content: "a1", created_at: "2026-06-19T10:00:05Z" },
      { type: "USER_INPUT", content: "q2", created_at: "2026-06-19T10:01:00Z" },
    ];
    renderStats(steps);
    // The DURATION card sub-label reports the number of active segments.
    const sub = document.querySelector("#duration-stat-card .stat-sub").textContent;
    expect(sub).toContain("2 active segments");
  });

  it("renders the tool distribution chart with tool names and counts", () => {
    const steps = [
      { type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
      { source: "MODEL", type: "RUN_TOOL", tool_calls: [{ name: "edit" }], created_at: "2026-06-19T10:00:01Z" },
      { source: "MODEL", type: "RUN_TOOL", tool_calls: [{ name: "edit" }], created_at: "2026-06-19T10:00:02Z" },
      { source: "MODEL", type: "RUN_TOOL", tool_calls: [{ name: "run" }], created_at: "2026-06-19T10:00:03Z" },
    ];
    renderStats(steps);
    const chart = document.getElementById("tools-chart");
    // Assert on the row label (title attribute) rather than a bare substring to avoid passing on
    // incidental markup.
    expect(chart.innerHTML).toContain('title="edit"');
    expect(chart.innerHTML).toContain('title="run"');
  });

  it("renders the issues breakdown chart with the error message", () => {
    const steps = [
      { type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
      {
        type: "ERROR_MESSAGE",
        status: "ERROR",
        error: "Something failed badly",
        created_at: "2026-06-19T10:00:01Z",
      },
    ];
    renderStats(steps);
    const chart = document.getElementById("errors-chart");
    expect(chart.innerHTML).toContain("Something failed badly");
  });

  it("makes interactive stat cards keyboard-operable buttons", () => {
    const steps = [
      { type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
      { source: "MODEL", type: "RUN_TOOL", tool_calls: [{ name: "edit" }], created_at: "2026-06-19T10:00:01Z" },
    ];
    renderStats(steps);

    const toolsCard = document.getElementById("tools-stat-card");
    expect(toolsCard.getAttribute("role")).toBe("button");
    expect(toolsCard.getAttribute("tabindex")).toBe("0");
    // No errors -> the OUTCOME card is inert and must not claim to be a button.
    expect(document.getElementById("errors-stat-card").getAttribute("role")).toBeNull();

    // Enter toggles the tools chart open, just like a click.
    toolsCard.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    expect(document.getElementById("tools-chart").style.display).toBe("block");
  });

  it("marks the OUTCOME card as a button when there are errors to expand", () => {
    const steps = [
      { type: "USER_INPUT", content: "hi", created_at: "2026-06-19T10:00:00Z" },
      { type: "ERROR_MESSAGE", status: "ERROR", content: "boom", created_at: "2026-06-19T10:00:01Z" },
    ];
    renderStats(steps);
    const card = document.getElementById("errors-stat-card");
    expect(card.getAttribute("role")).toBe("button");
    card.dispatchEvent(new KeyboardEvent("keydown", { key: " ", bubbles: true }));
    expect(document.getElementById("errors-chart").style.display).toBe("block");
  });

  it("renders the session timeline chart with a position indicator", () => {
    const steps = [
      { type: "USER_INPUT", content: "q1", created_at: "2026-06-19T10:00:00Z" },
      { source: "MODEL", type: "PLANNER_RESPONSE", content: "a", created_at: "2026-06-19T10:00:30Z" },
    ];
    renderStats(steps);
    const chart = document.getElementById("duration-chart");
    expect(chart.innerHTML).toContain("Session Timeline");
    expect(document.getElementById("session-timeline-indicator")).not.toBeNull();
  });
});
