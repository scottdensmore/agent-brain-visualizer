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
import {
  slugify,
  skillMarkdown,
  agentsMarkdown,
  downloadText,
} from "./mine-export.js";
import { drillRow, wireDrilldown } from "./drilldown.js";

const DL_BTN_STYLE =
  "padding:4px 10px; font-size:0.72rem; background:rgba(30,41,59,0.6); border:1px solid var(--border-color); color:var(--text-primary); cursor:pointer; border-radius:6px; white-space:nowrap;";

function emptyNote(text) {
  return `<div class="stat-sub">${escapeHtml(text)}</div>`;
}

// A skill proposal: name + when-to-use header, numbered body in a copyable block, and a download.
function skillCard(skill, index) {
  const body = skill.body || "";
  return `<div style="background:rgba(30,41,59,0.4); border:1px solid var(--border-color); border-radius:10px; padding:14px 16px; margin-bottom:12px;">
      <div style="display:flex; justify-content:space-between; align-items:baseline; gap:12px;">
        <div style="font-family:var(--mono,monospace); font-size:0.9rem; color:var(--accent-purple); font-weight:700;">${escapeHtml(
          skill.name || "skill"
        )}</div>
        <button class="skill-dl-btn" data-skill-index="${index}" style="${DL_BTN_STYLE}">⬇ SKILL.md</button>
      </div>
      <div class="stat-sub" style="margin:4px 0 10px;">${escapeHtml(
        skill.whenToUse || ""
      )}</div>
      <pre style="white-space:pre-wrap; margin:0; font-size:0.82rem; color:var(--text-primary); font-family:var(--mono,monospace);">${escapeHtml(
        body
      )}</pre>
    </div>`;
}

// An AGENTS.md rule: imperative bullet + muted rationale.
function ruleRow(rule) {
  return `<div style="display:flex; gap:12px; margin-bottom:12px;">
      <div style="color:#10b981; flex:0 0 auto;">▸</div>
      <div>
        <div style="font-size:0.9rem; color:var(--text-primary);">${escapeHtml(
          rule.rule || ""
        )}</div>
        <div class="stat-sub" style="margin-top:2px;">${escapeHtml(
          rule.rationale || ""
        )}</div>
      </div>
    </div>`;
}

// A failure→fix pair from the structural evidence (drills into the sessions that hit the error).
function fixRow(pair) {
  const fix = pair.fix && pair.fix.trim() ? pair.fix : "(no fix recorded)";
  const inner = `<div style="display:flex; gap:12px; margin-bottom:2px; align-items:baseline;">
      <div style="flex:0 0 40px; font-size:0.85rem; color:var(--text-secondary); font-weight:600;">×${
        pair.count || 1
      }</div>
      <div>
        <div style="font-size:0.88rem; color:var(--error);">${escapeHtml(
          pair.error || ""
        )}</div>
        <div class="stat-sub" style="margin-top:2px;">→ ${escapeHtml(fix)}</div>
      </div>
    </div>`;
  return drillRow("issue", pair.error || "", inner);
}

// A recurring workflow (tool-sequence n-gram); drills into the sessions that ran it.
function sequenceRow(seq) {
  const inner = `<div style="display:flex; gap:12px; margin-bottom:2px; align-items:baseline;">
      <div style="flex:0 0 40px; font-size:0.85rem; color:var(--text-secondary); font-weight:600;">×${
        seq.count || 1
      }</div>
      <div style="font-size:0.85rem; color:var(--text-primary); font-family:var(--mono,monospace);">${escapeHtml(
        seq.name || ""
      )}</div>
    </div>`;
  return drillRow("workflow", seq.name || "", inner);
}

function list(items, render, emptyText) {
  if (!items || items.length === 0) return emptyNote(emptyText);
  return items.map(render).join("");
}

