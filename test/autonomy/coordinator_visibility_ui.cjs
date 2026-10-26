// CDP driver for the coordinator-visibility acceptance run. Attaches to the packaged scratch app only.
// usage: node coordinator_visibility_ui.cjs <playwright-dir> <cdp-port> <action> '<json args>'
// Emits one JSON object; never prints message text (counts, labels, booleans and snippet-match counts only).
const [playwrightDir, cdpPort, action, rawArgs] = process.argv.slice(2);
const args = JSON.parse(rawArgs || '{}');
const { chromium } = require(playwrightDir);

const norm = (s) => s.replace(/\s+/g, ' ');

async function main() {
  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${cdpPort}`);
  const page = browser.contexts()[0].pages()[0];
  const out = { action };
  const body = async () => norm(await page.locator('body').innerText());
  const internalCount = async () => page.locator('[data-audience="internal"]').count();
  const matches = async (snips) => {
    const b = await body();
    return (snips || []).filter((t) => b.includes(norm(t).slice(0, 60))).length;
  };
  try {
    if (action === 'open-main-from-home') {
      await page.goto('app://renderer/index.html#/');
      await page.waitForLoadState('domcontentloaded');
      await page.waitForTimeout(2500);
      const button = page.getByRole('button', { name: /Open main conversation and board/i });
      out.homeButtonPresent = (await button.count()) > 0;
      if (out.homeButtonPresent) {
        await button.first().click();
        await page.waitForFunction((id) => location.hash.includes(id), args.sessionId, { timeout: 60000 }).catch(() => {});
      }
      out.url = page.url();
      out.reachedCoordinatorSession = page.url().includes(args.sessionId);
    } else if (action === 'open-main-from-sidebar') {
      await page.goto('app://renderer/index.html#/');
      await page.waitForLoadState('domcontentloaded');
      await page.waitForTimeout(2500);
      const row = page.getByRole('button', { name: /^Main coordinator$/i }).or(page.getByText(/^Main coordinator$/i));
      out.sidebarRowPresent = (await row.count()) > 0;
      if (out.sidebarRowPresent) {
        await row.first().click();
        await page.waitForFunction((id) => location.hash.includes(id), args.sessionId, { timeout: 60000 }).catch(() => {});
      }
      out.url = page.url();
      out.reachedCoordinatorSession = page.url().includes(args.sessionId);
      // The coordinator project is not listed as an ordinary project in the sidebar.
      out.coordinatorProjectListedAsProject = (await body()).includes(args.coordinatorProjectName || '\u0000');
    } else if (action === 'default-view') {
      await page.waitForTimeout(3000);
      out.composerPresent = (await page.locator('[data-testid="chat-agent-input"]').count()) > 0;
      out.toggleBarPresent = (await page.locator('[data-testid="coordinator-audience-bar"]').count()) > 0;
      out.toggleLabelPresent = (await body()).includes('Show internal activity');
      out.internalBlocks = await internalCount();
      out.visibleSnippetMatches = await matches(args.snippets);
      out.visibleSnippetCandidates = (args.snippets || []).length;
    } else if (action === 'send') {
      await page.evaluate((m) => { window.__cvMarker = m; }, args.marker);
      const box = page.locator('[data-testid="chat-agent-input"]');
      await box.first().click();
      await page.keyboard.type(args.text, { delay: 5 });
      await page.keyboard.press('Enter');
      out.sent = true;
    } else if (action === 'check-live') {
      out.markerStillPresent = (await page.evaluate(() => window.__cvMarker)) === args.marker; // no reload happened
      out.replySnippetMatches = await matches(args.snippets);
      out.replySnippetCandidates = (args.snippets || []).length;
      out.internalBlocks = await internalCount();
    } else if (action === 'toggle') {
      const sw = page.locator('[data-testid="coordinator-audience-bar"] [role="switch"]');
      out.switchPresent = (await sw.count()) > 0;
      out.internalBefore = await internalCount();
      await sw.first().click();
      await page.waitForTimeout(2500);
      out.internalAfterOn = await internalCount();
      out.internalLabelsPresent = (await page.locator('[data-audience="internal"] >> text=Internal').count()) > 0;
      out.switchChecked = (await sw.first().getAttribute('aria-checked')) === 'true';
      await sw.first().click();
      await page.waitForTimeout(1500);
      out.internalAfterOff = await internalCount();
    } else if (action === 'notifications') {
      const bell = page.getByRole('button', { name: /notification/i });
      out.bellPresent = (await bell.count()) > 0;
      if (out.bellPresent) {
        await bell.first().click();
        await page.waitForTimeout(2500);
        const b = await body();
        out.titlePresent = b.includes(args.title);
        out.purposeLabelPresent = b.includes(args.purposeLabel);
        await page.keyboard.press('Escape');
      }
    }
    out.ok = true;
  } catch (e) {
    out.ok = false;
    out.error = String(e.message || e).slice(0, 200);
  } finally {
    await browser.close();
  }
  console.log(JSON.stringify(out));
}
main();
