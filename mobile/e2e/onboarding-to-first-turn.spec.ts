import { test, expect } from '@playwright/test';

/**
 * End-to-end smoke for the onboarding-to-first-turn path.
 *
 * Drives the embedded SPA against a real orchestrator running with
 * the mock translator + auto-grant approvals. The orchestrator's
 * docs/auth.md documents the fragment-based deep link
 * (#token=<jwt>&sid=<sid>) the qr-jwt CLI emits; this test reuses
 * that contract so the same code path the operator hits on a phone
 * is what CI exercises.
 *
 * Required env vars (set by the CI job that wraps this test):
 *   PLAYWRIGHT_BASE_URL — e.g. http://127.0.0.1:8080
 *   E2E_TOKEN           — a JWT minted by scripts/gen-jwt
 *   E2E_SID             — session id the JWT carries
 */

const token = process.env.E2E_TOKEN;
const sid = process.env.E2E_SID ?? 'sess-e2e';

test.skip(!token, 'E2E_TOKEN not set — skipping mobile E2E');

test('onboarding via fragment deep-link lands on chat and round-trips a turn', async ({ page }) => {
  // 1. Deep-link in. The SPA reads #token=… from the URL fragment,
  //    persists to localStorage, strips the fragment, and navigates
  //    to /chat. Fragment-based delivery keeps the JWT off the wire
  //    in proxy access logs (see docs/auth.md).
  await page.goto(`/#token=${encodeURIComponent(token!)}&sid=${encodeURIComponent(sid)}`);

  // 2. Composer is the canonical "chat is mounted + WS is open" signal.
  //    Wait for it to be visible AND enabled — the orchestrator's
  //    hello envelope flips wsStatus to "open" which un-disables the
  //    submit button.
  const composer = page.getByLabel('composer');
  await expect(composer).toBeVisible();
  const submit = page.getByRole('button', { name: /send/i });
  await expect(submit).toBeEnabled({ timeout: 15_000 });

  // 3. Drive one turn. The mock translator returns a known canned
  //    reply; we assert the user bubble lands first, then the
  //    assistant text appears.
  await composer.fill('hello from e2e');
  await submit.click();

  // User bubble immediately reflects the typed text (recordSentIntent).
  await expect(page.getByText('hello from e2e')).toBeVisible();

  // Assistant reply from the mock translator (see
  // internal/middleware/factory.go:defaultMockTranslator). Match the
  // first few words so a future tweak to the mock text doesn't break
  // this test for the wrong reason.
  await expect(page.getByText(/\(mock\) hello/)).toBeVisible({ timeout: 10_000 });
});