export function renderMining(report, container) {
  if (!container) return;
  const r = report || {};
  const label = FLAVOR_LABELS[r.flavor] || r.flavor || "Sessions";
  const skills = r.skills || [];
  const rules = r.agentsRules || [];
  const gaps = r.toolingGaps || [];
  const sequences = r.toolSequences || [];
  const fixes = r.failureFixes || [];
  const recs = r.recommendations || [];

  const noteHtml =
    r.aiGenerated === false && r.note
      ? `<div style="margin:14px 0; padding:10px 14px; background:rgba(245,158,11,0.1); border:1px solid rgba(245,158,11,0.4); border-radius:8px; font-size:0.85rem; color:var(--text-primary);">${escapeHtml(
          r.note
        )}</div>`
      : "";

  const agentsDownload =
    rules.length > 0
      ? `<button id="agents-dl-btn" style="${DL_BTN_STYLE} margin-bottom:14px;">⬇ AGENTS.md</button>`
      : "";

  // The AI-proposed assets only render when the model ran; otherwise we lead with the evidence.
  const proposalsHtml = r.aiGenerated
    ? section(
        "Proposed Skills",
        "var(--accent-purple)",
        list(skills, skillCard, "No skills proposed")
      ) +
      section(
        "Proposed AGENTS.md Rules",
        "#10b981",
        agentsDownload + list(rules, ruleRow, "No rules proposed")
      ) +
      section(
        "Tooling Gaps",
        "#f59e0b",
        list(
          gaps,
          (g) =>
            `<div style="font-size:0.88rem; color:var(--text-primary); margin-bottom:6px;">• ${escapeHtml(
              g
            )}</div>`,
          "No tooling gaps identified"
        )
      )
    : "";

  container.innerHTML = `
    <div class="mining-view" style="padding:8px 4px 40px;">
      <h2 style="margin:0 0 4px; font-size:1.4rem; color:var(--text-primary);">Mined Improvements</h2>
      <div class="stat-sub" style="margin-bottom:8px;">${escapeHtml(label)} · ${
    r.sessionCount || 0
  } sessions · ${r.analyzedSessions || 0} AI-analyzed</div>
      ${noteHtml}
      ${proposalsHtml}
      ${section(
        "Evidence · Recurring Workflows",
        "var(--accent-purple)",
        list(sequences, sequenceRow, "No recurring tool sequences yet")
      )}
      ${section(
        "Evidence · Failure → Fix",
        "var(--error)",
        list(fixes, fixRow, "No failures recorded in analyses yet")
      )}
      ${section(
        "Evidence · Recommendation Backlog",
        "#10b981",
        list(
          recs,
          (rec) =>
            drillRow(
              "recommendation",
              rec.name || "",
              `<div style="display:flex; gap:12px; margin-bottom:2px;"><div style="flex:0 0 40px; font-size:0.85rem; color:var(--text-secondary); font-weight:600;">×${
                rec.count || 1
              }</div><div style="font-size:0.88rem; color:var(--text-primary);">${escapeHtml(
                rec.name || ""
              )}</div></div>`
            ),
          "No recommendations yet — generate AI analysis on more sessions"
        )
      )}
    </div>`;

  // Wire the download buttons (present only when the model proposed skills/rules).
  container.querySelectorAll(".skill-dl-btn").forEach((btn) => {
    btn.addEventListener("click", () => {
      const skill = skills[Number(btn.dataset.skillIndex)];
      if (skill)
        downloadText(`${slugify(skill.name)}.md`, skillMarkdown(skill));
    });
  });
  const agentsBtn = container.querySelector("#agents-dl-btn");
  if (agentsBtn) {
    agentsBtn.addEventListener("click", () =>
      downloadText("AGENTS.md", agentsMarkdown(r))
    );
  }

  // Evidence rows (workflows / failures / recommendations) drill into their sessions.
  wireDrilldown(container, r.flavor);
}

export async function showMining(flavor) {
  const container = document.getElementById("transcript-container");
  if (!container) return;
  container.innerHTML =
    '<div class="loading-state" style="text-align:center; padding:40px; color:#94a3b8;">Mining skills &amp; rules from sessions…</div>';
  try {
    const report = await fetchJson(
      `/api/mine?flavor=${encodeURIComponent(flavor)}`
    );
    renderMining(report, container);
  } catch (e) {
    container.innerHTML =
      '<div class="loading-state" style="text-align:center; color:red;">Failed to mine improvements.</div>';
  }
}
