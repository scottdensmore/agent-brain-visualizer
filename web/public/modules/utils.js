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
export const state = {
  activeFilters: {
    userQueries: false,
    toolsCalled: false,
    outcomeErrors: false,
    modelResponses: false,
  },
  spansMultipleDays: false,
  summaryCache: {},
  // Session-timeline bounds, set by renderStats and read by the scrubber and transcript markers.
  timelineStart: 0,
  timelineTotalMs: 0,
};

/** Human-readable labels for the session sources ("flavors") the picker offers. */
export const FLAVOR_LABELS = {
  "antigravity-cli": "Antigravity CLI",
  "antigravity-ide": "Antigravity IDE",
  antigravity: "Antigravity Agent",
  codex: "OpenAI Codex",
  "claude-code": "Claude Code",
};

// Transcript content is untrusted (it is whatever the agent, the user, or a web page the agent read
// happened to produce), so every piece of it that becomes HTML must pass through DOMPurify. The
// default allowed URI schemes are extended with file:// — Antigravity transcripts link local files
// that way and the click is intercepted for the in-app preview modal.
//
// DOMPurify's defaults stop scripts and event handlers, but they still allow several elements that
// markdown has no use for and that let transcript text reshape the page it is rendered into. A
// <style> block is not scoped to the transcript, so it can restyle or hide the application's own
// chrome; a <form> renders inside the trusted origin, which — next to the app's real "this server
// requires an API token" prompt — is enough to make a convincing one that posts the token elsewhere.
// Forbidding them costs nothing: none survives a markdown round-trip legitimately.
const PURIFY_CONFIG = {
  ALLOWED_URI_REGEXP:
    /^(?:(?:https?|mailto|file):|[^a-z]|[a-z+.\-]+(?:[^a-z+.\-:]|$))/i,
  FORBID_TAGS: [
    "style",
    "form",
    "input",
    "button",
    "select",
    "option",
    "textarea",
    "label",
    "fieldset",
    "legend",
  ],
  // `style` goes too: it is what turns an injected anchor into a transparent full-viewport overlay
  // that swallows every click. The app's own citation links use a class instead (timeline.js), so
  // nothing legitimate depends on it. `target` follows, so injected markup cannot open new contexts.
  FORBID_ATTR: ["style", "action", "formaction", "method", "enctype", "target"],
};

/** Renders untrusted markdown to sanitized HTML. The only safe way to innerHTML transcript text. */
export function renderMarkdown(text) {
  if (text == null) return "";
  return DOMPurify.sanitize(marked.parse(String(text)), PURIFY_CONFIG);
}

/**
 * fetch() for the app's own API. When the server guards the API with API_TOKEN, the token the user
 * entered is attached as a bearer; on a 401 the user is prompted once and the request retried, so a
 * freshly guarded server asks for the token instead of silently failing.
 */
export async function apiFetch(url, options = {}) {
  // localStorage can be unavailable (private browsing, test environments); degrade to promptless.
  const storage = {
    get() {
      try {
        return localStorage.getItem("agy-api-token") || "";
      } catch {
        return "";
      }
    },
    set(value) {
      try {
        localStorage.setItem("agy-api-token", value);
      } catch {}
    },
  };
  const doFetch = (token) => {
    const headers = { ...(options.headers || {}) };
    if (token) headers["Authorization"] = `Bearer ${token}`;
    return fetch(url, { ...options, headers });
  };
  let response = await doFetch(storage.get());
  if (response.status === 401) {
    const entered = window.prompt(
      "This server requires an API token (API_TOKEN). Enter it to continue:"
    );
    if (entered && entered.trim()) {
      storage.set(entered.trim());
      response = await doFetch(entered.trim());
    }
  }
  return response;
}

/** apiFetch that throws on a non-OK status and resolves to the parsed JSON body. */
export async function fetchJson(url, options) {
  const res = await apiFetch(url, options);
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}

export function escapeHtml(unsafe) {
  if (!unsafe) return "";
  return String(unsafe)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
}

/** A titled report section with a colored uppercase heading (shared by the report views). */
export function section(title, color, bodyHtml) {
  return `<div style="margin-top:28px;">
      <div style="font-size:0.75rem; font-weight:700; color:${color}; margin-bottom:16px; letter-spacing:0.05em; text-transform:uppercase;">${escapeHtml(
    title
  )}</div>
      ${bodyHtml}
    </div>`;
}

/** Rounds to one decimal place, treating null/undefined as 0. */
export function round1(n) {
  return Math.round((n || 0) * 10) / 10;
}

/**
 * Wires a non-<button> element to act like one: click plus Enter/Space activation. The caller is
 * responsible for making it focusable and announced (tabindex="0", role="button") in the markup.
 */
export function wireButton(el, handler) {
  el.addEventListener("click", handler);
  el.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault(); // Space must not scroll the page
      handler(e);
    }
  });
}

export function syntaxHighlight(json) {
  if (typeof json != "string") {
    json = JSON.stringify(json, undefined, 2);
  }
  json = json
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
  return json.replace(
    /("(\\u[a-zA-Z0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+\-]?\d+)?)/g,
    function (match) {
      var cls = "json-number";
      if (/^"/.test(match)) {
        if (/:$/.test(match)) {
          cls = "json-key";
        } else {
          cls = "json-string";
        }
      } else if (/true|false/.test(match)) {
        cls = "json-boolean";
      } else if (/null/.test(match)) {
        cls = "json-null";
      }
      return '<span class="' + cls + '">' + match + "</span>";
    }
  );
}

export function formatTime(dateStr, withSeconds = false) {
  if (!dateStr) return "";
  const d = new Date(dateStr);
  if (state.spansMultipleDays) {
    if (withSeconds) {
      return (
        d.toLocaleDateString([], { month: "short", day: "numeric" }) +
        ", " +
        d.toLocaleTimeString([], { hour12: false })
      );
    } else {
      return (
        d.toLocaleDateString([], { month: "short", day: "numeric" }) +
        ", " +
        d.toLocaleTimeString([], {
          hour: "2-digit",
          minute: "2-digit",
          hour12: false,
        })
      );
    }
  } else {
    if (withSeconds) {
      return d.toLocaleTimeString([], { hour12: false });
    } else {
      return d.toLocaleTimeString([], {
        hour: "2-digit",
        minute: "2-digit",
        hour12: false,
      });
    }
  }
}

export function updateTranscriptFilter() {
  const cards = document.querySelectorAll(".step-card");
  const { userQueries, toolsCalled, outcomeErrors, modelResponses } =
    state.activeFilters;

  if (!userQueries && !toolsCalled && !outcomeErrors && !modelResponses) {
    cards.forEach((c) => (c.style.display = "block"));
    return;
  }

  cards.forEach((c) => {
    const isUser = c.dataset.isUser === "true";
    const isTool = c.dataset.isTool === "true";
    const isError = c.dataset.isError === "true";
    const isModel = c.dataset.isModel === "true";

    let show = false;
    if (userQueries && isUser) show = true;
    if (toolsCalled && isTool) show = true;
    if (outcomeErrors && isError) show = true;
    if (modelResponses && isModel) show = true;

    c.style.display = show ? "block" : "none";
  });

  const wrappers = document.querySelectorAll(".sequence-wrapper");
  wrappers.forEach((wrapper) => {
    const hasVisibleCard = Array.from(
      wrapper.querySelectorAll(".step-card")
    ).some((c) => c.style.display === "block");
    wrapper.style.display = hasVisibleCard ? "block" : "none";
  });
}
