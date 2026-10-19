const { test, expect } = require("@playwright/test");

async function signIn(page) {
  await page.goto("/manage/login");
  await page.locator("#token").fill("admin-token");
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
  const heights = await page.evaluate(() => [
    ".meta .badge", "#team-module-archive", ".replace-option input", ".page-size-control select",
  ].map((selector) => {
    const element = document.querySelector(selector);
    if (!element) throw new Error(`missing control: ${selector}`);
    const style = getComputedStyle(element);
    return selector.endsWith(".badge") ? style.minHeight : style.height;
  }));
  expect(heights).toEqual(["24px", "42px", "18px", "32px"]);
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
