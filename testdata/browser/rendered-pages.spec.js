const { test, expect } = require("@playwright/test");

async function signIn(page, token = "admin-token") {
  await page.goto("/manage/login");
  await page.locator("#token").fill(token);
  await Promise.all([
    page.waitForURL("**/manage"),
    page.locator('button[type="submit"]').click(),
  ]);
}

test("layouts load embedded styles and deferred scripts", async ({ page }) => {
  const failedAssets = [];
  const errors = [];
  page.on("response", (response) => {
    if (new URL(response.url()).pathname.startsWith("/assets/") && !response.ok()) {
      failedAssets.push(response.url());
    }
  });
  page.on("pageerror", (error) => errors.push(error.message));

  await signIn(page);
  for (const path of ["/", "/modules/teamname/module-00", "/manage", "/manage/modules", "/manage/teams", "/manage/teams/teamname/modules"]) {
    await page.goto(path);
    await expect(page.locator("main")).toHaveCount(1);
    await expect(page.locator("style, script:not([src])")).toHaveCount(0);
    const sources = await page.locator('link[rel="stylesheet"], script[src]').evaluateAll((elements) => elements.map((element) => element.getAttribute("href") || element.getAttribute("src")));
    expect(new Set(sources).size).toBe(sources.length);
    await expect(page.locator('link[href="/assets/base.css"]')).toHaveCount(1);
    await expect(page.locator("h1")).toBeVisible();
  }
  expect(failedAssets).toEqual([]);
  expect(errors).toEqual([]);
});

test("shared section controller restores open module cards", async ({ page }) => {
  await signIn(page);
  await page.goto("/manage/modules");
  const module = page.locator("#item-teamname-module-00");
  await module.locator(":scope > summary").click();
  await expect(module).toHaveAttribute("open", "");
  await expect.poll(() => page.evaluate(() => JSON.parse(localStorage.getItem("puppet-forge:manage:open-sections") || "[]"))).toContain("module:teamname/module-00");
  await page.reload();
  await expect(module).toHaveAttribute("open", "");
});

test("shared styles preserve panel, badge, and control dimensions", async ({ page }) => {
  await signIn(page);
  await page.goto("/manage/modules");
  expect(await page.locator(".content-panel").first().evaluate((element) => getComputedStyle(element).padding)).toBe("22px");

  await page.goto("/manage/teams");
  expect(await page.locator(".team-row .badge").first().evaluate((element) => getComputedStyle(element).minHeight)).toBe("26px");

  await page.goto("/manage/teams/teamname/modules");
  await expect(page.locator("#team-module-archive")).toHaveCount(0);
  const heights = await page.evaluate(() => [
    ".meta .badge", ".page-size-control select",
  ].map((selector) => {
    const element = document.querySelector(selector);
    if (!element) throw new Error(`missing control: ${selector}`);
    const style = getComputedStyle(element);
    return selector.endsWith(".badge") ? style.minHeight : style.height;
  }));
  expect(heights).toEqual(["24px", "32px"]);
});

test("publisher upload controls use shared dimensions without replacement privileges", async ({ page }) => {
  await signIn(page, "browser-publish-token");
  await page.goto("/manage/modules");
  const archive = page.locator("#module-archive-input");
  await expect(archive).toHaveCount(1);
  await archive.locator("xpath=ancestor::details").evaluate((element) => { element.open = true; });
  await expect(archive).toBeVisible();
  expect(await archive.evaluate((element) => getComputedStyle(element).height)).toBe("42px");
  await expect(page.locator(".replace-option input")).toHaveCount(0);
});

test("public and manage catalogs share the same search component", async ({ page }) => {
  await page.goto("/");
  const publicFilter = page.locator("#module-filter-form.list-filter");
  await expect(publicFilter).toBeVisible();
  const publicStyle = await publicFilter.locator('input[type="search"]').evaluate((element) => ({
    height: getComputedStyle(element).height,
    radius: getComputedStyle(element).borderRadius,
  }));

  await signIn(page);
  await page.goto("/manage/modules");
  const manageFilter = page.locator("#manage-module-filter.list-filter");
  await expect(manageFilter).toBeVisible();
  const manageStyle = await manageFilter.locator('input[type="search"]').evaluate((element) => ({
    height: getComputedStyle(element).height,
    radius: getComputedStyle(element).borderRadius,
  }));
  expect(manageStyle).toEqual(publicStyle);
});

