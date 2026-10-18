// Global stubs for browser-only libraries loaded via CDN <script> tags in index.html.
// The frontend modules reference these as ambient globals.

// `marked` (markdown renderer) — stub with an identity-ish parser so rendering logic is testable.
globalThis.marked = {
  parse: (text) => (text == null ? "" : String(text)),
};

// `DOMPurify` (sanitizer, vendored) — passthrough stub; rendering logic is under test here, not
// sanitization itself.
globalThis.DOMPurify = {
  sanitize: (html) => (html == null ? "" : String(html)),
};

// `hljs` (syntax highlighter) — stub so calls are no-ops in tests.
globalThis.hljs = {
  highlightElement: () => {},
};

// jsdom does not implement these layout/scroll APIs; stub them so code that calls them runs.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}
if (!Element.prototype.scrollTo) {
  Element.prototype.scrollTo = () => {};
}

// jsdom does not implement IntersectionObserver (used by the timeline scrubber).
if (!globalThis.IntersectionObserver) {
  globalThis.IntersectionObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
    takeRecords() {
      return [];
    }
  };
}
