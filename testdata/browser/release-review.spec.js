const fs = require("node:fs");
const path = require("node:path");
const { test, expect } = require("@playwright/test");
const script = fs.readFileSync(path.join(__dirname, "../../internal/httpapi/assets/manage-review.js"), "utf8");

function reviewPage(file) {
  return `<section id="release-review"><input type="search" data-review-filter>
    <input type="checkbox" data-review-wrap><p id="review-load-error" role="alert" hidden></p>
    <ul class="review-tree"><li><details open><summary>manifests</summary><ul>
      <li data-review-file="manifests/first.pp"><a data-review-link href="?file=first">first.pp</a></li>
      <li data-review-file="manifests/second.pp"><a data-review-link href="?file=second">second.pp</a></li>
    </ul></details></li></ul>
    <div class="review-file" tabindex="-1"><h3>${file}</h3><div class="review-code-scroll">class example {}</div></div>
  </section>`;
}

test("release file navigation updates only the viewer and preserves filters", async ({ page }) => {
  let requests = 0;
  await page.route("http://review.test/manage/modules/teamname/demo/review**", async (route) => {
    requests++;
    await route.fulfill({ contentType: "text/html", body: `<h1 id="page-heading">Review</h1>${reviewPage(new URL(route.request().url()).searchParams.get("file") || "first")}` });
  });
  await page.goto("http://review.test/manage/modules/teamname/demo/review");
  await page.addScriptTag({ content: script });
  await page.locator("[data-review-filter]").fill("manifests");
  await page.locator("[data-review-wrap]").check();
  await page.getByRole("link", { name: "second.pp" }).click();
  await expect(page.locator(".review-file h3")).toHaveText("second");
  await expect(page.locator("[data-review-filter]")).toHaveValue("manifests");
  await expect(page.locator("[data-review-wrap]")).toBeChecked();
  await expect(page.locator("#page-heading")).toHaveCount(1);
  expect(requests).toBe(2);
  await page.goBack();
  await expect(page.locator(".review-file h3")).toHaveText("first");
});

test("failed release file fetch keeps the review visible", async ({ page }) => {
  await page.route("http://review.test/manage/modules/teamname/demo/review**", async (route) => {
    if (new URL(route.request().url()).searchParams.has("file")) {
      await route.fulfill({ status: 409, body: "release changed" });
    } else {
      await route.fulfill({ contentType: "text/html", body: reviewPage("first") });
    }
  });
  await page.goto("http://review.test/manage/modules/teamname/demo/review");
  await page.addScriptTag({ content: script });
  await page.getByRole("link", { name: "second.pp" }).click();
  await expect(page.locator("#review-load-error")).toBeVisible();
  await expect(page.locator(".review-file h3")).toHaveText("first");
});
