import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  timeout: 30000,
  workers: 1,
  use: { baseURL: "http://127.0.0.1:9245", headless: true },
  webServer: {
    command: "go run -tags server .",
    url: "http://127.0.0.1:9245",
    env: { CGO_ENABLED: "0", WAILS_SERVER_HOST: "127.0.0.1", WAILS_SERVER_PORT: "9245" },
    timeout: 120000,
    reuseExistingServer: false,
  },
});
