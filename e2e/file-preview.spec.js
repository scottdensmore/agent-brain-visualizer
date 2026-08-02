import { test, expect } from "./test-base.js";

test.describe("file preview modal", () => {
  test("clicking a file link opens, renders, and closes the modal", async ({ page }) => {
    await page.goto("/");
    await page.click('.conv-item:has-text("Fix the parser bug")');

    // The file link lives in a collapsed step body — expand the card that contains it.
    const linkCard = page.locator("#transcript-container .step-card", {
      has: page.locator('a[href^="file://"]'),
    });
    await linkCard.locator(".step-header").first().click();

    const link = linkCard.locator('a[href^="file://"]').first();
    await expect(link).toBeVisible();
    await link.click();

    const modal = page.locator("#file-modal");
    await expect(modal).toBeVisible();
    await expect(page.locator("#file-modal-title")).toContainText("config.txt");
    await expect(page.locator("#file-modal-content")).toContainText("parser.mode = strict");

    await page.keyboard.press("Escape");
    await expect(modal).toBeHidden();
  });

  test("previews a file that only exists on the machine that ran the agent", async ({ page }) => {
    // The bug this closes: the preview used to read the server's own disk, so a session viewed from
    // any machine but the one that produced it showed "File not found". Nothing writes
    // /srv/elsewhere/Remote.java here — it can only come from the copy pushed with the session.
    await page.goto("/");
    await page.click('.conv-item:has-text("Attached file from another machine")');

    const linkCard = page.locator("#transcript-container .step-card", {
      has: page.locator('a[href^="file://"]'),
    });
    await linkCard.locator(".step-header").first().click();
    await linkCard.locator('a[href^="file://"]').first().click();

    await expect(page.locator("#file-modal")).toBeVisible();
    await expect(page.locator("#file-modal-title")).toContainText("Remote.java");
    await expect(page.locator("#file-modal-content")).toContainText("class Remote");
  });

  test("the preview endpoint serves an attached file only for the session that carried it", async ({
    request,
  }) => {
    const path = encodeURIComponent("/srv/elsewhere/Remote.java");

    const carried = await request.get(
      `/api/brain/file?path=${path}&id=sess-0003-attached&flavor=antigravity-cli`
    );
    expect(carried.status()).toBe(200);
    expect(await carried.text()).toContain("class Remote");

    // A different session never attached it, and the path is outside the local sandbox, so there is
    // nothing to fall back to — the attachment is scoped to its own session, not a global file map.
    const other = await request.get(
      `/api/brain/file?path=${path}&id=sess-0001-parser&flavor=antigravity-cli`
    );
    expect(other.status()).not.toBe(200);
  });
});
