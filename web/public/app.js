/*
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
import { state, escapeHtml, apiFetch } from "./modules/utils.js";
import { renderTranscript } from "./modules/timeline.js";
import { renderStats } from "./modules/stats.js";
import { triggerAnalysis } from "./modules/analysis.js";
import { initUI } from "./modules/ui.js";
import { showInsights } from "./modules/insights.js";
import { showMining } from "./modules/mine.js";
import { showEval } from "./modules/eval.js";
import { showOptimize } from "./modules/optimize.js";

let allConversations = [];
let sortDescending = true;

// The sidebar loads the newest sessions a page at a time; a large store is never fetched (or
// rendered) all at once. Search and sort work over what's loaded — "Load more" pulls the next page.
const CONV_PAGE_SIZE = 200;
let convTotal = 0;

document.addEventListener("DOMContentLoaded", () => {
  initUI();

  const flavorSelect = document.getElementById("flavor-select");
  const savedFlavor = localStorage.getItem("agy-flavor");
  if (savedFlavor) {
    flavorSelect.value = savedFlavor;
  }

  const sidebarToggleBtn = document.getElementById("sidebar-toggle-btn");
  if (sidebarToggleBtn) {
    sidebarToggleBtn.addEventListener("click", () => {
      const sidebar = document.querySelector(".sidebar");
      sidebar.classList.toggle("collapsed");

      const expandedIcon = sidebarToggleBtn.querySelector(
        ".sidebar-icon-expanded"
      );
      const collapsedIcon = sidebarToggleBtn.querySelector(
        ".sidebar-icon-collapsed"
      );

      if (sidebar.classList.contains("collapsed")) {
        expandedIcon.classList.add("hidden");
        collapsedIcon.classList.remove("hidden");
        sidebarToggleBtn.querySelector(".sidebar-chevron").style.transform =
          "rotate(180deg)";
      } else {
        expandedIcon.classList.remove("hidden");
        collapsedIcon.classList.add("hidden");
        sidebarToggleBtn.querySelector(".sidebar-chevron").style.transform =
          "rotate(0deg)";
      }
    });
  }

  const resizer = document.getElementById("sidebar-resizer");
  const sidebar = document.querySelector(".sidebar");
  if (resizer && sidebar) {
    let isResizing = false;

    resizer.addEventListener("mousedown", (e) => {
      isResizing = true;
      document.body.style.cursor = "col-resize";
      sidebar.style.transition = "none";
      resizer.classList.add("resizing");
      // Prevent text selection while dragging
      document.body.style.userSelect = "none";
    });

    document.addEventListener("mousemove", (e) => {
      if (!isResizing) return;
      let newWidth = e.clientX;
      if (newWidth < 200) newWidth = 200;
      if (newWidth > 800) newWidth = 800;
      document.documentElement.style.setProperty(
        "--sidebar-width",
        `${newWidth}px`
      );
    });

    document.addEventListener("mouseup", () => {
      if (isResizing) {
        isResizing = false;
        document.body.style.cursor = "default";
        sidebar.style.transition =
          "margin-left 0.3s cubic-bezier(0.4, 0, 0.2, 1)";
        resizer.classList.remove("resizing");
        document.body.style.userSelect = "";
      }
    });
  }

  loadConversations();

  flavorSelect.addEventListener("change", () => {
    localStorage.setItem("agy-flavor", flavorSelect.value);
    loadConversations();
    document.getElementById("transcript-container").innerHTML =
      '<div class="empty-state">Select a session from the sidebar to view its transcript.</div>';
    const statsContainer = document.getElementById("session-stats-container");
    if (statsContainer) statsContainer.innerHTML = "";
    document.getElementById("current-session-title").innerText =
      "Select a session";
    document.getElementById("current-session-id").innerText = "";
    document.getElementById("summarize-btn").disabled = true;
    document.getElementById("ai-summary-container").classList.add("hidden");
  });

  const summaryHeader = document.getElementById("ai-summary-header");
  if (summaryHeader) {
    summaryHeader.addEventListener("click", () => {
      const content = document.getElementById("ai-summary-content");
      const chevron = summaryHeader.querySelector(".chevron");
      content.classList.toggle("collapsed");
      if (content.classList.contains("collapsed")) {
        chevron.style.transform = "rotate(0deg)";
      } else {
        chevron.style.transform = "rotate(90deg)";
      }
    });
  }

  document
    .getElementById("summarize-btn")
    .addEventListener("click", async (e) => {
      e.stopPropagation();
      const sessionId =
        document.getElementById("current-session-id").dataset.id;
      if (!sessionId) return;
      triggerAnalysis(sessionId, true);
    });

  const insightsBtn = document.getElementById("insights-btn");
  if (insightsBtn) {
    insightsBtn.addEventListener("click", () => {
      showInsights(document.getElementById("flavor-select").value);
    });
  }

  const mineBtn = document.getElementById("mine-btn");
  if (mineBtn) {
    mineBtn.addEventListener("click", () => {
      showMining(document.getElementById("flavor-select").value);
    });
  }

  const evalBtn = document.getElementById("eval-btn");
  if (evalBtn) {
    evalBtn.addEventListener("click", () => {
      showEval(document.getElementById("flavor-select").value);
    });
  }

  const optimizeBtn = document.getElementById("optimize-btn");
  if (optimizeBtn) {
    optimizeBtn.addEventListener("click", () => {
      showOptimize(document.getElementById("flavor-select").value);
    });
  }

  const refreshBtn = document.getElementById("refresh-conversations-btn");
  if (refreshBtn) {
    let refreshRot = 0;
    refreshBtn.addEventListener("click", () => {
      refreshRot += 180;
      refreshBtn.querySelector(
        "svg"
      ).style.transform = `rotate(${refreshRot}deg)`;
      loadConversations();
    });
  }

  const sortBtn = document.getElementById("sort-conversations-btn");
  if (sortBtn) {
    let sortRot = 0;
    sortBtn.addEventListener("click", () => {
      sortRot += 180;
      sortBtn.querySelector("svg").style.transform = `rotate(${sortRot}deg)`;
      sortDescending = !sortDescending;
      renderConversationsList();
    });
  }

  const searchInput = document.getElementById("conversation-search");
  const clearSearchBtn = document.getElementById("clear-search-btn");
  if (searchInput) {
    searchInput.addEventListener("input", () => {
      if (clearSearchBtn) {
        clearSearchBtn.style.display =
          searchInput.value.length > 0 ? "block" : "none";
      }
      renderConversationsList();
    });
  }

  if (clearSearchBtn) {
    clearSearchBtn.addEventListener("click", () => {
      if (searchInput) {
        searchInput.value = "";
        clearSearchBtn.style.display = "none";
        renderConversationsList();
        searchInput.focus();
      }
    });
  }
});

export function filterAndSortConversations(
  conversations,
  searchTerm,
  descending
) {
  let filtered = [...conversations];
  const term = (searchTerm || "").toLowerCase();
  if (term) {
    filtered = filtered.filter(
      (c) =>
        c.summary.toLowerCase().includes(term) ||
        c.id.toLowerCase().includes(term)
    );
  }
  if (!descending) {
    filtered.reverse();
  }
  return filtered;
}

export async function loadConversations() {
  const list = document.getElementById("conversations-list");
  const flavor = encodeURIComponent(
    document.getElementById("flavor-select").value
  );
  try {
    const res = await apiFetch(
      `/api/brain/conversations?flavor=${flavor}&limit=${CONV_PAGE_SIZE}&offset=0`
    );
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const page = await res.json();
    allConversations = page.items || [];
    convTotal = page.total ?? allConversations.length;
    announceConversations();
    // Only the first load auto-selects (deep-link or newest session); re-renders must not.
    renderConversationsList(true);
    // A reachable-but-empty store is the first-run case: guide the user to import instead of
    // leaving the main pane on the "select a session" prompt when there are none to select.
    if (convTotal === 0) showEmptyStoreOnboarding();
  } catch (e) {
    list.innerHTML =
      '<div class="loading-state" style="padding:16px;">Error loading sessions. Is the backend running?</div>';
  }
}

// The human-readable label of the selected source, e.g. "OpenAI Codex".
function sourceLabel() {
  const sel = document.getElementById("flavor-select");
  return sel?.selectedOptions?.[0]?.text?.trim() || "agent";
}

// Writes a message to the sidebar's polite live region (screen-reader announcement).
function announce(message) {
  const status = document.getElementById("conv-status");
  if (status) status.textContent = message;
}

// Guidance shown in the main pane when the selected source has no sessions yet. Scoped to the source
// (the picker has no "all" option), so it reads correctly whether this is a true first run or just a
// source the user hasn't imported.
function showEmptyStoreOnboarding() {
  const container = document.getElementById("transcript-container");
  if (!container) return;
  const source = escapeHtml(sourceLabel());
  const cmd = `agent-ingest --server ${window.location.origin}`;
  container.innerHTML = `
    <div class="empty-state">
      <div class="empty-icon" aria-hidden="true">📭</div>
      <h2>No ${source} sessions yet</h2>
      <p>Run the <strong>agent-ingest</strong> CLI on a machine that has ${source} sessions — it
        scans for Antigravity, OpenAI Codex, and Claude Code transcripts and pushes them here.</p>
      <div class="onboarding-cmd-row">
        <pre class="onboarding-cmd">${escapeHtml(cmd)}</pre>
        <button type="button" class="onboarding-copy" id="onboarding-copy">Copy</button>
      </div>
      <p>Run it again any time (or on a schedule) — it only uploads what changed.</p>
    </div>`;

  const copyBtn = document.getElementById("onboarding-copy");
  copyBtn?.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(cmd);
      copyBtn.textContent = "Copied";
      announce("Command copied to clipboard.");
    } catch {
      announce("Copy failed — select the command and copy it manually.");
    }
  });

  // The header prompts "Select a session", which makes no sense with none to select.
  const title = document.getElementById("current-session-title");
  if (title) title.textContent = "Getting started";
}

// Appends the next page to what's already loaded. The offset is the count we hold; on a stable list
// pages don't overlap, but if sessions are ingested while browsing, offset paging can repeat or skip
// a row at the boundary (an accepted trade-off for a sidebar — re-open the flavor to refresh).
export async function loadMoreConversations() {
  const flavor = encodeURIComponent(
    document.getElementById("flavor-select").value
  );
  const btn = document.getElementById("load-more-btn");
  if (btn) {
    btn.disabled = true;
    btn.textContent = "Loading…";
  }
  try {
    const res = await apiFetch(
      `/api/brain/conversations?flavor=${flavor}&limit=${CONV_PAGE_SIZE}&offset=${allConversations.length}`
    );
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const page = await res.json();
    allConversations = allConversations.concat(page.items || []);
    convTotal = page.total ?? convTotal;
    announceConversations();
    renderConversationsList();
    // Keep the keyboard user on the control they just used (the button is rebuilt each render).
    document.getElementById("load-more-btn")?.focus();
  } catch (e) {
    if (btn) {
      btn.disabled = false;
      btn.textContent = "Load more";
    }
  }
}

// Announces the loaded/total count on a polite live region, so a screen-reader user hears that more
// sessions arrived. Kept separate from the list so re-rendering rows doesn't spam the reader.
function announceConversations() {
  const status = document.getElementById("conv-status");
  if (status) {
    status.textContent =
      convTotal > allConversations.length
        ? `Loaded ${allConversations.length} of ${convTotal} sessions.`
        : `Loaded all ${allConversations.length} sessions.`;
  }
}

export function renderConversationsList(autoSelect = false) {
  const list = document.getElementById("conversations-list");
  // A re-render (search, sort, "Load more") rebuilds the list; keep the user's scroll position rather
  // than jumping them back to the top.
  const prevScroll = list.scrollTop;
  list.innerHTML = "";

  const searchTerm =
    document.getElementById("conversation-search")?.value || "";
  const filtered = filterAndSortConversations(
    allConversations,
    searchTerm,
    sortDescending
  );

  if (filtered.length === 0) {
    const empty = document.createElement("div");
    empty.className = "loading-state";
    // Three empty cases: a genuinely empty source (first run — wins even mid-search, since there's
    // nothing to search); an active search with more pages to pull; and a search that matched
    // nothing in a non-empty source.
    if (convTotal === 0) {
      empty.textContent = `No ${sourceLabel()} sessions yet — import with agent-ingest.`;
    } else if (searchTerm && allConversations.length < convTotal) {
      empty.textContent =
        "No matches in the loaded sessions — load more to keep searching.";
    } else {
      empty.textContent = "No sessions found";
    }
    list.appendChild(empty);
  } else {
    filtered.forEach((conv) => {
      const div = document.createElement("div");
      div.className = "conv-item";
      // The rows are divs, so tell the keyboard (and screen readers) they act like buttons; the
      // matching Enter/Space handling is delegated on the list below.
      div.setAttribute("role", "button");
      div.tabIndex = 0;
      div.dataset.id = conv.id;
      div.dataset.summary = conv.summary;
      div.dataset.updatedAt = conv.updatedAt || "0";
      div.innerHTML = `<div class="conv-id">${escapeHtml(conv.summary)}</div>`;
      list.appendChild(div);
    });
  }

  appendLoadMore(list);

  // Event Delegation for conversation selection
  if (!list.dataset.listenerAttached) {
    list.addEventListener("click", (e) => {
      const item = e.target.closest(".conv-item");
      if (item && item.dataset.id) {
        selectConversation(item.dataset.id, item);
      }
    });

    // Keyboard parity with the click delegation: Enter/Space on a focused item selects it.
    list.addEventListener("keydown", (e) => {
      if (e.key !== "Enter" && e.key !== " ") return;
      const item = e.target.closest(".conv-item");
      if (item && item.dataset.id) {
        e.preventDefault(); // Space must not scroll the sidebar
        selectConversation(item.dataset.id, item);
      }
    });

    // Custom Popover Logic
    const popover = document.getElementById("conv-popover");
    list.addEventListener("mouseover", (e) => {
      const item = e.target.closest(".conv-item");
      if (item && item.dataset.id && popover) {
        document.getElementById("popover-title").innerText =
          item.dataset.summary;
        document.getElementById("popover-id").innerText = item.dataset.id;

        const timestamp = parseInt(item.dataset.updatedAt, 10);
        let timeStr = "Unknown time";
        if (timestamp > 0) {
          const d = new Date(timestamp);
          const dateText = d.toLocaleDateString("en-US", {
            year: "numeric",
            month: "long",
            day: "numeric",
          });

          const diffMs = Date.now() - timestamp;
          const diffMin = Math.floor(diffMs / (1000 * 60));
          const diffHour = Math.floor(diffMs / (1000 * 60 * 60));
          const diffDay = Math.floor(diffMs / (1000 * 60 * 60 * 24));

          let relStr;
          if (diffDay > 0)
            relStr = diffDay === 1 ? "1 day ago" : `${diffDay} days ago`;
          else if (diffHour > 0)
            relStr = diffHour === 1 ? "1 hour ago" : `${diffHour} hours ago`;
          else if (diffMin > 0)
            relStr = diffMin === 1 ? "1 minute ago" : `${diffMin} minutes ago`;
          else relStr = "just now";

          timeStr = `${dateText} (${relStr})`;
        }
        document.getElementById("popover-time").innerHTML = timeStr;

        popover.classList.remove("hidden");
      }
    });

    list.addEventListener("mousemove", (e) => {
      const item = e.target.closest(".conv-item");
      if (item && popover && !popover.classList.contains("hidden")) {
        let top = e.clientY + 15;
        let left = e.clientX + 15;

        // Prevent overflow off bottom/right edges
        if (top + popover.offsetHeight > window.innerHeight) {
          top = e.clientY - popover.offsetHeight - 15;
        }
        if (left + popover.offsetWidth > window.innerWidth) {
          left = e.clientX - popover.offsetWidth - 15;
        }

        popover.style.top = `${top}px`;
        popover.style.left = `${left}px`;
      }
    });

    list.addEventListener("mouseout", (e) => {
      const item = e.target.closest(".conv-item");
      if (item && popover) {
        if (e.relatedTarget && item.contains(e.relatedTarget)) return;
        popover.classList.add("hidden");
      }
    });

    list.dataset.listenerAttached = "true";
  }

  const hashId = window.location.hash.substring(1);

  if (autoSelect) {
    // Initial load only: open the deep-linked session, or the newest one.
    const targetDiv = hashId
      ? list.querySelector(`[data-id="${hashId}"]`)
      : list.firstElementChild;
    if (targetDiv) {
      targetDiv.click();
    }
  } else if (hashId) {
    // A re-render must not re-open (and reload) the current session — just restore its highlight.
    list
      .querySelector(`.conv-item[data-id="${hashId}"]`)
      ?.classList.add("active");
  }

  list.scrollTop = prevScroll;
}

// Appends a "Load more" control when the store holds more sessions than are loaded. Rebuilt on every
// render (the list is cleared each time), so its count stays in step with what's loaded.
function appendLoadMore(list) {
  if (allConversations.length >= convTotal) return;
  const footer = document.createElement("div");
  footer.className = "conv-loadmore";
  const btn = document.createElement("button");
  btn.id = "load-more-btn";
  btn.className = "load-more-btn";
  // "loaded" makes clear the count is how many are held locally, not how many matched a search.
  btn.textContent = `Load more (${allConversations.length} of ${convTotal} loaded)`;
  btn.addEventListener("click", () => loadMoreConversations());
  footer.appendChild(btn);
  list.appendChild(footer);
}

// Monotonic generation counter guarding transcript loads: clicking session A then B quickly must
// never let A's slower response render into B's view. Same pattern as eval.js's renderGeneration.
let transcriptGeneration = 0;

export async function selectConversation(id, element) {
  const generation = ++transcriptGeneration;
  document
    .querySelectorAll(".conv-item")
    .forEach((el) => el.classList.remove("active"));
  element.classList.add("active");

  // Update URL hash
  window.location.hash = id;

  const title = document.getElementById("current-session-title");
  title.innerText = element.querySelector(".conv-id").innerText;

  const subtitle = document.getElementById("current-session-id");
  subtitle.innerText = id;
  subtitle.dataset.id = id;

  document.getElementById("summarize-btn").disabled = false;

  // Auto trigger analysis
  triggerAnalysis(id, false);

  const container = document.getElementById("transcript-container");
  container.innerHTML =
    '<div class="loading-state" style="text-align:center; padding: 40px; color:#94a3b8;">Loading transcript...</div>';

  try {
    const flavor = encodeURIComponent(
      document.getElementById("flavor-select").value
    );
    const response = await apiFetch(
      `/api/brain/conversations/${id}/transcript?flavor=${flavor}`
    );
    const steps = await response.json();

    // A newer selection superseded this one while the transcript was in flight.
    if (generation !== transcriptGeneration) return;

    state.spansMultipleDays = false;
    if (
      steps &&
      steps.length > 0 &&
      steps[0].created_at &&
      steps[steps.length - 1].created_at
    ) {
      const firstDateStr = new Date(steps[0].created_at).toLocaleDateString();
      const lastDateStr = new Date(
        steps[steps.length - 1].created_at
      ).toLocaleDateString();
      if (firstDateStr !== lastDateStr) {
        state.spansMultipleDays = true;
      }
    }

    renderTranscript(steps, container);
    renderStats(steps);
  } catch (e) {
    if (generation !== transcriptGeneration) return;
    container.innerHTML =
      '<div class="loading-state" style="text-align:center; color:red;">Failed to load transcript.</div>';
  }
}
