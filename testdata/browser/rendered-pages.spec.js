const { test, expect } = require("@playwright/test");

async function signIn(page) {
  await page.goto("/manage/login");
  await page.locator("#token").fill("admin-token");
  await Promise.all([
    page.waitForURL("**/manage"),
    page.locator('button[type="submit"]').click(),
  ]);
}

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
