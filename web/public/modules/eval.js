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
import {
  escapeHtml,
  formatTime,
  apiFetch,
  fetchJson,
  section,
  round1,
  FLAVOR_LABELS,
} from "./utils.js";
import { downloadText } from "./mine-export.js";
import { historyCsv } from "./eval-export.js";

// Human-readable descriptions for the deterministic checks (must match EvalScorer check names).
const CHECK_LABELS = {
  "schema-complete": "Has title, summary & flow",
  "has-recommendations": "Proposes a recommendation",
  "issues-have-fixes": "Every issue records a fix",
  "concise-summary": "Summary present & not oversized",
  "not-degenerate": "No repetition / Base64 blobs",
  "error-coverage": "Errored runs surface an issue",
};

function scoreColor(score) {
  if (score >= 80) return "#10b981";
  if (score >= 50) return "#f59e0b";
  return "var(--error)";
}

// Lower panel spread = more agreement (green); a wide spread means the lenses disagreed (red).
function spreadColor(spread) {
  if (spread < 0.5) return "#10b981";
  if (spread < 1.5) return "#f59e0b";
  return "var(--error)";
}

// Average per-session panel spread (panelMax − panelMin) over the multi-lens judged cases, or null.
function avgPanelSpread(cases) {
  const spreads = (cases || [])
    .filter((c) => c.samples > 1)
    .map((c) => Math.max(0, (c.panelMax || 0) - (c.panelMin || 0)));
  if (spreads.length === 0) return null;
  return round1(spreads.reduce((a, b) => a + b, 0) / spreads.length);
}

function passRateRow(item, evaluated, history) {
  const label = CHECK_LABELS[item.name] || item.name;
  const pct = evaluated > 0 ? Math.round((item.count / evaluated) * 100) : 0;
  // A trend of this check's pass rate across saved runs (blank until there are two).
  const trend = sparkline(checkRates(history, item.name), 1, { w: 84, h: 18 });
  return `<div style="display:flex; align-items:center; gap:16px; margin-bottom:10px;">
      <div style="flex:0 0 260px; font-size:0.82rem; color:var(--text-primary); text-align:right;" title="${escapeHtml(
        item.name
      )}">${escapeHtml(label)}</div>
      <div style="flex:1; height:8px; background:rgba(30,41,59,0.5); border-radius:4px; overflow:hidden;">
        <div style="height:100%; width:${pct}%; background:${scoreColor(
    pct
  )}; border-radius:4px;"></div>
      </div>
      <div style="flex:0 0 70px; font-size:0.85rem; color:var(--text-secondary); font-weight:600;">${
        item.count
      }/${evaluated} · ${pct}%</div>
      <div style="flex:0 0 84px;" title="Pass-rate trend across saved runs">${trend}</div>
    </div>`;
}

function rubricCard(label, value) {
  const v = value ?? 0;
  return `<div class="stat-card">
      <div class="stat-label">${label}</div>
      <div class="stat-value" style="color:${scoreColor(v * 20)};">${escapeHtml(
    String(v)
  )}<span style="font-size:0.9rem; color:var(--text-secondary);"> / 5</span></div>
      <div class="stat-sub">average</div>
    </div>`;
}

function judgedRow(c) {
  const panel = c.samples ? `${c.samples}-judge panel · ` : "";
  // Show the panel's overall-score spread when the lenses disagreed (amber past a point of a point).
  const hasSpread = c.samples > 1 && c.panelMax > c.panelMin;
  const spreadVal = round1((c.panelMax || 0) - (c.panelMin || 0));
  const spreadHtml = hasSpread
    ? `<span style="color:${
        spreadVal >= 1 ? "#f59e0b" : "var(--text-secondary)"
      };">panel ${round1(c.panelMin)}–${round1(c.panelMax)}</span> · `
    : "";
  return `<div style="display:flex; gap:16px; align-items:baseline; margin-bottom:12px;">
      <div style="flex:0 0 150px; font-size:0.82rem; color:var(--text-secondary); font-weight:600;">F ${round1(
        c.faithfulness
      )} · A ${round1(c.actionability)} · C ${round1(c.clarity)}</div>
      <div style="min-width:0;">
        <div style="font-size:0.88rem; color:var(--text-primary); overflow:hidden; text-overflow:ellipsis; white-space:nowrap;" title="${escapeHtml(
          c.sessionId || ""
        )}">${escapeHtml(c.title || c.sessionId || "session")}</div>
        <div class="stat-sub" style="margin-top:2px;">${escapeHtml(
          panel
        )}${spreadHtml}${escapeHtml(c.comment || "")}</div>
      </div>
    </div>`;
}

