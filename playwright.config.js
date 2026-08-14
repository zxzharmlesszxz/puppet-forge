const { defineConfig } = require("@playwright/test");

module.exports = defineConfig({
  testDir: "./testdata/browser",
  fullyParallel: true,
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI ? "github" : "list",
  webServer: process.env.BROWSER_EXTERNAL_SERVER ? undefined : {
    command: "go run ./testdata/browser/fixture",
    url: "http://127.0.0.1:18085/healthz",
    reuseExistingServer: !process.env.CI,
    timeout: 120000,
  },
  use: {
    baseURL: process.env.BROWSER_BASE_URL || "http://127.0.0.1:18085",
    browserName: "chromium",
    headless: true,
  },
});
