const fs = require("node:fs");
const path = require("node:path");
const { test, expect } = require("@playwright/test");

function assetScript(name) {
  return fs.readFileSync(path.join(__dirname, `../../internal/httpapi/assets/${name}`), "utf8");
}

const asyncLists = assetScript("async-lists.js");
const pageSizes = assetScript("page-size-persistence.js");
const clipboard = assetScript("clipboard.js");

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

for (const copied of [true, false]) {
  test(`copy feedback restores its label after repeated clicks (${copied})`, async ({ page }) => {
    await page.setContent('<button id="copy" type="button">Copy token</button>');
    await page.evaluate((script) => { new Function(script)(); }, clipboard);
    await page.evaluate((value) => {
      window['copyTextToClipboard'] = async () => value;
      const button = document.getElementById("copy");
      button.addEventListener("click", () => window['copyTextWithStatus'](button, "token"));
    }, copied);
    const button = page.locator("#copy");
    await button.click();
    await expect(button).toHaveText(copied ? "Copied" : "Failed");
    await button.click();
    await expect(button).toHaveText("Copy token");
  });
}

for (const state of [
  { cookie: "50", local: "10", rendered: "20", expected: "50" },
  { cookie: "invalid", local: "10", rendered: "20", expected: "10" },
  { cookie: "%E0%A4%A", local: "10", rendered: "20", expected: "10" },
  { cookie: "invalid", local: "invalid", rendered: "20", expected: "20" },
]) {
  test(`page-size preferences validate cookie and storage (${state.cookie}, ${state.local})`, async ({ page }) => {
    await page.route("http://forge.test/**", (route) => route.fulfill({
      contentType: "text/html",
      body: '<form data-page-size-key="modules" data-page-size-cookie="modules_per_page"></form>',
    }));
    await page.goto("http://forge.test/");
    await page.evaluate((script) => { new Function(script)(); }, pageSizes);
    const result = await page.evaluate((value) => {
      const form = document.querySelector("form");
      form.dataset.renderedPageSize = value.rendered;
      document.cookie = `modules_per_page=${value.cookie}; Path=/`;
      localStorage.setItem("puppet-forge:page-size:modules", value.local);
      const preferences = window['pageSizePreferences'];
      const preferred = preferences.read(form);
      preferences.write(form, "100");
      preferences.write(form, "invalid");
      return { preferred, stored: preferences.read(form), local: localStorage.getItem("puppet-forge:page-size:modules") };
    }, state);
    expect(result).toEqual({ preferred: state.expected, stored: "100", local: "100" });
  });
}

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
