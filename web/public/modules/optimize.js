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
import { escapeHtml, fetchJson, round1, FLAVOR_LABELS } from "./utils.js";

const CHECK_LABELS = {
  "schema-complete": "Has title, summary & flow",
  "has-recommendations": "Proposes a recommendation",
  "issues-have-fixes": "Every issue records a fix",
  "concise-summary": "Summary present & not oversized",
  "not-degenerate": "No repetition / Base64 blobs",
  "error-coverage": "Errored runs surface an issue",
};

function variantColumn(title, v, isWinner) {
  const r = v || {};
  const rates = (r.checkPassRates || [])
    .map(
      (c) =>
        `<div style="display:flex; justify-content:space-between; gap:12px; font-size:0.8rem; margin-bottom:4px;">
          <span style="color:var(--text-secondary);">${escapeHtml(
            CHECK_LABELS[c.name] || c.name
          )}</span><span style="color:var(--text-primary);">${c.count}/${
          r.scored || 0
        }</span>
        </div>`
    )
    .join("");
  const border = isWinner
    ? "2px solid #10b981"
    : "1px solid var(--border-color)";
  return `<div style="flex:1; min-width:0; background:rgba(30,41,59,0.4); border:${border}; border-radius:10px; padding:14px 16px;">
      <div style="display:flex; justify-content:space-between; align-items:baseline;">
        <div style="font-weight:700; color:var(--text-primary);">${escapeHtml(
          title
        )}</div>
        ${
          isWinner
            ? '<span style="font-size:0.72rem; color:#10b981;">▲ winner</span>'
            : ""
        }
      </div>
      <div style="font-size:2rem; font-weight:700; color:var(--text-primary); margin:6px 0;">${escapeHtml(
        String(round1(r.avgScore))
      )}<span style="font-size:0.9rem; color:var(--text-secondary);"> / 100</span></div>
      <div class="stat-sub" style="margin-bottom:10px;">${
        r.scored || 0
      } analyses scored</div>
      ${rates}
    </div>`;
}

export function renderOptimizeResults(report) {
  const r = report || {};
  if (!r.sampleSize) {
    return `<div style="margin-top:16px; padding:12px 14px; background:rgba(245,158,11,0.1); border:1px solid rgba(245,158,11,0.4); border-radius:8px; font-size:0.85rem; color:var(--text-primary);">${escapeHtml(
      r.note || "No comparison to show."
    )}</div>`;
  }
  const a = r.a || {};
  const b = r.b || {};
  const diff = round1((b.avgScore || 0) - (a.avgScore || 0));
  const verdict =
    diff > 0
      ? `Prompt B wins by +${diff}`
      : diff < 0
      ? `Prompt A wins by +${round1(-diff)}`
      : "It's a tie";
  return `<div style="margin-top:18px;">
      <div style="font-size:0.9rem; font-weight:700; color:var(--text-primary); margin-bottom:10px;">${escapeHtml(
        verdict
      )} <span class="stat-sub" style="font-weight:400;">· ${
    r.sampleSize
  } sessions sampled</span></div>
      <div style="display:flex; gap:16px;">
        ${variantColumn("Prompt A (baseline)", a, diff < 0)}
        ${variantColumn("Prompt B (variant)", b, diff > 0)}
      </div>
    </div>`;
}

const TEXTAREA_STYLE =
  "width:100%; box-sizing:border-box; min-height:120px; font-family:var(--mono,monospace); font-size:0.82rem; padding:10px; background:rgba(15,23,42,0.6); color:var(--text-primary); border:1px solid var(--border-color); border-radius:8px; resize:vertical;";

export function renderLab(flavor, def, container) {
  if (!container) return;
  const label = FLAVOR_LABELS[flavor] || flavor || "Sessions";
  const instruction = (def && def.instruction) || "";
  const maxSample = (def && def.maxSample) || 5;
  container.innerHTML = `
    <div class="optimize-view" style="padding:8px 4px 40px;">
      <h2 style="margin:0 0 4px; font-size:1.4rem; color:var(--text-primary);">Prompt Lab</h2>
      <div class="stat-sub" style="margin-bottom:18px;">Compare two analysis prompts on a sample of ${escapeHtml(
        label
      )} sessions — re-analyzed and scored by the deterministic eval.</div>

      <label style="display:block; font-size:0.78rem; font-weight:700; color:var(--text-secondary); margin-bottom:6px; text-transform:uppercase; letter-spacing:0.04em;">Prompt A (baseline)</label>
      <textarea id="opt-a" style="${TEXTAREA_STYLE}">${escapeHtml(
    instruction
  )}</textarea>

      <label style="display:block; font-size:0.78rem; font-weight:700; color:var(--text-secondary); margin:14px 0 6px; text-transform:uppercase; letter-spacing:0.04em;">Prompt B (variant)</label>
      <textarea id="opt-b" style="${TEXTAREA_STYLE}">${escapeHtml(
    instruction
  )}</textarea>

      <div style="display:flex; align-items:center; gap:12px; margin-top:14px;">
        <label style="font-size:0.85rem; color:var(--text-secondary);">Sample size
          <input id="opt-n" type="number" min="1" max="${maxSample}" value="3" style="width:56px; margin-left:6px; padding:4px 6px; background:rgba(15,23,42,0.6); color:var(--text-primary); border:1px solid var(--border-color); border-radius:6px;" />
        </label>
        <button id="opt-run" class="btn" style="padding:8px 14px; font-size:0.85rem; background:rgba(139,92,246,0.15); border:1px solid var(--accent-purple); color:var(--text-primary); cursor:pointer; border-radius:8px;">Run comparison</button>
      </div>
      <div id="opt-results"></div>
    </div>`;

  const runBtn = container.querySelector("#opt-run");
  if (runBtn) {
    runBtn.addEventListener("click", () => runComparison(flavor, container));
  }
}

async function runComparison(flavor, container) {
  const results = container.querySelector("#opt-results");
  const runBtn = container.querySelector("#opt-run");
  const sampleSize = Number(container.querySelector("#opt-n").value) || 3;
  const instructionA = container.querySelector("#opt-a").value;
  const instructionB = container.querySelector("#opt-b").value;
  if (results) {
    results.innerHTML = `<div class="stat-sub" style="margin-top:16px;">Re-analyzing &amp; scoring…</div>`;
  }
  if (runBtn) runBtn.disabled = true;
  try {
    const report = await fetchJson("/api/optimize", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ flavor, sampleSize, instructionA, instructionB }),
    });
    if (results) results.innerHTML = renderOptimizeResults(report);
  } catch (e) {
    if (results) {
      results.innerHTML = `<div class="stat-sub" style="margin-top:16px; color:red;">Comparison failed.</div>`;
    }
  } finally {
    if (runBtn) runBtn.disabled = false;
  }
}

export async function showOptimize(flavor) {
  const container = document.getElementById("transcript-container");
  if (!container) return;
  container.innerHTML =
    '<div class="loading-state" style="text-align:center; padding:40px; color:#94a3b8;">Loading the prompt lab…</div>';
  let def = { instruction: "", maxSample: 5 };
  try {
    def = await fetchJson("/api/optimize");
  } catch (e) {
    // Fall back to empty editors.
  }
  renderLab(flavor, def, container);
}
