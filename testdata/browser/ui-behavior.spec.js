const fs = require("node:fs");
const path = require("node:path");
const { test, expect } = require("@playwright/test");

function templateScript(name) {
  const source = fs.readFileSync(path.join(__dirname, `../../internal/httpapi/templates/${name}`), "utf8");
  const script = source.match(/<script[^>]*>([\s\S]*?)<\/script>/)?.[1];
  if (!script) throw new Error(`${name} does not contain a script`);
  return script;
}

const asyncLists = templateScript("async-lists.gohtml");
const pageSizes = templateScript("page-size-persistence.gohtml");
const clipboard = fs.readFileSync(
  path.join(__dirname, "../../internal/httpapi/templates/clipboard.gohtml"),
  "utf8",
).replace(/^{{define[^\n]*}}|{{end}}$/gm, "");

test("copy helper uses the Clipboard API", async ({ page }) => {
  await page.setContent('<!doctype html><html lang="en"><body></body></html>');
  await page.evaluate((script) => {
    Object.defineProperty(window, "isSecureContext", { value: true });
    const clipboardMock = {};
    Object.defineProperty(clipboardMock, "writeText", {
      value: async (value) => { window.copiedValue = value; },
    });
    Object.defineProperty(navigator, "clipboard", {
      value: clipboardMock,
    });
    new Function(script)();
  }, clipboard);

  await expect.poll(() => page.evaluate(() => window["copyTextToClipboard"]("module link"))).toBe(true);
  await expect.poll(() => page.evaluate(() => window.copiedValue)).toBe("module link");
});

test("async page size persists and refreshes only its list", async ({ page }) => {
  const fixture = (value, scripts = "") => `<!doctype html><html lang="en"><body>
    <div id="module-list" data-async-list>
      <form action="/" data-page-size-key="modules" data-page-size-cookie="modules_per_page"
            data-page-size-param="per_page" data-page-param="page" data-rendered-page-size="${value}"
            data-async-filter="module-list">
        <label>Per page <select name="per_page"><option>10</option><option>20</option><option>50</option></select></label>
      </form>
      <span data-testid="page-size">${value}</span>
    </div>${scripts}</body></html>`;

  await page.route("http://forge.test/**", async (route) => {
    const initial = route.request().resourceType() === "document";
    const cookie = route.request().headers().cookie || "";
    const value = cookie.includes("modules_per_page=50") ? "50" : "20";
    const scripts = initial ? "<script>" + pageSizes + "</script><script>" + asyncLists + "</script>" : "";
    await route.fulfill({
      contentType: "text/html",
      body: fixture(value, scripts),
    });
  });

  await page.goto("http://forge.test/");
  await page.locator("select").selectOption("50");
  await expect(page.getByTestId("page-size")).toHaveText("50");
  await expect.poll(() => page.evaluate(() => localStorage.getItem("puppet-forge:page-size:modules"))).toBe("50");
  await expect.poll(() => page.context().cookies()).toContainEqual(expect.objectContaining({ name: "modules_per_page", value: "50" }));
});
