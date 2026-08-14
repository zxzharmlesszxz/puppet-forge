const fs = require("node:fs");
const path = require("node:path");
const { test, expect } = require("@playwright/test");

const template = fs.readFileSync(path.join(__dirname, "../../internal/httpapi/templates/async-lists.gohtml"), "utf8");
const controller = template.match(/<script[^>]*>([\s\S]*?)<\/script>/)?.[1];
if (!controller) throw new Error("async-list controller script was not found");

function pageHTML(value = "initial", includeController = false) {
  const scripts = includeController ? "<script>" + controller + "</script>" : "";
  return `<!doctype html><html lang="en"><body>
    <div id="module-list" data-async-list>
      <form action="/" data-async-filter="module-list"><input type="search" name="q" value=""></form>
      <span data-testid="value">${value}</span>
    </div>
    ${scripts}
  </body></html>`;
}

async function submit(page, value) {
  await page.locator('input[name="q"]').fill(value);
  await page.locator("form").evaluate((form) => form.requestSubmit());
}

test("the newest list request owns the DOM and busy state", async ({ page }) => {
  const delays = { first: 500, second: 40, third: 150 };
  await page.route("http://forge.test/**", async (route) => {
    const value = new URL(route.request().url()).searchParams.get("q");
    if (!value) return route.fulfill({ contentType: "text/html", body: pageHTML("initial", true) });
    await new Promise((resolve) => setTimeout(resolve, delays[value]));
    await route.fulfill({ contentType: "text/html", body: pageHTML(value) }).catch(() => {});
  });

  await page.goto("http://forge.test/");
  await submit(page, "first");
  await page.waitForTimeout(10);
  await submit(page, "second");
  await page.waitForTimeout(20);
  await expect(page.locator("#module-list")).toHaveAttribute("aria-busy", "true");
  await submit(page, "third");
  await expect(page.getByTestId("value")).toHaveText("third");
  await expect(page.locator("#module-list")).not.toHaveAttribute("aria-busy", "true");
});

test("history refresh cancels an older list request", async ({ page }) => {
  await page.route("http://forge.test/**", async (route) => {
    const value = new URL(route.request().url()).searchParams.get("q");
    if (!value) return route.fulfill({ contentType: "text/html", body: pageHTML("initial", true) });
    await new Promise((resolve) => setTimeout(resolve, value === "old" ? 40 : 150));
    await route.fulfill({ contentType: "text/html", body: pageHTML(value) }).catch(() => {});
  });

  await page.goto("http://forge.test/");
  await submit(page, "old");
  await page.waitForTimeout(20);
  await page.evaluate(() => {
    window.history.replaceState(null, "", "/?q=new");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  await expect(page.getByTestId("value")).toHaveText("new");
});
