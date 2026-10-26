import { test, expect } from "@playwright/test";

test("Go → UI batches preserve 10000 events and long jobs can be cancelled and restarted", async ({ page }) => {
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto("/");
  const status = page.getByRole("status");
  await expect(status).toContainText("idle");
  await page.getByRole("button", { name: "10 000 событий", exact: true }).click();
  await expect(status).toContainText("completed; подтверждено Go: 10000; получено UI: 10000", { timeout: 15000 });
  await expect(status).toContainText("порядок: OK");

  await page.getByRole("button", { name: "Долгая задача", exact: true }).click();
  await expect(status).toContainText("Получено:");
  await page.getByRole("button", { name: "Отменить", exact: true }).click();
  await expect(status).toContainText("cancelled");
  await page.getByRole("button", { name: "10 000 событий", exact: true }).click();
  await expect(status).toContainText("completed; подтверждено Go: 10000; получено UI: 10000", { timeout: 15000 });
  await expect(status).toContainText("порядок: OK");
  expect(errors).toEqual([]);
});
