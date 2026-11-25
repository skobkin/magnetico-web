// CDP driver (Node 22, global WebSocket): the Admin UI in a real headless Chrome, production CSP.
const fs = require("fs");
const BASE = "http://127.0.0.1:18081";
const OUT = "/work/shots";
const SETUP = fs.readFileSync("/work/user-add.out", "utf8").match(/http:\/\/127\.0\.0\.1:18081\/password\/setup\/\S+/)[0];
const PASSWORD = "browser-check-pass-1";
const report = { checks: [], console: [], exceptions: [], logs: [], requests: new Set(), csp: [] };
const check = (name, ok, detail) => { report.checks.push({ name, ok: !!ok, detail }); console.log((ok ? "ok    " : "FAIL  ") + name + (detail ? " — " + detail : "")); };
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function newTab() {
  const t = await (await fetch("http://127.0.0.1:9222/json/new?about:blank", { method: "PUT" })).json();
  const ws = new WebSocket(t.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0; const pending = new Map(); const handlers = [];
  ws.onmessage = ev => { const m = JSON.parse(ev.data); if (m.id && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.rej(new Error(JSON.stringify(m.error))) : p.res(m.result); } else handlers.forEach(h => h(m)); };
  const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; pending.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
  return { send, on: h => handlers.push(h), close: () => ws.close(), targetId: t.id };
}

(async () => {
  const tab = await newTab();
  const { send } = tab;
  tab.on(m => {
    if (m.method === "Runtime.consoleAPICalled") report.console.push({ type: m.params.type, text: m.params.args.map(a => a.value ?? a.description).join(" ") });
    if (m.method === "Runtime.exceptionThrown") report.exceptions.push(m.params.exceptionDetails.text + " " + (m.params.exceptionDetails.exception?.description || ""));
    if (m.method === "Log.entryAdded") { report.logs.push({ source: m.params.entry.source, level: m.params.entry.level, text: m.params.entry.text, url: m.params.entry.url }); if (/Content Security Policy|Refused to/i.test(m.params.entry.text)) report.csp.push(m.params.entry.text); }
    if (m.method === "Network.requestWillBeSent") report.requests.add(new URL(m.params.request.url).origin);
    if (m.method === "Network.responseReceived" && m.params.type === "Document") report.lastDoc = { url: m.params.response.url, status: m.params.response.status, csp: m.params.response.headers["Content-Security-Policy"] || m.params.response.headers["content-security-policy"] };
  });
  await send("Page.enable"); await send("Runtime.enable"); await send("Log.enable"); await send("Network.enable");
  const nav = async url => { await send("Page.navigate", { url }); await sleep(1500); };
  const ev = async expr => { const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true }); if (r.exceptionDetails) throw new Error(r.exceptionDetails.text + " " + JSON.stringify(r.exceptionDetails.exception?.description)); return r.result.value; };
  const shot = async name => { const r = await send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true }); fs.writeFileSync(`${OUT}/${name}.png`, Buffer.from(r.data, "base64")); };
  const viewport = (w, h, mobile = false) => send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile });
  await send("Network.clearBrowserCookies");
  await viewport(1280, 900);

  // anonymous access
  await nav(`${BASE}/admin`);
  check("anonymous /admin is sent to sign-in", (await ev("location.pathname")) === "/login", await ev("location.pathname + location.search"));

  // password setup (the real one-time link), then sign in
  await nav(SETUP);
  if (await ev(`!!document.querySelector('input[name=password2]')`)) {
    await ev(`document.querySelector('input[name=password]').value=${JSON.stringify(PASSWORD)}; document.querySelector('input[name=password2]').value=${JSON.stringify(PASSWORD)}; document.querySelector('form').submit(); 1`);
    await sleep(1500);
    console.log("setup form submitted: " + (await ev("location.pathname + ' ' + document.title")));
  } else { console.log("setup link already used: signing in with the password set earlier"); }
  await nav(`${BASE}/login`);
  await ev(`document.querySelector('input[name=username]').value='browser-admin'; document.querySelector('input[name=password]').value=${JSON.stringify(PASSWORD)}; document.querySelector('form').submit(); 1`);
  await sleep(1500);
  await nav(`${BASE}/admin`);
  check("administrator reaches /admin", (await ev("location.pathname")) === "/admin", await ev("location.pathname"));
  await sleep(2500);
  report.docCsp = report.lastDoc && report.lastDoc.csp;
  const PROD_CSP = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"; // internal/web/server/middleware.go, unchanged
  check("the page carries exactly the production CSP", report.docCsp === PROD_CSP, report.docCsp);

  // charts
  const cards = await ev(`Array.from(document.querySelectorAll('[data-chart]')).map(n => ({ chart: n.getAttribute('data-chart'), plots: n.querySelectorAll('.uplot').length, canvases: n.querySelectorAll('canvas').length, status: n.querySelector('.chart-status').hidden ? '' : n.querySelector('.chart-status').textContent, legend: n.querySelectorAll('.u-legend .u-series').length, text: n.querySelector('.chart-text').textContent.length }))`);
  console.log(JSON.stringify(cards));
  check("five chart groups on the overview", cards.length === 5, cards.map(c => c.chart).join(","));
  check("every chart group draws at least one uPlot canvas", cards.every(c => c.plots > 0 && c.canvases > 0), JSON.stringify(cards.map(c => [c.chart, c.plots])));
  check("no chart is left in a loading or error state", cards.every(c => c.status === ""), JSON.stringify(cards.map(c => c.status)));
  check("every chart has its textual summary", cards.every(c => c.text > 20));
  await shot("overview-desktop-light");

  // legend toggle on the first card
  const before = await ev(`(() => { const n = document.querySelector('[data-chart]'); const s = n.querySelectorAll('.u-legend .u-series'); return Array.from(s).map(x => x.classList.contains('u-off')); })()`);
  await ev(`(() => { const s = document.querySelectorAll('[data-chart]')[0].querySelectorAll('.u-legend .u-series')[1]; const t = s.querySelector('.u-marker') || s; t.click(); return 1; })()`);
  await sleep(400);
  const after = await ev(`(() => { const n = document.querySelector('[data-chart]'); const s = n.querySelectorAll('.u-legend .u-series'); return Array.from(s).map(x => x.classList.contains('u-off')); })()`);
  check("a legend click toggles a series", JSON.stringify(before) !== JSON.stringify(after), JSON.stringify({ before, after }));

  // period buttons: week → redraw; and Earlier
  const widthBefore = await ev(`document.querySelector('[data-chart] canvas').width`);
  await viewport(800, 900);
  await sleep(800);
  const widthAfter = await ev(`document.querySelector('[data-chart] canvas').width`);
  check("resizing redraws the plot", widthBefore !== widthAfter, `${widthBefore} -> ${widthAfter}`);
  const gran = await ev(`(() => { const b = Array.from(document.querySelectorAll('[data-chart]')[0].querySelectorAll('.chart-controls button')).map(x => x.textContent); return b; })()`);
  console.log("controls", JSON.stringify(gran));
  for (const g of gran.slice(0, 4)) {
    await ev(`(() => { const b = Array.from(document.querySelectorAll('[data-chart]')[0].querySelectorAll('.chart-controls button')).find(x => x.textContent === ${JSON.stringify(g)}); b.click(); return 1; })()`);
    await sleep(900);
    const st = await ev(`(() => { const n = document.querySelectorAll('[data-chart]')[0]; return { status: n.querySelector('.chart-status').hidden ? '' : n.querySelector('.chart-status').textContent, plots: n.querySelectorAll('.uplot').length, label: (n.querySelector('.chart-period')||{}).textContent }; })()`);
    check(`granularity "${g}" draws`, st.plots > 0 && st.status === "", JSON.stringify(st));
  }

  // dark theme and a phone layout
  await viewport(1280, 900);
  await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: "dark" }] });
  await nav(`${BASE}/admin`); await sleep(2500);
  await shot("overview-desktop-dark");
  const dark = await ev(`getComputedStyle(document.body).backgroundColor`);
  await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: "light" }] });
  await viewport(360, 800, true);
  await nav(`${BASE}/admin`); await sleep(2500);
  const hscroll = await ev(`document.documentElement.scrollWidth - document.documentElement.clientWidth`);
  check("360 px layout has no horizontal page scroll", hscroll <= 0, "overflow " + hscroll + " px");
  check("dark theme paints a dark background", /rgb\((\d+), (\d+), (\d+)\)/.test(dark) && dark.match(/\d+/g).slice(0, 3).every(v => +v < 80), dark);
  await shot("overview-phone");
  for (const p of ["/admin/crawlers", "/admin/users", "/admin/invitations", "/admin/trackers", "/admin/quarantine", "/torrents"]) {
    await nav(BASE + p); await sleep(800);
    const ov = await ev(`document.documentElement.scrollWidth - document.documentElement.clientWidth`);
    check(`360 px: ${p} has no horizontal page scroll`, ov <= 0, "overflow " + ov + " px");
    if (p === "/admin/trackers") await shot("trackers-phone");
  }

  // the listings
  await viewport(1280, 900);
  for (const p of ["/admin/crawlers", "/admin/users", "/admin/invitations", "/admin/trackers", "/admin/quarantine"]) {
    await nav(BASE + p);
    const r = await ev(`({ path: location.pathname, h1: (document.querySelector('h1')||{}).textContent, bad: /Internal Server Error|panic|undefined/i.test(document.body.innerText) })`);
    check(`${p} renders`, r.path === p && !r.bad, JSON.stringify(r));
    if (p === "/admin/crawlers") await shot("crawlers");
  }

  report.requests = [...report.requests];
  check("no request left the loopback origin", report.requests.every(o => o === BASE || o === "null" || o.startsWith("data:") || o === "blob:null"), report.requests.join(","));
  check("no CSP violation", report.csp.length === 0, report.csp.join(" | "));
  check("no console error or exception", report.console.filter(c => c.type === "error").length === 0 && report.exceptions.length === 0, JSON.stringify({ console: report.console.slice(0, 5), exceptions: report.exceptions }));
  fs.writeFileSync(`${OUT}/report.json`, JSON.stringify(report, null, 2));
  const failed = report.checks.filter(c => !c.ok).length;
  console.log(failed ? `${failed} check(s) FAILED` : "all checks passed");
  tab.close(); process.exit(failed ? 1 : 0);
})().catch(e => { console.error("driver error:", e); process.exit(2); });
