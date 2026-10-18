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
import { state, escapeHtml, formatTime, apiFetch } from "./utils.js";

const SUPPORTED_HIGHLIGHT_EXTS = [
  "js",
  "json",
  "java",
  "html",
  "css",
  "md",
  "sh",
  "bash",
  "yaml",
  "yml",
  "xml",
  "sql",
  "kt",
  "kts",
  "gradle",
  "properties",
  "py",
  "go",
  "rs",
  "cpp",
  "c",
  "ts",
  "jsx",
  "tsx",
];

// Map a file path to a highlight.js language class, or "" when the extension is unsupported.
export function languageClassFor(path) {
  const ext = path.split(".").pop().toLowerCase();
  return SUPPORTED_HIGHLIGHT_EXTS.includes(ext) ? `language-${ext}` : "";
}

// Extract the filesystem path from a file:// href (strips the scheme, any line-number hash, and
// percent-encoding).
export function pathFromFileLink(href) {
  let path = href.replace("file://", "");
  path = path.split("#")[0];
  return decodeURIComponent(path);
}

// The element that had focus before the modal opened, so closing can hand focus back to it.
let focusBeforeModal = null;

// Fetch a file from the backend and render it in the preview modal. On failure, alerts the user.
export async function openFilePreview(path) {
  const modal = document.getElementById("file-modal");
  const modalTitle = document.getElementById("file-modal-title");
  const modalContent = document.getElementById("file-modal-content");

  try {
    // Name the session so the server can serve the copy the ingest client attached to it, which is
    // what makes the preview work when the viewer is not the machine that ran the agent. Without
    // these the server falls back to reading its own disk, which only works locally.
    const params = new URLSearchParams({ path });
    const sessionId = document.getElementById("current-session-id")?.dataset.id;
    const flavor = document.getElementById("flavor-select")?.value;
    if (sessionId) params.set("id", sessionId);
    if (flavor) params.set("flavor", flavor);

    const res = await apiFetch(`/api/brain/file?${params}`);
    if (!res.ok) {
      if (res.status === 404) throw new Error("File not found");
      throw new Error("Failed to load file");
    }
    const content = await res.text();

    modalTitle.innerText = path;

    const langClass = languageClassFor(path);
    modalContent.className = langClass;
    modalContent.innerHTML = escapeHtml(content);

    if (langClass && window.hljs) {
      delete modalContent.dataset.highlighted;
      hljs.highlightElement(modalContent);
    }

    // Opening steals focus for the dialog: remember where the keyboard user was, then land them on
    // the close button. Closing (below) reverses this.
    focusBeforeModal = document.activeElement;
    modal.classList.remove("hidden");
    document.getElementById("close-modal-btn")?.focus();
  } catch (err) {
    alert(err.message + ":\n" + path);
  }
}

export function closeFileModal() {
  const modal = document.getElementById("file-modal");
  if (modal) modal.classList.add("hidden");
  if (focusBeforeModal && typeof focusBeforeModal.focus === "function") {
    focusBeforeModal.focus();
  }
  focusBeforeModal = null;
}

// Keeps Tab (and Shift+Tab) cycling inside the open modal instead of escaping into the page.
function trapModalTab(e, modal) {
  const focusables = modal.querySelectorAll(
    'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
  );
  if (focusables.length === 0) return;
  const first = focusables[0];
  const last = focusables[focusables.length - 1];
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first.focus();
  }
}

export function initUI() {
  const modal = document.getElementById("file-modal");
  const closeBtn = document.getElementById("close-modal-btn");

  if (closeBtn) {
    closeBtn.addEventListener("click", () => {
      closeFileModal();
    });
  }

  if (modal) {
    modal.addEventListener("click", (e) => {
      if (e.target === modal) {
        closeFileModal();
      }
    });
  }

  // Intercept file:// links using event delegation
  document.addEventListener("click", async (e) => {
    const link = e.target.closest("a");
    if (link && link.href && link.href.startsWith("file://")) {
      e.preventDefault();
      await openFilePreview(pathFromFileLink(link.href));
    }
  });

  document.addEventListener("keydown", (e) => {
    if (!modal || modal.classList.contains("hidden")) return;
    if (e.key === "Escape") {
      closeFileModal();
    } else if (e.key === "Tab") {
      trapModalTab(e, modal);
    }
  });

  initScrubber();
}

