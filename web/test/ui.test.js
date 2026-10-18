import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  languageClassFor,
  pathFromFileLink,
  openFilePreview,
  closeFileModal,
} from "../public/modules/ui.js";

beforeEach(() => {
  document.body.innerHTML = `
    <button id="outside-btn"></button>
    <div id="file-modal" class="hidden">
      <div id="file-modal-title"></div>
      <pre id="file-modal-content"></pre>
      <button id="close-modal-btn"></button>
    </div>
  `;
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("languageClassFor", () => {
  it("maps supported extensions to a highlight.js class", () => {
    expect(languageClassFor("/a/b/Main.java")).toBe("language-java");
    expect(languageClassFor("script.JS")).toBe("language-js");
  });

  it("returns empty string for unsupported extensions", () => {
    expect(languageClassFor("/a/b/photo.png")).toBe("");
  });
});

describe("pathFromFileLink", () => {
  it("strips the scheme, the line-number hash and percent-encoding", () => {
    expect(pathFromFileLink("file:///home/u/My%20File.txt#L10-L20")).toBe(
      "/home/u/My File.txt"
    );
  });
});

describe("openFilePreview", () => {
  it("loads file content into the modal with a language class and highlights it", async () => {
    global.fetch = vi.fn(() =>
      Promise.resolve({ ok: true, text: () => Promise.resolve("class A {}") })
    );
    const hl = vi.spyOn(globalThis.hljs, "highlightElement");

    await openFilePreview("/home/u/.gemini/A.java");

    const modal = document.getElementById("file-modal");
    expect(modal.classList.contains("hidden")).toBe(false);
    expect(document.getElementById("file-modal-title").innerText).toBe(
      "/home/u/.gemini/A.java"
    );
    const content = document.getElementById("file-modal-content");
    expect(content.className).toBe("language-java");
    expect(content.innerHTML).toBe("class A {}");
    expect(hl).toHaveBeenCalledTimes(1);
  });

  it("escapes file content to prevent HTML injection", async () => {
    global.fetch = vi.fn(() =>
      Promise.resolve({
        ok: true,
        text: () => Promise.resolve("<script>alert(1)</script>"),
      })
    );

    await openFilePreview("/home/u/.gemini/notes.txt");

    const content = document.getElementById("file-modal-content");
    expect(content.innerHTML).toBe("&lt;script&gt;alert(1)&lt;/script&gt;");
    // Unsupported extension -> no language class, no highlight.
    expect(content.className).toBe("");
  });

  it("alerts and leaves the modal hidden on a 404", async () => {
    global.fetch = vi.fn(() => Promise.resolve({ ok: false, status: 404 }));
    const alertSpy = vi.fn();
    vi.stubGlobal("alert", alertSpy);

    await openFilePreview("/home/u/.gemini/missing.txt");

    expect(alertSpy).toHaveBeenCalledTimes(1);
    expect(alertSpy.mock.calls[0][0]).toContain("File not found");
    expect(document.getElementById("file-modal").classList.contains("hidden")).toBe(true);
  });
});

describe("closeFileModal", () => {
  it("hides the modal", () => {
    const modal = document.getElementById("file-modal");
    modal.classList.remove("hidden");
    closeFileModal();
    expect(modal.classList.contains("hidden")).toBe(true);
  });
});

describe("modal focus management", () => {
  it("moves focus to the close button on open and restores it on close", async () => {
    global.fetch = vi.fn(() =>
      Promise.resolve({ ok: true, text: () => Promise.resolve("hello") })
    );
    const outside = document.getElementById("outside-btn");
    outside.focus();
    expect(document.activeElement).toBe(outside);

    await openFilePreview("/home/u/notes.txt");
    expect(document.activeElement).toBe(
      document.getElementById("close-modal-btn")
    );

    closeFileModal();
    expect(document.activeElement).toBe(outside);
  });
});
