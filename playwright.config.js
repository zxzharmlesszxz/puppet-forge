const { defineConfig } = require("@playwright/test");

module.exports = defineConfig({
  testDir: "./testdata/browser",
  fullyParallel: true,
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI ? "github" : "list",
  use: {
    browserName: "chromium",
    headless: true,
  },
});