function judgeHtmlFor(report, evaluated) {
  const j = report.judge || {};
  if (j.ran) {
    const cards = `<div class="stats-grid">
        ${rubricCard("FAITHFULNESS", j.avgFaithfulness)}
        ${rubricCard("ACTIONABILITY", j.avgActionability)}
        ${rubricCard("CLARITY", j.avgClarity)}
      </div>`;
    const rows = (j.cases || []).map(judgedRow).join("");
    const spread = avgPanelSpread(j.cases);
    const agreement =
      spread === null
        ? ""
        : `<div class="stat-sub" style="margin-top:10px;">Panel agreement: <strong style="color:${spreadColor(
            spread
          )};">±${spread}</strong> average spread (0 = the lenses agreed).</div>`;
    return section(
      `LLM judge · rubric 1–5 · ${j.judgedSessions || 0} judged`,
      "var(--accent-purple)",
      cards + agreement + `<div style="margin-top:20px;">${rows}</div>`
    );
  }
  if (evaluated > 0) {
    return `<div style="margin-top:28px;">
        <button id="run-judge-btn" class="btn" style="padding:8px 14px; font-size:0.85rem; background:rgba(139,92,246,0.15); border:1px solid var(--accent-purple); color:var(--text-primary); cursor:pointer; border-radius:8px;">⚖️ Run LLM judge</button>
        <div class="stat-sub" style="margin-top:8px;">${escapeHtml(
          j.note || ""
        )}</div>
      </div>`;
  }
  return "";
}

function worstRow(c) {
  const failed = (c.failed || []).map((f) => CHECK_LABELS[f] || f);
  const failsHtml = failed.length
    ? failed.map((f) => `<span>✗ ${escapeHtml(f)}</span>`).join("")
    : "<span>—</span>";
  return `<div style="display:flex; gap:16px; align-items:baseline; margin-bottom:12px;">
      <div style="flex:0 0 44px; font-size:1rem; font-weight:700; color:${scoreColor(
        c.score
      )};">${c.score}</div>
      <div style="min-width:0;">
        <div style="font-size:0.88rem; color:var(--text-primary); overflow:hidden; text-overflow:ellipsis; white-space:nowrap;" title="${escapeHtml(
          c.sessionId || ""
        )}">${escapeHtml(c.title || c.sessionId || "session")}</div>
        <div class="stat-sub" style="display:flex; flex-wrap:wrap; gap:10px; margin-top:2px;">${failsHtml}</div>
      </div>
    </div>`;
}

// A signed delta badge vs the previous run's score (green up / red down / muted flat).
function deltaBadge(delta) {
  if (delta === null || delta === undefined) return "";
  const color =
    delta > 0
      ? "#10b981"
      : delta < 0
      ? "var(--error)"
      : "var(--text-secondary)";
  const sign = delta > 0 ? "+" : "";
  return `<span style="margin-left:8px; font-size:0.78rem; color:${color};">${sign}${escapeHtml(
    String(round1(delta))
  )}</span>`;
}

function historyRow(run, prev, index) {
  const delta = prev ? run.avgScore - prev.avgScore : null;
  const judged = run.judged
    ? ` · judge F ${round1(run.avgFaithfulness)} / A ${round1(
        run.avgActionability
      )} / C ${round1(run.avgClarity)}`
    : "";
  return `<div style="display:flex; gap:12px; align-items:baseline; margin-bottom:10px;">
      <input type="checkbox" class="cmp-check" data-run-index="${index}" title="Select two runs to compare" style="margin-top:4px; cursor:pointer;" />
      <div style="flex:0 0 70px; font-size:1rem; font-weight:700; color:${scoreColor(
        run.avgScore || 0
      )};">${escapeHtml(String(round1(run.avgScore)))}${deltaBadge(delta)}</div>
      <div style="min-width:0;">
        <div style="font-size:0.85rem; color:var(--text-primary);">${escapeHtml(
          run.modelLabel || "—"
        )}</div>
        <div class="stat-sub">${escapeHtml(formatTime(run.savedAt))} · ${
    run.evaluatedSessions || 0
  } evaluated${escapeHtml(judged)}</div>
      </div>
      <button class="run-del-btn" data-saved-at="${escapeHtml(
        run.savedAt || ""
      )}" title="Delete this run" style="margin-left:auto; background:transparent; border:none; color:var(--text-secondary); cursor:pointer; font-size:1rem; line-height:1; padding:2px 6px;">×</button>
    </div>`;
}

