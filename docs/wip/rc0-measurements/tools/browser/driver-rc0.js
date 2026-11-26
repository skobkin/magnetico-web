// RC0 automated acceptance: the published image's web UI in a real headless Chrome, over
// HTTPS through the disposable Traefik, with the production CSP. Node 22 (global
// WebSocket), Chrome DevTools Protocol on 127.0.0.1:9333. Derived from
// docs/wip/09B-measurements/browser/driver.js.
//
//   env: IDS_FILE (JSON: {"tree": id, "dirs": id}), OUT (screenshots and report; they can
//   show real torrent names: keep them out of the repository), CDP (default 127.0.0.1:9333)
// Signs in as the documented synthetic administrator test/test. Prints no name or token.
const fs = require("fs");
const BASE = "https://magnet.rc0.test:18443";
const CDP = process.env.CDP || "127.0.0.1:9333";
const OUT = process.env.OUT || "/out";
const IDS = JSON.parse(fs.readFileSync(process.env.IDS_FILE, "utf8"));
const PROD_CSP = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'";
// the API documentation pages' own policy (internal/web/apidocs/apidocs.go docsCSP)
const DOCS_CSP = "default-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'";
const report = { checks: [], console: [], exceptions: [], csp: [], requests: new Set() };
const check = (name, ok, detail) => { report.checks.push({ name, ok: !!ok, detail }); console.log((ok ? "ok    " : "FAIL  ") + name + (detail ? " — " + detail : "")); };
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function newTab() {
  const t = await (await fetch(`http://${CDP}/json/new?about:blank`, { method: "PUT" })).json();
  const ws = new WebSocket(t.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0; const pending = new Map(); const handlers = [];
  ws.onmessage = ev => { const m = JSON.parse(ev.data); if (m.id && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.rej(new Error(JSON.stringify(m.error))) : p.res(m.result); } else handlers.forEach(h => h(m)); };
  const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; pending.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
  return { send, on: h => handlers.push(h), close: () => ws.close() };
}

(async () => {
  const tab = await newTab();
  const { send } = tab;
  tab.on(m => {
    if (m.method === "Runtime.consoleAPICalled") report.console.push({ type: m.params.type, text: m.params.args.map(a => a.value ?? a.description).join(" ").slice(0, 200) });
    if (m.method === "Runtime.exceptionThrown") report.exceptions.push((m.params.exceptionDetails.text + " " + (m.params.exceptionDetails.exception?.description || "")).slice(0, 300));
    if (m.method === "Log.entryAdded" && /Content Security Policy|Refused to/i.test(m.params.entry.text)) report.csp.push(m.params.entry.text.slice(0, 300));
    if (m.method === "Network.requestWillBeSent") report.requests.add(new URL(m.params.request.url).origin);
    if (m.method === "Network.responseReceived" && m.params.type === "Document") report.lastDoc = { status: m.params.response.status, csp: m.params.response.headers["Content-Security-Policy"] || m.params.response.headers["content-security-policy"] };
  });
  await send("Page.enable"); await send("Runtime.enable"); await send("Log.enable"); await send("Network.enable");
  const nav = async (url, wait = 1500) => { await send("Page.navigate", { url }); await sleep(wait); };
  const ev = async expr => { const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true }); if (r.exceptionDetails) throw new Error(r.exceptionDetails.text); return r.result.value; };
  const shot = async name => { const r = await send("Page.captureScreenshot", { format: "png" }); fs.writeFileSync(`${OUT}/${name}.png`, Buffer.from(r.data, "base64")); };
  const viewport = (w, h, mobile = false) => send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile });
  const overflow = () => ev(`document.documentElement.scrollWidth - document.documentElement.clientWidth`);
  await send("Network.clearBrowserCookies");
  await viewport(1280, 900);

  await nav(`${BASE}/torrents`);
  check("anonymous /torrents is sent to sign-in", (await ev("location.pathname")) === "/login", await ev("location.pathname"));
  check("sign-in page carries exactly the production CSP", report.lastDoc && report.lastDoc.csp === PROD_CSP);
  await ev(`document.querySelector('input[name=username]').value='test'; document.querySelector('input[name=password]').value='test'; document.querySelector('form').submit(); 1`);
  await sleep(2000);
  await nav(`${BASE}/torrents?q=1080p`, 2500);
  const rows = await ev(`document.querySelectorAll('a[href^="/torrents/"]').length`);
  check("signed in; search for a common word lists results", (await ev("location.pathname")) === "/torrents" && rows > 10, `${rows} result links`);
  const nextLink = await ev(`(() => { const a = document.querySelector('a[rel=next]'); return a ? a.getAttribute('href') : null; })()`);
  if (nextLink) { await nav(BASE + nextLink, 2500); check("keyset pagination: the next page renders", (await ev(`document.querySelectorAll('a[href^="/torrents/"]').length`)) > 10); }
  else check("keyset pagination link present", false, "no next link found");
  await nav(`${BASE}/torrents?q=zzqxjnothing`, 1500);
  check("a search without hits renders its empty state", (await ev(`document.querySelectorAll('a[href^="/torrents/"]').length`)) === 0 && !/Internal Server Error/.test(await ev("document.body.innerText")));

  // tree mode
  await nav(`${BASE}/torrents/${IDS.tree}`, 3000);
  const tm = await ev(`({ mode: document.querySelector('#file-browser').dataset.mode, toggles: document.querySelectorAll('#file-browser .folder-toggle').length, filter: !!document.querySelector('#file-browser [data-filter]') })`);
  check("tree mode: the file tree renders with folder toggles and a filter", tm.mode === "tree" && tm.toggles > 0 && tm.filter, JSON.stringify(tm));
  const exp = await ev(`(async () => { const b = document.querySelector('#file-browser .folder-toggle'); const before = b.getAttribute('aria-expanded'); b.click(); await new Promise(r => setTimeout(r, 300)); const b2 = document.querySelector('#file-browser .folder-toggle[data-id="' + b.dataset.id + '"]'); return [before, b2 && b2.getAttribute('aria-expanded')]; })()`);
  check("tree mode: a folder toggle changes aria-expanded", exp[0] !== exp[1], JSON.stringify(exp));
  const filt = await ev(`(async () => { const i = document.querySelector('#file-browser [data-filter]'); const n0 = document.querySelectorAll('#file-browser .tree-row').length; i.value = 'zzqxj'; i.dispatchEvent(new Event('input', { bubbles: true })); await new Promise(r => setTimeout(r, 600)); const n1 = document.querySelectorAll('#file-browser .tree-row').length; return [n0, n1]; })()`);
  check("tree mode: the local filter narrows the list", filt[1] < filt[0], JSON.stringify(filt));
  await shot("torrent-tree");

  // directory mode
  await nav(`${BASE}/torrents/${IDS.dirs}`, 3000);
  const dm = await ev(`({ mode: document.querySelector('#file-browser').dataset.mode, next: !!document.querySelector('#file-browser [data-page=next]'), crumbs: !!document.querySelector('#file-browser [data-crumbs]'), text: document.querySelector('#file-browser').innerText.length })`);
  check("directory mode: the first page renders with breadcrumbs and paging", dm.mode === "dirs" && dm.crumbs && dm.text > 100, JSON.stringify(dm));
  const pg = await ev(`(async () => { const n = document.querySelector('#file-browser [data-page=next]'); if (!n || n.disabled) return 'no next'; const t0 = document.querySelector('#file-browser').innerText; n.click(); await new Promise(r => setTimeout(r, 1500)); return document.querySelector('#file-browser').innerText !== t0 ? 'changed' : 'same'; })()`);
  check("directory mode: Next loads another page or the root fits one page", pg === "changed" || pg === "no next", pg);
  await shot("torrent-dirs");

  // Swagger UI under the production CSP
  await nav(`${BASE}/api/v2`, 4000);
  const sw = await ev(`({ ops: document.querySelectorAll('.opblock').length, err: document.querySelectorAll('.errors-wrapper, .loading-container .error').length })`);
  check("Swagger UI renders the v2 operations", sw.ops >= 5 && sw.err === 0, JSON.stringify(sw));
  check("Swagger page carries exactly its documented policy (docsCSP)", report.lastDoc && report.lastDoc.csp === DOCS_CSP, report.lastDoc && report.lastDoc.csp);

  // the administrator overview
  await nav(`${BASE}/admin`, 4000);
  const cards = await ev(`Array.from(document.querySelectorAll('[data-chart]')).map(n => ({ chart: n.getAttribute('data-chart'), plots: n.querySelectorAll('.uplot').length, status: n.querySelector('.chart-status').hidden ? '' : n.querySelector('.chart-status').textContent }))`);
  check("admin overview: five chart groups", cards.length === 5, cards.map(c => c.chart).join(","));
  check("admin overview: every chart draws or states why it has no data", cards.every(c => c.plots > 0 || c.status !== ""), JSON.stringify(cards));
  await shot("admin-overview");

  // dark theme and a 360 px layout
  await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: "dark" }] });
  await nav(`${BASE}/torrents?q=1080p`, 2000);
  const dark = await ev(`getComputedStyle(document.body).backgroundColor`);
  check("dark theme paints a dark background", /rgb\((\d+), (\d+), (\d+)\)/.test(dark) && dark.match(/\d+/g).slice(0, 3).every(v => +v < 80), dark);
  await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: "light" }] });
  await viewport(360, 800, true);
  for (const p of ["/torrents?q=1080p", `/torrents/${IDS.tree}`, `/torrents/${IDS.dirs}`, "/account?tab=profile", "/account?tab=rss", "/admin", "/api/v2"]) {
    await nav(BASE + p, 2000);
    const ov = await overflow();
    check(`360 px: ${p.replace(/\d{3,}/, "<id>")} has no horizontal page scroll`, ov <= 0, `overflow ${ov} px`);
  }

  report.requests = [...report.requests];
  check("no request to another origin", report.requests.every(o => o === BASE || o === "null" || o.startsWith("data:")), report.requests.join(","));
  check("no CSP violation", report.csp.length === 0, report.csp.join(" | "));
  check("no console error or exception", report.console.filter(c => c.type === "error").length === 0 && report.exceptions.length === 0, JSON.stringify({ console: report.console.filter(c => c.type === "error").slice(0, 5), exceptions: report.exceptions.slice(0, 5) }));
  fs.writeFileSync(`${OUT}/report.json`, JSON.stringify(report, null, 2));
  const failed = report.checks.filter(c => !c.ok).length;
  console.log(failed ? `${failed} check(s) FAILED` : "all checks passed");
  tab.close(); process.exit(failed ? 1 : 0);
})().catch(e => { console.error("driver error:", e.message); process.exit(2); });
