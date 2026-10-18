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
import { escapeHtml, fetchJson, section, FLAVOR_LABELS } from "./utils.js";
import { drillRow, wireDrilldown } from "./drilldown.js";

function formatDuration(totalSeconds) {
  const secs = Math.round(totalSeconds || 0);
  const mins = Math.floor(secs / 60);
  const hrs = Math.floor(mins / 60);
  if (hrs > 0) return `${hrs}h ${mins % 60}m`;
  if (mins > 0) return `${mins}m ${secs % 60}s`;
  return `${secs}s`;
}

function statCard(label, value, sub) {
  return `<div class="stat-card">
      <div class="stat-label">${label}</div>
      <div class="stat-value">${escapeHtml(String(value))}</div>
      <div class="stat-sub">${escapeHtml(sub)}</div>
    </div>`;
}

// Each row drills into the sessions behind it: `category` is the drill-down kind (tool/error/…).
function barList(items, color, emptyText, category) {
  if (!items || items.length === 0) {
    return `<div class="stat-sub">${escapeHtml(emptyText)}</div>`;
  }
  const max = items[0].count || 1;
  return items
    .map((it) => {
      const width = Math.max((it.count / max) * 100, 2);
      const inner = `<div style="display:flex; align-items:center; gap:16px; margin-bottom:4px;">
            <div style="flex:0 0 280px; font-size:0.82rem; color:var(--text-primary); text-align:right; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">${escapeHtml(
              it.name
            )}</div>
            <div style="flex:1; height:8px; background:rgba(30,41,59,0.5); border-radius:4px; overflow:hidden;">
              <div style="height:100%; width:${width}%; background:${color}; border-radius:4px;"></div>
            </div>
            <div style="flex:0 0 36px; font-size:0.85rem; color:var(--text-secondary); font-weight:600;">${
              it.count
            }</div>
          </div>`;
      return drillRow(category, it.name, inner);
    })
    .join("");
}

export function renderInsights(report, container) {
  if (!container) return;
  const r = report || {};
  const tools = r.topTools || [];
  const errors = r.topErrors || [];
  const recs = r.topRecommendations || [];
  const issues = r.topIssues || [];
  const label = FLAVOR_LABELS[r.flavor] || r.flavor || "Sessions";

  const sampledNote =
    r.sampledSessions < r.sessionCount
      ? ` (analyzed ${r.sampledSessions} most recent)`
      : "";

  container.innerHTML = `
    <div class="insights-view" style="padding:8px 4px 40px;">
      <h2 style="margin:0 0 4px; font-size:1.4rem; color:var(--text-primary);">Fleet Insights</h2>
      <div class="stat-sub" style="margin-bottom:20px;">${escapeHtml(
        label
      )} · ${r.sessionCount || 0} sessions${escapeHtml(sampledNote)}</div>

      <div class="stats-grid">
        ${statCard(
          "SESSIONS",
          r.sessionCount || 0,
          `${r.analyzedSessions || 0} AI-analyzed`
        )}
        ${statCard(
          "OUTCOMES",
          `${r.cleanSessions || 0} clean`,
          `${r.sessionsWithErrors || 0} hit errors`
        )}
        ${statCard(
          "TOOL CALLS",
          r.toolCallTotal || 0,
          `${r.avgToolsPerSession || 0} avg per session`
        )}
        ${statCard(
          "AVG DURATION",
          formatDuration(r.avgDurationSeconds),
          "per session"
        )}
      </div>

      ${section(
        "Top Tools",
        "var(--accent-purple)",
        barList(tools, "var(--accent-purple)", "No tool calls recorded", "tool")
      )}
      ${section(
        "Most Common Errors",
        "var(--error)",
        barList(errors, "var(--error)", "No errors detected 🎉", "error")
      )}
      ${section(
        "Recommendation Backlog",
        "#10b981",
        barList(
          recs,
          "#10b981",
          "No recommendations yet — generate AI analysis on more sessions",
          "recommendation"
        )
      )}
      ${section(
        "Recurring Issues",
        "#f59e0b",
        barList(issues, "#f59e0b", "No issues recorded yet", "issue")
      )}
    </div>`;

  // Each tally row expands to the sessions behind it (drill-down).
  wireDrilldown(container, r.flavor);
}

export async function showInsights(flavor) {
  const container = document.getElementById("transcript-container");
  if (!container) return;
  container.innerHTML =
    '<div class="loading-state" style="text-align:center; padding:40px; color:#94a3b8;">Computing fleet insights…</div>';
  try {
    const report = await fetchJson(
      `/api/insights?flavor=${encodeURIComponent(flavor)}`
    );
    renderInsights(report, container);
  } catch (e) {
    container.innerHTML =
      '<div class="loading-state" style="text-align:center; color:red;">Failed to load insights.</div>';
  }
}