// A before→after comparison of two saved runs (A = older, B = newer): score, per-check pass
// counts, and rubric, each with the B−A delta. Pure/exported for testing.
export function renderComparison(a, b) {
  if (!a || !b) return "";
  const rows = [
    cmpRow("Avg score", a.avgScore, b.avgScore),
    cmpRow("Evaluated", a.evaluatedSessions, b.evaluatedSessions),
  ];
  for (const name of checkUnion(a, b)) {
    rows.push(
      cmpRow(CHECK_LABELS[name] || name, passCount(a, name), passCount(b, name))
    );
  }
  if (a.judged && b.judged) {
    rows.push(cmpRow("Faithfulness", a.avgFaithfulness, b.avgFaithfulness));
    rows.push(cmpRow("Actionability", a.avgActionability, b.avgActionability));
    rows.push(cmpRow("Clarity", a.avgClarity, b.avgClarity));
  }
  const head = (label, run) =>
    `<div style="font-size:0.8rem; color:var(--text-primary);">${escapeHtml(
      label
    )}: <strong>${escapeHtml(run.modelLabel || "—")}</strong> · ${escapeHtml(
      formatTime(run.savedAt)
    )}</div>`;
  return `<div style="margin-top:16px; padding:14px 16px; background:rgba(30,41,59,0.4); border:1px solid var(--border-color); border-radius:10px;">
      <div style="margin-bottom:10px;">${head("A", a)}${head("B", b)}</div>
      <table style="width:100%; border-collapse:collapse; font-size:0.85rem;">
        <thead><tr style="color:var(--text-secondary); text-align:right;">
          <th style="text-align:left;">Metric</th><th>A</th><th>B</th><th>Δ (B−A)</th>
        </tr></thead>
        <tbody>${rows.join("")}</tbody>
      </table>
    </div>`;
}

function checkUnion(a, b) {
  const names = [];
  const seen = new Set();
  for (const run of [a, b]) {
    for (const c of run.checkPassRates || []) {
      if (c && c.name && !seen.has(c.name)) {
        seen.add(c.name);
        names.push(c.name);
      }
    }
  }
  return names;
}

function passCount(run, name) {
  const found = (run.checkPassRates || []).find((c) => c.name === name);
  return found ? found.count : 0;
}

function cmpRow(label, av, bv) {
  const a1 = round1(av);
  const b1 = round1(bv);
  const d = round1(b1 - a1);
  const color =
    d > 0 ? "#10b981" : d < 0 ? "var(--error)" : "var(--text-secondary)";
  const sign = d > 0 ? "+" : "";
  return `<tr style="text-align:right;">
      <td style="text-align:left; color:var(--text-primary); padding:2px 0;">${escapeHtml(
        label
      )}</td>
      <td style="color:var(--text-secondary);">${a1}</td>
      <td style="color:var(--text-primary);">${b1}</td>
      <td style="color:${color}; font-weight:600;">${sign}${d}</td>
    </tr>`;
}

// A tiny inline SVG sparkline of avg score over the saved runs (oldest → newest). All coordinates
// derive from numeric scores, so nothing here is attacker-controllable. Empty until there are 2 runs.
// A reusable inline SVG sparkline for a numeric series (clamped to [0, max]). All coordinates derive
// from the numbers, so there is nothing to escape. Empty until there are two points.
export function sparkline(values, max, opts = {}) {
  const { w = 80, h = 20, marker = false, strokeWidth = 1.5 } = opts;
  const vals = Array.isArray(values) ? values : [];
  if (vals.length < 2) return "";
  const pad = marker ? 4 : 2;
  const domain = max > 0 ? max : 1;
  const x = (i) => pad + (i * (w - 2 * pad)) / (vals.length - 1);
  const y = (v) => {
    const c = Math.max(0, Math.min(domain, v || 0));
    return h - pad - (c / domain) * (h - 2 * pad);
  };
  const pts = vals
    .map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`)
    .join(" ");
  const dot = marker
    ? `<circle cx="${x(vals.length - 1).toFixed(1)}" cy="${y(
        vals[vals.length - 1]
      ).toFixed(1)}" r="3" fill="var(--accent-purple)" />`
    : "";
  const style = marker
    ? "display:block; margin-bottom:12px;"
    : "display:inline-block; vertical-align:middle;";
  return `<svg width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" style="${style}" aria-hidden="true"><polyline points="${pts}" fill="none" stroke="var(--accent-purple)" stroke-width="${strokeWidth}" stroke-linejoin="round" stroke-linecap="round" />${dot}</svg>`;
}

