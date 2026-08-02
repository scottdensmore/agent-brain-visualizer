import { test, expect } from "./test-base.js";

// Transcript content is untrusted: agents paste in whatever the web, a tool, or a user produced.
// The "Hostile transcript" fixture carries script tags, onerror handlers, and an attacker-controlled
// tool name; none of it may execute or reach the DOM un-neutralized. Guards the DOMPurify/escapeHtml
// pipeline in timeline.js and analysis.js against regressions.
test.describe("transcript XSS hardening", () => {
  test("hostile transcript content renders inert", async ({ page }) => {
    await page.goto("/");
    await page.selectOption("#flavor-select", "antigravity-ide");

    await page.locator('.conv-item:has-text("Hostile transcript")').click();
    const tc = page.locator("#transcript-container");
    await expect(tc.locator(".step-card")).toHaveCount(2);

    // Expand every card so all body content (markdown, thinking, tool calls) is rendered.
    for (const header of await tc.locator(".step-card .step-header").all()) {
      await header.click();
    }
    await expect(tc.locator(".tool-name")).toBeVisible();

    // No payload executed — neither from content, thinking, tool name, nor the cached summary.
    expect(await page.evaluate(() => window.__xss)).toBeUndefined();

    // No live <script> elements made it into the transcript or summary.
    expect(await tc.locator("script").count()).toBe(0);
    expect(await page.locator("#ai-summary-container script").count()).toBe(0);

    // The attacker-controlled tool name is shown as text, not parsed as markup.
    await expect(tc.locator(".tool-name")).toContainText("onerror");
    expect(await tc.locator(".tool-name img").count()).toBe(0);

    // Elements DOMPurify allows by default but markdown has no use for, and which let transcript
    // text reshape the page around it rather than merely sit in it.
    expect(await tc.locator("style").count()).toBe(0);
    expect(await tc.locator("form, input, button, select, textarea").count()).toBe(0);

    // The style attribute is stripped too, so an injected anchor cannot become a full-viewport
    // overlay that swallows every click.
    const overlay = tc.locator("#xss-overlay");
    if ((await overlay.count()) > 0) {
      expect(await overlay.getAttribute("style")).toBeNull();
    }

    // The page's own chrome survived: a <style>body{display:none}</style> that had been applied
    // would have hidden this.
    await expect(page.locator("#conversations-list")).toBeVisible();
  });

  test("the app's own citation links still render and open safely", async ({ page }) => {
    // The citation superscripts are built into the content that DOMPurify sanitizes, so they must
    // not rely on the attributes the sanitizer now strips.
    await page.goto("/");
    await page.locator('.conv-item:has-text("Fix the parser bug")').click();
    const tc = page.locator("#transcript-container");
    await expect(tc.locator(".step-card").first()).toBeVisible();
    for (const header of await tc.locator(".step-card .step-header").all()) {
      await header.click();
    }
    // Whatever links the transcript produces must carry no inline style and no target.
    for (const link of await tc.locator(".markdown-body a").all()) {
      expect(await link.getAttribute("style")).toBeNull();
      expect(await link.getAttribute("target")).toBeNull();
    }
  });
});
