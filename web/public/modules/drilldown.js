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
import { escapeHtml, fetchJson, wireButton } from "./utils.js";

// Shared "which sessions are behind this tally item" drill-down, used by the Insights and Mine
// views. A caller wraps a clickable summary in `drillRow(category, key, innerHtml)` and then calls
// `wireDrilldown(container, flavor)` once after rendering; clicking expands the contributing
// sessions (fetched from /api/insights/sessions) and clicking a session opens it.

/** Wraps a row's clickable content so it can expand into the sessions behind `category`/`key`. */
export function drillRow(category, key, innerHtml) {
  return `<div class="drill-row" data-drill-category="${escapeHtml(
    category
  )}" data-drill-key="${escapeHtml(key)}">
      <div class="drill-bar" role="button" tabindex="0" style="cursor:pointer;" title="Show the sessions behind this">${innerHtml}</div>
      <div class="drill-sessions" style="display:none;"></div>
    </div>`;
}

/** Wires every drill row in `container` to expand its sessions for the given flavor. */
export function wireDrilldown(container, flavor) {
  if (!container) return;
  container.querySelectorAll(".drill-row").forEach((row) => {
    const bar = row.querySelector(".drill-bar");
    if (bar) wireButton(bar, () => toggleDrill(row, flavor));
  });
}

function renderDrillSessions(result) {
  const sessions = (result && result.sessions) || [];
  if (sessions.length === 0) {
    return `<div class="stat-sub" style="padding:4px 0 10px 24px;">No sessions.</div>`;
  }
  const total = result.totalMatches || sessions.length;
  const more =
    total > sessions.length ? ` (showing ${sessions.length} of ${total})` : "";
  const rows = sessions
    .map(
      (s) =>
        `<div class="drill-session" role="button" tabindex="0" data-id="${escapeHtml(
          s.id
        )}" style="padding:3px 0 3px 24px; font-size:0.82rem; color:var(--accent-purple); cursor:pointer;" title="${escapeHtml(
          s.id
        )}">↳ ${escapeHtml(s.title || s.id)}</div>`
    )
    .join("");
  return `<div style="border-left:2px solid rgba(148,163,184,0.2); margin:2px 0 12px 8px;">${rows}
      <div class="stat-sub" style="padding:2px 0 4px 24px;">${escapeHtml(
        `${total} session${total === 1 ? "" : "s"}${more}`
      )}</div>
    </div>`;
}

// Opens a session by clicking its sidebar item (reusing the app's own selection), else via the hash.
// Matches by dataset rather than a built selector, so any id (even one with quotes) is safe.
function openSession(id) {
  const item = Array.from(document.querySelectorAll(".conv-item")).find(
    (el) => el.dataset.id === id
  );
  if (item) item.click();
  else window.location.hash = id;
}

async function toggleDrill(row, flavor) {
  const sub = row.querySelector(".drill-sessions");
  if (sub.dataset.loaded === "1") {
    sub.style.display = sub.style.display === "none" ? "block" : "none";
    return;
  }
  const category = row.dataset.drillCategory;
  const key = row.dataset.drillKey;
  sub.style.display = "block";
  sub.innerHTML = `<div class="stat-sub" style="padding:4px 0 8px 24px;">Loading sessions…</div>`;
  try {
    const result = await fetchJson(
      `/api/insights/sessions?flavor=${encodeURIComponent(
        flavor
      )}&category=${encodeURIComponent(category)}&key=${encodeURIComponent(
        key
      )}`
    );
    sub.innerHTML = renderDrillSessions(result);
    sub.dataset.loaded = "1";
    sub.querySelectorAll(".drill-session").forEach((el) => {
      wireButton(el, () => openSession(el.dataset.id));
    });
  } catch (e) {
    sub.innerHTML = `<div class="stat-sub" style="padding:4px 0 8px 24px; color:red;">Failed to load sessions.</div>`;
  }
}