// The per-run pass rate (0–1) of one check across the saved history, oldest → newest.
function checkRates(history, name) {
  const runs = Array.isArray(history) ? [...history].reverse() : [];
  return runs.map((run) => {
    const found = (run.checkPassRates || []).find((c) => c.name === name);
    const evald = run.evaluatedSessions || 0;
    return evald > 0 && found ? found.count / evald : 0;
  });
}

export function scoreSparkline(history) {
  const runs = Array.isArray(history) ? [...history].reverse() : [];
  return sparkline(
    runs.map((r) => r.avgScore),
    100,
    {
      w: 240,
      h: 40,
      marker: true,
      strokeWidth: 2,
    }
  );
}

function historySection(history) {
  if (!Array.isArray(history) || history.length === 0) {
    return section(
      "Run history",
      "var(--text-secondary)",
      `<div class="stat-sub">No saved runs yet — save a run to start tracking quality over time.</div>`
    );
  }
  // history is newest-first; compare each run to the next (older) one.
  const rows = history
    .map((run, i) => historyRow(run, history[i + 1], i))
    .join("");
  const csvBtn = `<button id="history-csv-btn" style="padding:4px 10px; font-size:0.78rem; background:rgba(30,41,59,0.6); border:1px solid var(--border-color); color:var(--text-primary); cursor:pointer; border-radius:6px; margin-bottom:12px;">⬇ CSV</button>`;
  const hint = `<div class="stat-sub" style="margin-bottom:10px;">Tick two runs to compare them.</div>`;
  const compareBox = `<div id="run-compare"></div>`;
  return section(
    "Run history",
    "var(--text-secondary)",
    csvBtn + scoreSparkline(history) + hint + rows + compareBox
  );
}

export function renderEval(report, container, history = []) {
  if (!container) return;
  const r = report || {};
  const label = FLAVOR_LABELS[r.flavor] || r.flavor || "Sessions";
  const evaluated = r.evaluatedSessions || 0;
  const passRates = r.checkPassRates || [];
  const worst = r.worstCases || [];

  const bodyHtml =
    evaluated === 0
      ? `<div style="margin-top:20px; padding:14px 16px; background:rgba(245,158,11,0.1); border:1px solid rgba(245,158,11,0.4); border-radius:8px; font-size:0.88rem; color:var(--text-primary);">No analyzed sessions yet — run AI analysis on some sessions, then re-run the eval to score them.</div>`
      : section(
          "Check pass-rates",
          "var(--accent-purple)",
          passRates.map((it) => passRateRow(it, evaluated, history)).join("")
        ) +
        section(
          "Lowest-scoring analyses",
          "var(--error)",
          worst.map(worstRow).join("")
        );

  const saveBtn =
    evaluated > 0
      ? `<button id="save-run-btn" class="btn" style="padding:6px 12px; font-size:0.82rem; background:rgba(30,41,59,0.5); border:1px solid var(--border-color); color:var(--text-primary); cursor:pointer; border-radius:8px;">💾 Save run</button>`
      : "";

  container.innerHTML = `
    <div class="eval-view" style="padding:8px 4px 40px;">
      <div style="display:flex; justify-content:space-between; align-items:flex-start; gap:12px;">
        <div>
          <h2 style="margin:0 0 4px; font-size:1.4rem; color:var(--text-primary);">Analysis Eval</h2>
          <div class="stat-sub" style="margin-bottom:20px;">${escapeHtml(
            label
          )} · ${r.sessionCount || 0} sessions · scored by <strong>${escapeHtml(
    r.modelLabel || "—"
  )}</strong></div>
        </div>
        ${saveBtn}
      </div>

      <div class="stats-grid">
        <div class="stat-card">
          <div class="stat-label">AVG SCORE</div>
          <div class="stat-value" style="color:${scoreColor(
            r.avgScore || 0
          )};">${escapeHtml(String(r.avgScore ?? 0))}</div>
          <div class="stat-sub">out of 100</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">EVALUATED</div>
          <div class="stat-value">${evaluated}</div>
          <div class="stat-sub">of ${r.sessionCount || 0} sessions</div>
        </div>
      </div>
      ${bodyHtml}
      ${judgeHtmlFor(r, evaluated)}
      ${historySection(history)}
    </div>`;

  // The "Run LLM judge" button re-fetches this same source with the judge enabled.
  const judgeBtn = container.querySelector("#run-judge-btn");
  if (judgeBtn) {
    judgeBtn.addEventListener("click", () => showEval(r.flavor, true));
  }

  // "Save run" persists this snapshot, then refreshes the history in place (no re-eval).
  const saveRunBtn = container.querySelector("#save-run-btn");
  if (saveRunBtn) {
    saveRunBtn.addEventListener("click", () => saveRun(r, container));
  }

  // "CSV" downloads the saved run history as a spreadsheet-friendly file.
  const csvBtn = container.querySelector("#history-csv-btn");
  if (csvBtn) {
    csvBtn.addEventListener("click", () =>
      downloadText(
        `eval-history-${r.flavor || "sessions"}.csv`,
        historyCsv(history),
        "text/csv;charset=utf-8"
      )
    );
  }

  // Each run's "×" deletes that snapshot, then refreshes the history in place.
  container.querySelectorAll(".run-del-btn").forEach((btn) => {
    btn.addEventListener("click", () =>
      deleteRun(btn.dataset.savedAt, r, container)
    );
  });

  // Ticking two run checkboxes renders an A(older)↔B(newer) diff below the history.
  const compareBox = container.querySelector("#run-compare");
  const checks = Array.from(container.querySelectorAll(".cmp-check"));
  const updateCompare = () => {
    const picked = checks
      .filter((c) => c.checked)
      .map((c) => Number(c.dataset.runIndex))
      .sort((x, y) => y - x); // descending index => [0] is older, [1] is newer
    compareBox.innerHTML =
      picked.length === 2
        ? renderComparison(history[picked[0]], history[picked[1]])
        : "";
  };
  checks.forEach((c) => c.addEventListener("change", updateCompare));
}