test("public search and pagination update only the module list", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator("#module-list .row")).toHaveCount(20);
  const navigation = page.locator(".public-navigation");
  const navigationHTML = await navigation.innerHTML();
  let documentRequests = 0;
  page.on("request", (request) => {
    if (request.resourceType() === "document") documentRequests++;
  });

  await page.locator("#module-filter").fill("module-24");
  await expect(page.locator("#module-list .row")).toHaveCount(1);
  await expect(page.locator("#module-list .name")).toContainText("teamname/module-24");
  await expect(page.locator("#module-filter")).toBeFocused();

  await page.locator("#module-filter").pressSequentially("-missing");
  await expect(page.locator("#module-list .row")).toHaveCount(0);
  await expect(page.locator("#module-filter")).toHaveValue("module-24-missing");
  await expect(page.locator("#module-filter")).toBeFocused();

  await page.locator("#module-list .list-filter-clear").click();
  await expect(page.locator("#module-list .row")).toHaveCount(20);
  await expect(page.locator("#module-filter")).toHaveValue("");
  expect(await navigation.innerHTML()).toBe(navigationHTML);
  expect(documentRequests).toBe(0);
});

test("overview lists paginate independently and page size survives reload", async ({ page }) => {
  await signIn(page);
  const modules = page.locator("#modules");
  const teams = page.locator("#teams");
  const initialTeams = await teams.innerHTML();

  await modules.locator('select[name="modules_per_page"]').selectOption("10");
  await expect(modules.locator(".item-list li")).toHaveCount(10);
  expect(await teams.innerHTML()).toBe(initialTeams);

  await page.reload();
  await expect(modules.locator('select[name="modules_per_page"]')).toHaveValue("10");
  await expect(modules.locator(".item-list li")).toHaveCount(10);
});

test("module title stays inside its card on mobile", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/modules/teamname/module-00");

  const title = await page.locator(".hero h1").boundingBox();
  const hero = await page.locator(".hero").boundingBox();
  const copyLink = await page.locator("#copy-version-link").boundingBox();
  expect(title).not.toBeNull();
  expect(hero).not.toBeNull();
  expect(copyLink).not.toBeNull();
  expect(title.x + title.width).toBeLessThanOrEqual(hero.x + hero.width);
  expect(copyLink.x + copyLink.width).toBeLessThanOrEqual(390);
});

test("README tables render and scroll within mobile content", async ({ page }) => {
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 844 });
    await page.goto("/modules/teamname/module-00");
    const table = page.locator("#readme table");
    await expect(table).toBeVisible();
    await expect(table.locator("th")).toHaveText(["APL key", "Type", "Default"]);
    await expect(table.locator("tbody tr")).toHaveCount(2);
    expect(await table.evaluate((element) => getComputedStyle(element).overflowX)).toBe("auto");
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    if (width === 390) {
      expect(await table.evaluate((element) => element.scrollWidth)).toBeGreaterThan(await table.evaluate((element) => element.clientWidth));
    }
  }
});

test("README allows trusted badges and captions unavailable images", async ({ page, baseURL }) => {
  const badge = { contentType: "image/svg+xml", body: '<svg xmlns="http://www.w3.org/2000/svg" width="80" height="20"><rect width="80" height="20" fill="green"/></svg>' };
  await page.route("https://images.example.test/**", (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/redirect.svg" || path === "/blocked-redirect.svg") {
      const target = path === "/redirect.svg"
        ? new URL("/browser-fixture/badge.svg", baseURL).href
        : "https://blocked.example.test/badge.svg";
      return route.fulfill({ status: 302, headers: { Location: target }, body: "" });
    }
    return route.fulfill(path === "/badge.svg" ? badge : { status: 404, contentType: "text/plain", body: "missing" });
  });
  await page.addInitScript(() => {
    window.blockedImages = [];
    document.addEventListener("securitypolicyviolation", (event) => {
      if (event.effectiveDirective === "img-src") window.blockedImages.push(event.blockedURI);
    });
  });
  await page.goto("/modules/upstream/module-34");
  const readme = page.locator("#readme");
  const allowed = readme.locator('img[alt="Allowed badge"]');
  await expect(allowed).toBeVisible();
  await expect.poll(() => allowed.evaluate((image) => image.naturalWidth)).toBe(80);
  const redirected = readme.locator('img[alt="Redirected badge"]');
  await expect(redirected).toBeVisible();
  await expect.poll(() => redirected.evaluate((image) => image.naturalWidth)).toBe(40);
  await expect(readme).toContainText("Missing badge");
  await expect(readme).toContainText("Image unavailable");
  await expect(readme).toContainText("Blocked badge");
  await expect(readme).toContainText("Blocked redirect");
  await expect(readme.locator("img")).toHaveCount(2);
  await expect.poll(() => page.evaluate(() => window.blockedImages)).toContain("https://blocked.example.test/badge.svg");
});
