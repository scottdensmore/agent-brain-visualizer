import { test, expect } from "./test-base.js";

test.describe("session browsing", () => {
  test("lists the seeded sessions", async ({ page }) => {
    await page.goto("/");
    const items = page.locator("#conversations-list .conv-item");
    // Three antigravity-cli fixtures: the two below plus the attached-file session that proves the
    // preview works off-machine (see file-preview.spec.js).
    await expect(items).toHaveCount(3);
    await expect(page.locator(".conv-item", { hasText: "Fix the parser bug" })).toBeVisible();
    await expect(page.locator(".conv-item", { hasText: "Add dark mode" })).toBeVisible();
  });

  test("search filters the list", async ({ page }) => {
    await page.goto("/");
    await expect(page.locator("#conversations-list .conv-item")).toHaveCount(3);
    await page.fill("#conversation-search", "parser");
    await expect(page.locator("#conversations-list .conv-item")).toHaveCount(1);
    await expect(page.locator("#conversations-list .conv-item")).toContainText(
      "Fix the parser bug"
    );
  });

  test("sort toggle reverses the order (newest-first by default)", async ({ page }) => {
    await page.goto("/");
    await expect(page.locator("#conversations-list .conv-item").first()).toContainText(
      "Add dark mode"
    );
    await page.click("#sort-conversations-btn");
    // Oldest first: the attached-file session carries the earliest mtime of the three.
    await expect(page.locator("#conversations-list .conv-item").first()).toContainText(
      "Attached file from another machine"
    );
  });

  test("switching flavor loads that flavor's sessions", async ({ page }) => {
    await page.goto("/");
    await expect(page.locator("#conversations-list .conv-item")).toHaveCount(3);
    await page.selectOption("#flavor-select", "antigravity-ide");
    // The IDE flavor holds its own session plus the hostile XSS fixture (see security.spec.js).
    await expect(page.locator("#conversations-list .conv-item")).toHaveCount(2);
    await expect(
      page.locator(".conv-item", { hasText: "IDE refactor session" })
    ).toBeVisible();
  });

  test("an empty source shows first-run onboarding", async ({ page }) => {
    await page.goto("/");
    // "Antigravity Agent" (antigravity) has no seeded sessions.
    await page.selectOption("#flavor-select", "antigravity");

    // The copy is scoped to the selected source.
    await expect(page.locator("#conversations-list")).toContainText(
      "No Antigravity Agent sessions yet"
    );
    const main = page.locator("#transcript-container");
    await expect(main).toContainText("No Antigravity Agent sessions yet");
    await expect(main).toContainText("agent-ingest --server");
    await expect(main.locator("#onboarding-copy")).toBeVisible();
  });

  test("the conversations API pages with limit/offset and reports the total", async ({
    request,
  }) => {
    // The sidebar's "Load more" relies on this contract: a bounded page plus the full total.
    const page1 = await (
      await request.get("/api/brain/conversations?flavor=antigravity-cli&limit=1&offset=0")
    ).json();
    expect(page1.total).toBe(3);
    expect(page1.items).toHaveLength(1);

    const page2 = await (
      await request.get("/api/brain/conversations?flavor=antigravity-cli&limit=1&offset=1")
    ).json();
    expect(page2.items).toHaveLength(1);
    expect(page2.items[0].id).not.toBe(page1.items[0].id); // consecutive pages don't overlap
  });
});
