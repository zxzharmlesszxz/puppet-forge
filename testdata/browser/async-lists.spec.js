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

function moduleCardHTML(version, loaded = true) {
  return `<details id="item-teamname-module" class="module-details" data-module-card-url="/manage/modules/teamname/module/card"${loaded ? ' data-module-card-loaded="true"' : ""}>
    <summary>teamname/module</summary>
    <div class="module-notice-slot" aria-live="polite"></div>
    <div class="release-list"><span data-testid="release">${version}</span></div>
    <form method="post" action="/manage/modules/teamname/module/versions/${version}/delete" data-async-mutation data-refresh-target="item-teamname-module">
      <button type="submit">Delete</button>
    </form>
  </details>`;
}

test("module releases load lazily and mutations refresh only their card", async ({ page }) => {
  let version = "1.0.0";
  await page.route("http://forge.test/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (request.method() === "POST") {
      version = "none";
      return route.fulfill({
        status: 204,
        headers: {
          "X-Puppet-Forge-Refresh": "item-teamname-module",
          "X-Puppet-Forge-Message": "version deleted",
        },
      });
    }
    if (url.pathname.endsWith("/card")) {
      return route.fulfill({ contentType: "text/html", body: moduleCardHTML(version) });
    }
    return route.fulfill({
      contentType: "text/html",
      body: `<!doctype html><html lang="en"><body>${moduleCardHTML("Open to load releases", false)}<script>${controller}</script></body></html>`,
    });
  });

  await page.goto("http://forge.test/manage/modules");
  await page.locator("#item-teamname-module summary").click();
  await expect(page.getByTestId("release")).toHaveText("1.0.0");
  await page.getByRole("button", { name: "Delete" }).click();
  await expect(page.getByTestId("release")).toHaveText("none");
  await expect(page.locator(".module-notice-slot .async-mutation-notice")).toHaveText("version deleted");
  await expect(page.locator("#item-teamname-module > :first-child")).toHaveJSProperty("tagName", "SUMMARY");
  await expect(page).toHaveURL("http://forge.test/manage/modules");
});

test("failed lazy card requests fall back to the owning page", async ({ page }) => {
  await page.route("http://forge.test/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith("/card")) return route.fulfill({ status: 500, body: "failed" });
    return route.fulfill({
      contentType: "text/html",
      body: `<!doctype html><html lang="en"><body>${moduleCardHTML("Open to load releases", false)}<script>${controller}</script></body></html>`,
    });
  });

  await page.goto("http://forge.test/manage/modules");
  await page.locator("#item-teamname-module summary").click();
  await expect(page).toHaveURL("http://forge.test/manage/modules");
  await expect(page.locator("#item-teamname-module")).toBeVisible();
});

function mutationPageHTML(listValue, includeController = false) {
  const scripts = includeController ? "<script>" + controller + "</script>" : "";
  return `<!doctype html><html lang="en"><body>
    <section class="panel">
      <form id="publish-form" method="post" action="/manage/modules" enctype="multipart/form-data" data-async-mutation data-refresh-target="manage-module-list">
        <input name="space" value="teamname">
        <input name="file" type="file">
        <button type="submit">Publish</button>
      </form>
      <form id="upstream-form" method="post" action="/manage/upstream" data-async-mutation data-refresh-target="manage-module-list">
        <input name="module" value="puppetlabs/stdlib">
        <button type="submit">Add Upstream Module</button>
      </form>
    </section>
    <section id="manage-module-list" class="panel" data-async-list>
      <span data-testid="module-list-value">${listValue}</span>
    </section>
    ${scripts}
  </body></html>`;
}

test("publish and upstream mutations refresh only the module list", async ({ page }) => {
  let listValue = "initial";
  const posts = [];
  await page.route("http://forge.test/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (request.method() === "POST") {
      posts.push(url.pathname);
      listValue = url.pathname === "/manage/modules" ? "published" : "upstream added";
      return route.fulfill({
        status: 204,
        headers: {
          "X-Puppet-Forge-Refresh": "manage-module-list",
          "X-Puppet-Forge-Message": listValue,
        },
      });
    }
    return route.fulfill({ contentType: "text/html", body: mutationPageHTML(listValue, url.pathname === "/manage/modules") });
  });

  await page.goto("http://forge.test/manage/modules");
  await page.locator('#publish-form input[type="file"]').setInputFiles({
    name: "teamname-module-1.0.0.tar.gz",
    mimeType: "application/gzip",
    buffer: Buffer.from("archive"),
  });
  await page.getByRole("button", { name: "Publish" }).click();
  await expect(page.getByTestId("module-list-value")).toHaveText("published");
  await expect(page.locator('#publish-form input[type="file"]')).toHaveJSProperty("value", "");

  await page.getByRole("button", { name: "Add Upstream Module" }).click();
  await expect(page.getByTestId("module-list-value")).toHaveText("upstream added");
  expect(posts).toEqual(["/manage/modules", "/manage/upstream"]);
  await expect(page).toHaveURL("http://forge.test/manage/modules");
});

test("module deletion removes its card and suppresses duplicate submits", async ({ page }) => {
  let postCount = 0;
  let deleted = false;
  await page.route("http://forge.test/**", async (route) => {
    if (route.request().method() === "POST") {
      postCount++;
      await new Promise((resolve) => setTimeout(resolve, 100));
      deleted = true;
      return route.fulfill({
        status: 204,
        headers: {
          "X-Puppet-Forge-Remove": "item-teamname-module",
          "X-Puppet-Forge-Message": "module deleted",
        },
      });
    }
    return route.fulfill({
      contentType: "text/html",
      body: `<!doctype html><html lang="en"><body>
        <section id="manage-module-list" class="panel" data-async-list>
          ${deleted ? "" : `<details id="item-teamname-module" class="module-details">
            <summary>teamname/module</summary>
            <form id="delete-module" method="post" action="/manage/modules/teamname/module/delete" data-async-mutation data-remove-target="item-teamname-module">
              <button type="submit">Delete module</button>
            </form>
          </details>`}
        </section>
        <script>${controller}</script>
      </body></html>`,
    });
  });

  await page.goto("http://forge.test/manage/modules");
  await page.locator("#delete-module").evaluate((element) => {
    if (!(element instanceof HTMLFormElement)) return;
    element.requestSubmit();
    element.requestSubmit();
  });
  await expect(page.locator("#item-teamname-module")).toHaveCount(0);
  expect(postCount).toBe(1);
});

test("mutation validation and server errors remain local", async ({ page }) => {
  let status = 422;
  await page.route("http://forge.test/**", async (route) => {
    if (route.request().method() === "POST") {
      const message = status === 422 ? "invalid module archive" : "upstream unavailable";
      return route.fulfill({ status, contentType: "application/json", body: JSON.stringify({ error: message }) });
    }
    return route.fulfill({ contentType: "text/html", body: mutationPageHTML("unchanged", true) });
  });

  await page.goto("http://forge.test/manage/modules");
  await page.getByRole("button", { name: "Add Upstream Module" }).click();
  await expect(page.locator(".async-mutation-notice.error")).toHaveText("invalid module archive");
  await expect(page.getByRole("button", { name: "Add Upstream Module" })).toBeEnabled();

  status = 500;
  await page.getByRole("button", { name: "Add Upstream Module" }).click();
  await expect(page.locator(".async-mutation-notice.error")).toHaveText("upstream unavailable");
  await expect(page.getByTestId("module-list-value")).toHaveText("unchanged");
  await expect(page).toHaveURL("http://forge.test/manage/modules");
});