function initScrubber() {
  const track = document.getElementById("timeline-track");
  if (!track) return;
  const thumb = document.getElementById("timeline-thumb");
  const label = document.getElementById("timeline-label");
  const scrollContainer = document.getElementById("transcript-container");

  if (!scrollContainer) return;

  let isDragging = false;
  let isTicking = false;

  // Use IntersectionObserver to update label efficiently without getBoundingClientRect
  let visibleCards = new Set();

  const updateTimelineRect = () => {
    if (state.timelineTotalMs > 0 && visibleCards.size > 0) {
      let minTs = Infinity;
      let maxTs = -Infinity;
      visibleCards.forEach((el) => {
        if (el.dataset.timestampStart && el.dataset.timestampEnd) {
          const start = parseInt(el.dataset.timestampStart, 10);
          const end = parseInt(el.dataset.timestampEnd, 10);
          if (start < minTs) minTs = start;
          if (end > maxTs) maxTs = end;
        } else if (el.dataset.timestamp) {
          const tsStart = parseInt(el.dataset.timestamp, 10);
          const tsEnd = el.dataset.timestampEnd
            ? parseInt(el.dataset.timestampEnd, 10)
            : tsStart;
          if (tsStart < minTs) minTs = tsStart;
          if (tsEnd > maxTs) maxTs = tsEnd;
        }
      });

      if (minTs !== Infinity && maxTs !== -Infinity) {
        // Add a slight buffer to the width so it doesn't look like a 0px line for a single card
        const startPct =
          ((minTs - state.timelineStart) / state.timelineTotalMs) * 100;
        let widthPct = ((maxTs - minTs) / state.timelineTotalMs) * 100;

        const indicator = document.getElementById("session-timeline-indicator");
        if (indicator) {
          const clampedStart = Math.max(0, Math.min(100, startPct));
          indicator.style.left = `calc(${clampedStart}% - ${
            (clampedStart / 100) * 2
          }px)`;
          indicator.style.width = widthPct + "%";
          indicator.style.minWidth = "2px";
          indicator.style.display = "block";
        }
      }
    }
  };

  const labelObserver = new IntersectionObserver(
    (entries) => {
      entries.forEach((entry) => {
        if (entry.isIntersecting) {
          const card = entry.target;
          if (card.dataset.time) {
            label.innerText = card.dataset.time;
          }
        }
      });
    },
    {
      root: scrollContainer,
      rootMargin: "-10% 0px -80% 0px",
      threshold: 0,
    }
  );

  const rectObserver = new IntersectionObserver(
    (entries) => {
      let changedVisibility = false;
      entries.forEach((entry) => {
        if (entry.isIntersecting) {
          visibleCards.add(entry.target);
          changedVisibility = true;
        } else {
          if (visibleCards.has(entry.target)) {
            visibleCards.delete(entry.target);
            changedVisibility = true;
          }
        }
      });
      if (changedVisibility) {
        updateTimelineRect();
      }
    },
    {
      root: scrollContainer,
      rootMargin: "0px",
      threshold: 0,
    }
  );

  // Watch for new step-cards being added to the DOM and observe them
  const mutationObserver = new MutationObserver((mutations) => {
    mutations.forEach((mutation) => {
      mutation.addedNodes.forEach((node) => {
        if (node.nodeType === 1) {
          if (
            node.classList.contains("step-card") ||
            node.id === "timeline-bottom-marker"
          ) {
            if (node.classList.contains("step-card"))
              labelObserver.observe(node);
            rectObserver.observe(node);
          }
          node
            .querySelectorAll(".step-card, #timeline-bottom-marker")
            .forEach((card) => {
              if (card.classList.contains("step-card"))
                labelObserver.observe(card);
              rectObserver.observe(card);
            });
        }
      });
    });
  });
  mutationObserver.observe(scrollContainer, { childList: true, subtree: true });

  // Only update thumb position in the scroll event, greatly reducing layout thrashing
  function updateScrubberThumb() {
    if (isTicking) return;
    isTicking = true;

    window.requestAnimationFrame(() => {
      const scrollableHeight =
        scrollContainer.scrollHeight - scrollContainer.clientHeight;
      if (scrollableHeight <= 0) {
        thumb.style.top = "0px";
        isTicking = false;
        return;
      }

      const scrollPerc = scrollContainer.scrollTop / scrollableHeight;
      const trackHeight = track.clientHeight;
      const thumbHeight = thumb.clientHeight;

      const maxTop = trackHeight - thumbHeight;
      thumb.style.top = scrollPerc * maxTop + "px";

      isTicking = false;
    });
  }

  function scrollToTrackPosition(clientY) {
    const rect = track.getBoundingClientRect();
    let y = clientY - rect.top;
    y = Math.max(0, Math.min(y, rect.height));

    const perc = y / rect.height;
    const scrollableHeight =
      scrollContainer.scrollHeight - scrollContainer.clientHeight;
    scrollContainer.scrollTo(0, perc * scrollableHeight);
  }

  track.addEventListener("mousedown", (e) => {
    isDragging = true;
    track.classList.add("active");
    e.preventDefault(); // Prevent text selection
    scrollToTrackPosition(e.clientY);
  });

  window.addEventListener("mousemove", (e) => {
    if (!isDragging) return;
    e.preventDefault();
    scrollToTrackPosition(e.clientY);
  });

  window.addEventListener("mouseup", () => {
    isDragging = false;
    track.classList.remove("active");
  });

  scrollContainer.addEventListener("scroll", updateScrubberThumb);
  setTimeout(updateScrubberThumb, 500);
  window.addEventListener("transcriptLoaded", updateScrubberThumb);
}
