'use strict';

// Exercise the shipped assets and production CSP with fake loopback APIs only.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const { chromium } = require('playwright');

const root = path.resolve(__dirname, '..');
const plugin = 'cliproxyapi-commandcode-provider';
const resource = '/v0/resource/plugins/' + plugin;
const management = '/v0/management/plugins/' + plugin;
const secret = 'fake-browser-management-key';
const authIndex = '0123456789abcdef';
const source = fs.readFileSync(path.resolve(root, '../provider/management.go'), 'utf8');
const policies = [...source.matchAll(/headers\.Set\("Content-Security-Policy", "([^"]+)"\)/g)];
assert.equal(policies.length, 1, 'must read the single production CSP');

async function main() {
  const out = process.argv[2];
  assert.ok(out, 'output directory required');
  fs.mkdirSync(out, { recursive: true });
  const calls = [];
  const results = [];
  const controls = { mode: 'cli', fail: false, confirm: true };
  const server = http.createServer(async (req, res) => {
    try {
      const url = new URL(req.url, 'http://127.0.0.1');
      if (url.pathname === '/embed') {
        res.setHeader('Content-Type', 'text/html; charset=utf-8');
        res.end('<!doctype html><html lang="zh-CN"><head><link rel="stylesheet" href="/fixture.css"></head>' +
          '<body><header>CPA 本地验证宿主</header><iframe title="CommandCode" src="' + resource + '/ui"></iframe></body></html>');
        return;
      }
      if (url.pathname === '/fixture.css') {
        res.setHeader('Content-Type', 'text/css');
        res.end(fs.readFileSync(path.join(root, 'ui.css'), 'utf8') +
          '\nbody{padding:0}header{height:48px;padding:12px}iframe{display:block;width:100%;height:4200px;border:0}');
        return;
      }
      if (url.pathname.startsWith(resource + '/')) {
        const name = url.pathname.slice(resource.length + 1);
        const file = name === 'ui' ? 'ui.html' : name;
        if (!['ui.html', 'ui.js', 'ui.css'].includes(file)) { res.writeHead(404).end(); return; }
        res.setHeader('Content-Security-Policy', policies[0][1]);
        res.setHeader('Content-Type', file.endsWith('.html') ? 'text/html; charset=utf-8' :
          file.endsWith('.css') ? 'text/css' : 'text/javascript');
        res.end(fs.readFileSync(path.join(root, file)));
        return;
      }
      if (!url.pathname.startsWith(management + '/')) { res.writeHead(404).end(); return; }
      assert.equal(req.headers.authorization, 'Bearer ' + secret);
      assert.equal(req.url.includes(secret), false);
      let body = '';
      for await (const chunk of req) body += chunk;
      calls.push({ method: req.method, path: url.pathname, body });
      res.setHeader('Content-Type', 'application/json');
      if (url.pathname.endsWith('/keys') && req.method === 'PATCH') {
        if (controls.fail) { res.writeHead(500).end('{}'); return; }
        const patch = JSON.parse(body);
        assert.deepEqual(Object.keys(patch).sort(), ['auth_index', 'mode']);
        assert.equal(patch.auth_index, authIndex);
        if (controls.confirm) controls.mode = patch.mode;
        res.end(JSON.stringify(patch));
      } else if (url.pathname.endsWith('/keys')) {
        res.end(JSON.stringify({ files: [{
          name: 'fake-key.json', label: 'Fixture <b>key</b>', auth_index: authIndex,
          mode: controls.mode, mode_source: 'auth', status: 'active',
          success_count: 3, failure_count: 0
        }] }));
      } else if (url.pathname.endsWith('/settings')) {
        res.end(JSON.stringify({
          mode: 'cli', default_mode: 'cli', mode_scope: 'auth', cli_version: '1.79.2',
          catalog: { count: 2, source: 'fixture', stale: false }, 'manual-models': []
        }));
      } else { res.end('{}'); }
    } catch (error) {
      res.writeHead(500).end('{}');
      console.error(error);
    }
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const origin = 'http://127.0.0.1:' + server.address().port;
  const browser = await chromium.launch({
    executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true
  });
  try {
    const context = await browser.newContext();
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    page.on('console', (message) => {
      if (/violat.*Content Security Policy/i.test(message.text())) errors.push(message.text());
    });
    page.on('request', (request) => {
      assert.equal(new URL(request.url()).origin, origin, 'no external requests');
      assert.equal(request.url().includes(secret), false, 'no credentials in URLs');
    });
    await page.goto(origin + '/embed');
    const frame = page.frameLocator('iframe');
    await frame.locator('#mgmt-key').fill(secret);
    await frame.locator('#refresh-btn').click();
    await frame.locator('#manual-model-status').filter({ hasText: '已加载' }).waitFor();
    assert.equal(await frame.locator('#files-body b').count(), 0, 'labels remain text');

    const importTrigger = frame.locator('#import-mode-trigger');
    await frame.locator('label[for="import-mode"]').click();
    assert.equal(await frame.locator('#import-mode-trigger').evaluate((el) => el === document.activeElement), true);
    const before = calls.length;
    await importTrigger.press('ArrowDown');
    await importTrigger.press('End');
    assert.equal(await frame.locator('#import-mode').inputValue(), 'cli');
    await importTrigger.press('Enter');
    assert.equal(await frame.locator('#import-mode').inputValue(), 'provider');
    assert.equal(calls.length, before, 'selection does not write or probe');
    await importTrigger.click();
    await importTrigger.press('Escape');
    assert.equal(await frame.locator('.cpa-select-menu').count(), 0);
    await importTrigger.click();
    await page.mouse.click(8, 8);
    await frame.locator('.cpa-select-menu').waitFor({ state: 'hidden' });
    results.push('label focus, keyboard commit, Escape and parent outside dismissal');

    const modeTrigger = () => frame.locator('.cell-mode .cpa-select-trigger');
    const chooseMode = async (label) => {
      await modeTrigger().click();
      await frame.locator('.cpa-select-menu [role="option"]').filter({ hasText: label }).click();
    };
    const writesBefore = calls.filter((item) => item.method === 'PATCH').length;
    await chooseMode('Provider');
    assert.equal(calls.filter((item) => item.method === 'PATCH').length, writesBefore);
    await frame.locator('[data-action="save-mode"]').click();
    await frame.locator('#status').filter({ hasText: '确认生效' }).waitFor();
    const patchIndex = calls.findIndex((item) => item.method === 'PATCH');
    assert.equal(calls.slice(patchIndex + 1).some((item) => item.method === 'GET' && item.path.endsWith('/keys')), true);
    assert.match(await modeTrigger().textContent(), /Provider/);
    await chooseMode('CLI');
    controls.fail = true;
    await frame.locator('[data-action="save-mode"]').click();
    await frame.locator('#status').filter({ hasText: '500' }).waitFor();
    assert.equal(await frame.locator("select[data-role='mode-select']").inputValue(), 'cli');
    assert.match(await modeTrigger().textContent(), /CLI/);
    controls.fail = false;
    controls.confirm = false;
    await frame.locator('[data-action="save-mode"]').click();
    await frame.locator('#status').filter({ hasText: '未能确认' }).waitFor();
    assert.equal(await frame.locator("select[data-role='mode-select']").inputValue(), 'cli');
    controls.confirm = true;
    results.push('mode PATCH requires GET confirmation; failures preserve the selected draft');

    for (let i = 0; i < 3; i++) {
      await modeTrigger().click();
      await frame.locator('#reload-btn').click();
      await page.waitForFunction(() => {
        const doc = document.querySelector('iframe').contentDocument;
        return doc && !doc.getElementById('import-mode-trigger').disabled;
      });
      await frame.locator('#import-mode-trigger').evaluate((el) => {
        if (el.disabled) throw new Error('select remained disabled after reload');
        const native = document.querySelectorAll('select');
        const triggers = document.querySelectorAll('.cpa-select-trigger');
        if (native.length !== triggers.length) throw new Error('detached triggers leaked');
        if (new Set(Array.from(triggers, (node) => node.id)).size !== triggers.length) throw new Error('duplicate trigger IDs');
      });
      assert.equal(await frame.locator('.cpa-select-menu').count(), 0);
    }
    results.push('repeated list replacement cleans up menus, triggers and IDs');

    for (const theme of ['light', 'white', 'dark']) {
      await page.evaluate((value) => {
        if (value === 'light') document.documentElement.removeAttribute('data-theme');
        else document.documentElement.setAttribute('data-theme', value);
      }, theme);
      await page.waitForFunction((value) => {
        const doc = document.querySelector('iframe').contentDocument;
        return (doc.documentElement.getAttribute('data-theme') || 'light') === value;
      }, theme);
      for (const width of [1280, 768, 390]) {
        await page.setViewportSize({ width, height: 900 });
        await frame.locator('#import-mode-trigger').evaluate((el) => {
          if (document.documentElement.scrollWidth > window.innerWidth) throw new Error('page overflow');
          if (Math.round(el.getBoundingClientRect().height) !== 34) throw new Error('trigger height');
          const small = document.querySelector('.cell-mode .cpa-select-trigger');
          if (Math.round(small.getBoundingClientRect().height) !== 28) throw new Error('row trigger height');
          if (localStorage.length || sessionStorage.length || document.cookie) throw new Error('credentials persisted');
        });
        await importTrigger.click();
        const box = await frame.locator('.cpa-select-menu').evaluate((el) => {
          const rect = el.getBoundingClientRect();
          const frameRect = window.frameElement.getBoundingClientRect();
          return { bottom: rect.bottom, limit: window.parent.innerHeight - frameRect.top };
        });
        assert.ok(box.bottom <= box.limit + 1, 'menu must stay in parent-visible viewport');
        await page.screenshot({ path: path.join(out, theme + '-' + width + '-dropdown.png'), animations: 'disabled' });
        await importTrigger.press('Escape');
        await frame.locator('#settings-heading').scrollIntoViewIfNeeded();
        await page.screenshot({ path: path.join(out, theme + '-' + width + '-models.png'), animations: 'disabled' });
        results.push(theme + '/' + width + ' theme, compact controls, viewport and storage');
      }
    }
    const touch = await browser.newContext({ viewport: { width: 390, height: 900 }, hasTouch: true });
    const touchPage = await touch.newPage();
    await touchPage.goto(origin + resource + '/ui');
    assert.ok(await touchPage.locator('#import-mode-trigger').evaluate((el) => el.getBoundingClientRect().height >= 44));
    await touch.close();
    assert.deepEqual(errors, []);
    results.push('coarse pointer keeps accessible touch targets; no browser/CSP errors');
    await context.close();
  } finally {
    await browser.close();
    await new Promise((resolve) => server.close(resolve));
  }
  fs.writeFileSync(path.join(out, 'results.json'), JSON.stringify(results, null, 2));
  console.log(results.length + '/' + results.length + ' passed');
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