// Guards overlapping async renders (reload / save / delete): each op takes a generation, and only
// the latest-started one is allowed to write to the DOM — so a slow earlier op can't clobber a
// newer render with stale data.
let renderGeneration = 0;

export async function showEval(flavor, judge = false) {
  const container = document.getElementById("transcript-container");
  if (!container) return;
  const gen = ++renderGeneration;
  const loading = judge
    ? "Running the LLM judge…"
    : "Scoring analysis quality…";
  container.innerHTML = `<div class="loading-state" style="text-align:center; padding:40px; color:#94a3b8;">${loading}</div>`;
  try {
    const url = `/api/eval?flavor=${encodeURIComponent(flavor)}${
      judge ? "&judge=true" : ""
    }`;
    const [report, history] = await Promise.all([
      fetchJson(url),
      fetchHistory(flavor),
    ]);
    if (gen !== renderGeneration) return; // superseded by a newer action
    renderEval(report, container, history);
  } catch (e) {
    if (gen === renderGeneration) {
      container.innerHTML =
        '<div class="loading-state" style="text-align:center; color:red;">Failed to load the eval.</div>';
    }
  }
}

async function fetchHistory(flavor) {
  try {
    return await fetchJson(
      `/api/eval/runs?flavor=${encodeURIComponent(flavor)}`
    );
  } catch (e) {
    return [];
  }
}

// Persist the current report as a run, then refresh just the history (no re-eval / no re-judge).
async function saveRun(report, container) {
  const gen = ++renderGeneration;
  const btn = container.querySelector("#save-run-btn");
  if (btn) {
    btn.textContent = "Saving…";
    btn.disabled = true;
  }
  try {
    await apiFetch("/api/eval/runs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(report),
    });
  } catch (e) {
    // Non-fatal: fall through and re-render with whatever history we can fetch.
  }
  const history = await fetchHistory(report.flavor);
  if (gen !== renderGeneration) return; // a newer action superseded this render
  renderEval(report, container, history);
}

// Delete one saved run, then refresh the history in place.
async function deleteRun(savedAt, report, container) {
  if (!savedAt) return;
  const gen = ++renderGeneration;
  try {
    await apiFetch(`/api/eval/runs?savedAt=${encodeURIComponent(savedAt)}`, {
      method: "DELETE",
    });
  } catch (e) {
    // Non-fatal: fall through and re-render with whatever history we can fetch.
  }
  const history = await fetchHistory(report.flavor);
  if (gen !== renderGeneration) return;
  renderEval(report, container, history);
}
