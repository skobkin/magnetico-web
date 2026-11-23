/* In-page self-tests of static/app.js. Test-only: never served by the
 * application. It needs a DOM and runs after app.js was loaded with
 * <html data-selftest="1">. window.runSelfTests(vectors) resolves to
 * {passed, failed, results:[{name, ok, detail}]}. */
window.runSelfTests = async function (vectors) {
  "use strict";
  var UI = window.MagneticoUI, results = [];
  function check(name, ok, detail) { results.push({ name: name, ok: !!ok, detail: ok ? "" : String(detail === undefined ? "" : detail) }); }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function q(root, sel) { return root.querySelector(sel); }
  function qa(root, sel) { return Array.prototype.slice.call(root.querySelectorAll(sel)); }
  function texts(root, sel) { return qa(root, sel).map(function (n) { return n.textContent; }); }
  function b64(s) { return btoa(s); }

  function fakeFetch(handler) {
    var f = function (url, opts) {
      f.calls.push(url);
      return new Promise(function (resolve, reject) {
        var settled = false;
        function abort() { if (settled) return; settled = true; var e = new Error("aborted"); e.name = "AbortError"; reject(e); }
        if (opts.signal.aborted) return abort();
        opts.signal.addEventListener("abort", abort);
        Promise.resolve(handler(url)).then(function (r) {
          if (settled) return;
          settled = true;
          var ct = r.problem ? "application/problem+json" : "application/json";
          resolve({
            ok: r.status >= 200 && r.status < 300, status: r.status,
            headers: { get: function (n) { return n.toLowerCase() === "content-type" ? ct : null; } },
            text: function () { return Promise.resolve(r.raw !== undefined ? r.raw : JSON.stringify(r.body)); }
          });
        }, function (err) {
          if (settled) return;
          settled = true;
          reject(err);
        });
      });
    };
    f.calls = [];
    return f;
  }
  function mkRoot(mode) {
    var el = document.createElement("div");
    el.setAttribute("data-mode", mode);
    el.setAttribute("data-tree-url", "/t/tree");
    el.setAttribute("data-dir-url", "/t/files");
    document.body.appendChild(el);
    return el;
  }
  /* A recording stand-in for window.location/history (the page under test
   * need not have a real URL). pushState must never be used. */
  function fakeEnv(search, hash, state) {
    var e = { location: { href: "http://h/torrents/1" + search + hash, pathname: "/torrents/1", search: search, hash: hash } };
    e.history = {
      state: state, replaced: 0, pushed: 0,
      replaceState: function (st, title, url) {
        this.replaced++; this.state = st;
        var u = new URL(url, "http://h");
        e.location.search = u.search; e.location.hash = u.hash; e.location.href = "http://h" + url;
      },
      pushState: function () { this.pushed++; }
    };
    return e;
  }
  function problem(status, slug) { return { status: status, problem: true, body: { type: "urn:magnetico:problem:" + slug, status: status } }; }

  /* 1. shared name vectors (Go pathfmt.Display is the reference) */
  var bad = [];
  vectors.forEach(function (v, i) {
    var got = JSON.stringify(UI.segmentsOfBytes(UI.base64Bytes(v.bytes_base64), 0).map(function (s) { return s.marker ? { text: s.text, marker: true } : { text: s.text }; }));
    var want = JSON.stringify(v.segments.map(function (s) { return s.marker ? { text: s.text, marker: true } : { text: s.text }; }));
    if (got !== want) bad.push(i + ": " + got + " != " + want);
  });
  check("name vectors (" + vectors.length + ") match Go pathfmt", bad.length === 0, bad.slice(0, 3).join(" | "));
  var e1 = UI.entrySegments({ name: "", name_base64: undefined }, 0);
  check("empty component marker", e1.length === 1 && e1[0].marker && e1[0].text === "(empty)", JSON.stringify(e1));
  var trunc = UI.segmentsOfBytes(new Uint8Array(5000).fill(97), 2048);
  check("truncation is explicit", trunc.length === 2 && trunc[1].marker && trunc[1].text === "…(2952 more bytes)", JSON.stringify(trunc.map(function (s) { return s.text.length > 40 ? s.text.length : s.text; })));
  check("formatSize", UI.formatSize(0) === "0 B" && UI.formatSize(1536) === "1.5 KiB" && UI.formatSize(1073741824) === "1.0 GiB", [UI.formatSize(1536), UI.formatSize(1073741824)]);

  /* 2. Mode A */
  function treeDoc(nFiles) {
    var files = [];
    for (var i = 0; i < nFiles; i++) files.push({ name: "episode-" + i + ".mkv", size: 100 + i, file_index: i + 3, duplicate: false });
    return {
      version: 1, file_count: nFiles + 5, size: 99999,
      root: {
        file_count: nFiles + 5, size: 99999,
        dirs: [
          { name: "Season 1", file_count: nFiles, size: 5000, dirs: [], files: files },
          { name: null, name_base64: b64("bad\xffdir"), file_count: 2, size: 20, dirs: [], files: [
            { name: "same.txt", size: 10, file_index: 0, duplicate: false }, { name: "same.txt", size: 10, file_index: 1, duplicate: true }] },
          { name: "", file_count: 1, size: 7, dirs: [], files: [{ name: "x<img src=x onerror=alert(1)>.txt", size: 7, file_index: 2, duplicate: false }] }
        ],
        files: [{ name: "README.txt", size: 5, file_index: nFiles + 3, duplicate: false }, { name: null, name_base64: b64("a\xfe.bin"), size: 6, file_index: nFiles + 4, duplicate: false }]
      }
    };
  }
  var f = fakeFetch(function () { return { status: 200, body: treeDoc(300) }; });
  var root = mkRoot("tree");
  UI.initFileBrowser(root, { fetch: f });
  check("tree: loading state shown at once", !!q(root, ".file-loading"), root.textContent);
  await sleep(60);
  check("tree: one request, no more after load", f.calls.length === 1 && f.calls[0] === "/t/tree", f.calls);
  var rows = qa(root, ".tree-row");
  check("tree: folders collapsed by default (305 files)", rows.length === 5 && qa(root, ".tree-row .file-icon").length === 2, rows.length);
  // open the two small folders first: their rows come after the 300-file season
  qa(root, "button[data-id]")[2].click();
  await sleep(10);
  qa(root, "button[data-id]")[1].click();
  await sleep(10);
  var labels = texts(root, ".file-label").join("|");
  check("tree: invalid bytes escaped, never U+FFFD", labels.indexOf("bad\\xffdir") >= 0 && labels.indexOf("a\\xfe.bin") >= 0 && labels.indexOf("\uFFFD") < 0, labels.slice(0, 200));
  check("tree: empty component marker", labels.indexOf("(empty)") >= 0, labels.slice(0, 200));
  check("tree: hostile name stays text", !root.querySelector("img") && labels.indexOf("x<img src=x onerror=alert(1)>.txt") >= 0, labels.slice(0, 300));
  q(root, "button[data-id]").click(); // Season 1: 300 files, rendered in chunks of 120 rows
  await sleep(10);
  rows = qa(root, ".tree-row");
  check("tree: expand renders at most one chunk of rows", rows.length === 120 && !q(root, '[data-act="more"]').hidden, rows.length);
  q(root, '[data-act="more"]').click();
  await sleep(10);
  check("tree: show more adds a chunk", qa(root, ".tree-row").length === 240, qa(root, ".tree-row").length);
  check("tree: expand/collapse made no request", f.calls.length === 1, f.calls);
  check("tree: expanded button has aria-expanded=true", q(root, 'button[data-id]').getAttribute("aria-expanded") === "true");
    // filter
  var input = q(root, "[data-filter]");
  input.value = "EPISODE-29";
  input.dispatchEvent(new Event("input", { bubbles: true }));
  await sleep(200);
  rows = qa(root, ".tree-row");
  var ftexts = texts(root, ".file-label");
  check("tree: filter keeps matches and their parent folder (11 files)", rows.length === 12 && ftexts[0] === "Season 1", rows.length + " " + ftexts.slice(0, 3));
  check("tree: filter status counts matches", /11 matching files/.test(q(root, "[data-status]").textContent), q(root, "[data-status]").textContent);
  input.value = "episode-29x";
  input.dispatchEvent(new Event("input", { bubbles: true }));
  await sleep(200);
  check("tree: empty filter result message", /No file names match/.test(root.textContent), root.textContent.slice(0, 100));
  q(root, '[data-act="clear"]').click();
  await sleep(10);
  rows = qa(root, ".tree-row");
  check("tree: clearing the filter restores manual expansion", rows.length === 120 && input.value === "", rows.length);
  q(root, '[data-act="collapse"]').click();
  await sleep(10);
  check("tree: collapse all", qa(root, ".tree-row").length === 5, qa(root, ".tree-row").length);
  q(root, '[data-act="expand"]').click();
  await sleep(10);
  check("tree: expand all opens every folder", qa(root, ".tree-row").length > 5 && qa(root, '.folder-toggle[aria-expanded="false"]').length === 0, qa(root, ".tree-row").length);
  q(root, '[data-act="collapse"]').click();
  await sleep(10);

  // small torrents expand everything
  var fs = fakeFetch(function () { var d = treeDoc(3); d.file_count = 8; return { status: 200, body: d }; });
  var rs = mkRoot("tree");
  UI.initFileBrowser(rs, { fetch: fs });
  await sleep(60);
  check("tree: <=20 files start expanded", qa(rs, ".tree-row").length === 3 + 3 + 2 + 1 + 2, qa(rs, ".tree-row").length);
  check("tree: hostile name is text, not markup", !rs.querySelector("img") && texts(rs, ".file-label").indexOf("x<img src=x onerror=alert(1)>.txt") >= 0, texts(rs, ".file-label"));

  // errors, retry, timeout, switch to dirs, complexity
  var n = 0;
  var fr = fakeFetch(function () { n++; return n === 1 ? { status: 503, body: {} } : { status: 200, body: treeDoc(3) }; });
  var rr = mkRoot("tree");
  UI.initFileBrowser(rr, { fetch: fr });
  await sleep(60);
  check("tree: 503 shows an alert with Retry", !!q(rr, '[role="alert"]') && !!q(rr, '[data-act="retry"]'), rr.textContent);
  q(rr, '[data-act="retry"]').click();
  await sleep(60);
  check("tree: retry loads the tree", fr.calls.length === 2 && qa(rr, ".tree-row").length > 0, fr.calls);
  var ft = fakeFetch(function () { return new Promise(function () {}); });
  var rt = mkRoot("tree");
  UI.initFileBrowser(rt, { fetch: ft, timeoutMs: 50 });
  await sleep(150);
  check("tree: a stalled request times out with Retry", /did not answer in time/.test(rt.textContent) && !!q(rt, '[data-act="retry"]'), rt.textContent);
  var fn = fakeFetch(function () { return Promise.reject(new Error("net")); });
  var rn = mkRoot("tree");
  UI.initFileBrowser(rn, { fetch: fn });
  await sleep(60);
  check("tree: a network failure shows Retry", /Check your connection/.test(rn.textContent), rn.textContent);
  var fb = fakeFetch(function () { return { status: 200, raw: "not json" }; });
  var rb = mkRoot("tree");
  UI.initFileBrowser(rb, { fetch: fb });
  await sleep(60);
  check("tree: unreadable answer is an error, not a blank list", /unreadable/.test(rb.textContent), rb.textContent);
  var fx = fakeFetch(function () { return { status: 200, body: { version: 2, root: {} } }; });
  var rx = mkRoot("tree");
  UI.initFileBrowser(rx, { fetch: fx });
  await sleep(60);
  check("tree: wrong schema version is rejected", /unreadable/.test(rx.textContent), rx.textContent);
  var f401 = fakeFetch(function () { return problem(401, "unauthenticated"); });
  var r401 = mkRoot("tree");
  UI.initFileBrowser(r401, { fetch: f401 });
  await sleep(60);
  check("tree: 401 offers sign-in", /session has expired/.test(r401.textContent) && !!q(r401, 'a[href^="/login"]'), r401.textContent);
  var fc = fakeFetch(function () { return problem(422, "index-too-complex"); });
  var rc = mkRoot("tree");
  UI.initFileBrowser(rc, { fetch: fc });
  await sleep(60);
  check("tree: index-too-complex is final (no fallback, no retry)", /exceeds this server/.test(rc.textContent) && !q(rc, '[data-act="retry"]') && fc.calls.length === 1, fc.calls);
  var pages = { "/t/files?limit=160": { status: 200, body: page("", [], [dirEntry("a", "0", 2), fileEntry("f", 0)], 2, null) } };
  var fu = fakeFetch(function (url) { return url === "/t/tree" ? problem(422, "full-tree-unavailable") : (pages[url] || { status: 404, body: {} }); });
  var ru = mkRoot("tree");
  UI.initFileBrowser(ru, { fetch: fu });
  await sleep(80);
  check("tree: full-tree-unavailable switches to directory mode", fu.calls[0] === "/t/tree" && fu.calls[1] === "/t/files?limit=160" && /On-demand browsing/.test(ru.textContent), fu.calls);

  /* 3. Mode B */
  function dirEntry(name, path, count) { return { type: "dir", name: name, path: path, file_count: count, size: 1000, dir_count: 0, entry_count: count }; }
  function fileEntry(name, idx, dup) { return { type: "file", name: name, size: 10, file_index: idx, duplicate: !!dup }; }
  function page(path, anc, entries, total, next, offset) {
    return { version: 1, path: path, ancestors: anc, dir: { file_count: total, size: 1, dir_count: 0, entry_count: total }, offset: offset || 0, limit: 160, next_offset: next, entries: entries };
  }
  var routes = {
    "/t/files?limit=160": page("", [], [dirEntry("docs", "0", 400), dirEntry("", "1", 1), { type: "dir", name: null, name_base64: b64("b\xffd"), path: "2", file_count: 1, size: 1, dir_count: 0, entry_count: 1 }, fileEntry("top.txt", 9)], 4, null),
    "/t/files?dir=0&limit=160": page("0", [{ name: "docs", path: "0" }], [fileEntry("f0", 0), fileEntry("f1", 1), fileEntry("f1", 2, true)], 400, 160),
    "/t/files?dir=0&offset=160&limit=160": page("0", [{ name: "docs", path: "0" }], [fileEntry("g0", 160)], 400, 320, 160),
    "/t/files?dir=0&offset=320&limit=160": page("0", [{ name: "docs", path: "0" }], [fileEntry("h0", 320)], 400, null, 320)
  };
  var calls = [];
  var fd = fakeFetch(function (url) { calls.push(url); return routes[url] ? { status: 200, body: routes[url] } : problem(404, "not-found"); });
  var envD = fakeEnv("?x=1", "#frag", { keep: 1 });
  var rd = mkRoot("dirs");
  UI.initFileBrowser(rd, { fetch: fd, location: envD.location, history: envD.history });
  check("dirs: loading state at once", !!q(rd, ".file-loading"));
  await sleep(60);
  check("dirs: root listing rendered", texts(rd, ".file-label").join("|") === "docs|(empty)|b\\xffd|top.txt", texts(rd, ".file-label"));
  check("dirs: Previous disabled, Next disabled at the end", q(rd, '[data-page="prev"]').disabled && q(rd, '[data-page="next"]').disabled);
  check("dirs: showing text", /Showing 1–4 of 4 entries/.test(rd.textContent), rd.textContent);
  qa(rd, ".server-folder-link")[0].click();
  await sleep(60);
  check("dirs: folder opens in place with breadcrumbs", calls[1] === "/t/files?dir=0&limit=160" && /docs/.test(q(rd, ".file-breadcrumbs").textContent) && q(rd, ".file-breadcrumbs button[aria-current]") !== null, calls);
  check("dirs: URL updated with replaceState, other parts kept", envD.location.search.indexOf("x=1") >= 0 && envD.location.search.indexOf("dir=0") >= 0 && envD.location.hash === "#frag" && envD.history.state && envD.history.state.keep === 1, envD.location.href);
  check("dirs: duplicate marker shown", texts(rd, ".file-marker").indexOf("Duplicate path") >= 0, texts(rd, ".file-marker"));
  q(rd, '[data-page="next"]').click();
  await sleep(60);
  check("dirs: next page uses next_offset", calls[2] === "/t/files?dir=0&offset=160&limit=160" && /Showing 161–161 of 400/.test(rd.textContent), calls);
  check("dirs: offset in URL", envD.location.search.indexOf("offset=160") >= 0, envD.location.search);
  q(rd, '[data-page="next"]').click();
  await sleep(60);
  q(rd, '[data-page="prev"]').click();
  await sleep(60);
  check("dirs: previous returns to the earlier offset", calls[4] === "/t/files?dir=0&offset=160&limit=160", calls);
  q(rd, "button[data-path]").click(); // Root crumb
  await sleep(60);
  check("dirs: root crumb returns to the top and clears dir from URL", envD.location.search.indexOf("dir=") < 0 && envD.location.search.indexOf("x=1") >= 0, envD.location.search);
  qa(rd, ".server-folder-link")[0].click();
  await sleep(60);
  check("dirs: navigation only replaces history, never pushes", envD.history.pushed === 0 && envD.history.replaced >= 4, envD.history.pushed + "/" + envD.history.replaced);

  // error + retry + stale request cancellation
  var hits = 0;
  var fe = fakeFetch(function (url) {
    if (url === "/t/files?limit=160") return { status: 200, body: routes["/t/files?limit=160"] };
    if (url === "/t/files?dir=0&limit=160") { hits++; return hits === 1 ? { status: 503, body: {} } : { status: 200, body: routes[url] }; }
    return problem(404, "not-found");
  });
  var re = mkRoot("dirs");
  UI.initFileBrowser(re, { fetch: fe, location: fakeEnv("", "", null).location, history: fakeEnv("", "", null).history });
  await sleep(60);
  qa(re, ".server-folder-link")[0].click();
  await sleep(60);
  check("dirs: error keeps breadcrumbs and offers Retry", !!q(re, '[role="alert"]') && !!q(re, '[data-act="retry"]') && !!q(re, ".file-breadcrumbs"), re.textContent);
  q(re, '[data-act="retry"]').click();
  await sleep(60);
  check("dirs: retry loads the directory", /f0/.test(re.textContent), re.textContent);
  var slow = fakeFetch(function (url) {
    if (url === "/t/files?limit=160") return { status: 200, body: routes[url] };
    if (url === "/t/files?dir=0&limit=160") return new Promise(function (r) { setTimeout(function () { r({ status: 200, body: routes[url] }); }, 100); });
    return { status: 200, body: page("1", [{ name: "", path: "1" }], [fileEntry("late", 5)], 1, null) };
  });
  var env2 = fakeEnv("", "", null);
  var rs2 = mkRoot("dirs");
  UI.initFileBrowser(rs2, { fetch: slow, location: env2.location, history: env2.history });
  await sleep(60);
  var links = qa(rs2, ".server-folder-link");
  links[0].click();
  await sleep(10);
  var again = qa(rs2, "button[data-path]").filter(function (b) { return b.getAttribute("data-path") === "" })[0];
  again.click();
  await sleep(200);
  check("dirs: a superseded request is cancelled and never overwrites newer state", !/f0/.test(rs2.textContent) && /top\.txt/.test(rs2.textContent), rs2.textContent);
  var tmo = fakeFetch(function () { return new Promise(function () {}); });
  var env3 = fakeEnv("", "", null);
  var rm = mkRoot("dirs");
  UI.initFileBrowser(rm, { fetch: tmo, timeoutMs: 60, location: env3.location, history: env3.history });
  await sleep(30);
  await sleep(100);
  check("dirs: root request times out cleanly", /did not answer in time/.test(rm.textContent), rm.textContent);

  // restore from URL, fallback when stale
  var env4 = fakeEnv("?dir=0&offset=160", "", null);
  var fr2 = fakeFetch(function (url) { return routes[url] ? { status: 200, body: routes[url] } : problem(404, "not-found"); });
  var rr2 = mkRoot("dirs");
  UI.initFileBrowser(rr2, { fetch: fr2, location: env4.location, history: env4.history });
  await sleep(60);
  check("dirs: reload restores the location from the URL", fr2.calls[0] === "/t/files?dir=0&offset=160&limit=160" && /g0/.test(rr2.textContent), fr2.calls);
  var env5 = fakeEnv("?dir=7_7&offset=3", "", null);
  var fr3 = fakeFetch(function (url) { return routes[url] ? { status: 200, body: routes[url] } : problem(404, "not-found"); });
  var rr3 = mkRoot("dirs");
  UI.initFileBrowser(rr3, { fetch: fr3, location: env5.location, history: env5.history });
  await sleep(100);
  check("dirs: a stale location falls back to the top with a notice", fr3.calls.length === 2 && /no longer available/.test(rr3.textContent) && /docs/.test(rr3.textContent), fr3.calls);
  var loc = UI.readLocation({ search: "?dir=01&offset=abc" });
  check("dirs: malformed URL parameters are ignored", loc.path === "" && loc.offset === 0, JSON.stringify(loc));

  // Root pagination restore and byte-limited pages of variable size.
  var rl = UI.readLocation({ search: "?offset=120" });
  check("dirs: the root offset survives readLocation", rl.path === "" && rl.offset === 120, JSON.stringify(rl));
  var COUNTS = { 0: 50, 50: 70, 70: 60, 120: 80 }, TOTAL = 200;
  function rootAt(off) {
    var n = Math.min(COUNTS[off] || 40, TOTAL - off), es = [];
    for (var i = off; i < off + n; i++) es.push(fileEntry("e" + i, i));
    return page("", [], es, TOTAL, off + n < TOTAL ? off + n : null, off);
  }
  function rootFetch() {
    var f = fakeFetch(function (url) {
      var m = /^\/t\/files\?(?:offset=(\d+)&)?limit=160$/.exec(url);
      if (!m) return problem(404, "not-found");
      var off = m[1] ? Number(m[1]) : 0;
      return off >= TOTAL ? { status: 200, body: page("", [], [], TOTAL, null, off) } : { status: 200, body: rootAt(off) };
    });
    return f;
  }
  var envR = fakeEnv("?offset=120", "", null);
  var fR = rootFetch();
  var rR = mkRoot("dirs");
  UI.initFileBrowser(rR, { fetch: fR, location: envR.location, history: envR.history });
  await sleep(60);
  check("dirs: root page 2 reload restores the root offset", fR.calls[0] === "/t/files?offset=120&limit=160" && /Showing 121–200 of 200/.test(rR.textContent) && envR.location.search.indexOf("offset=120") >= 0, fR.calls.concat(envR.location.search));
  q(rR, '[data-page="prev"]').click();
  await sleep(100);
  var seen = texts(rR, ".file-label");
  check("dirs: Previous after reload reaches the page we left (no skipped entries)", fR.calls.length === 3 && fR.calls[1] === "/t/files?limit=160" && fR.calls[2] === "/t/files?offset=70&limit=160" && seen[0] === "e70" && seen[seen.length - 1] === "e129", fR.calls.concat(seen.slice(0, 1), seen.slice(-1)));
  check("dirs: the restored back page is reflected in the URL", envR.location.search.indexOf("offset=70") >= 0, envR.location.search);
  q(rR, '[data-page="next"]').click();
  await sleep(60);
  check("dirs: Next after a restored Previous continues from next_offset", /Showing 131–200/.test(rR.textContent) || fR.calls[3] === "/t/files?offset=130&limit=160", fR.calls);
  // Offset restored from the URL, then forward and back inside one view uses the exact earlier pages.
  var envS = fakeEnv("?offset=50", "", null);
  var fS = rootFetch();
  var rS = mkRoot("dirs");
  UI.initFileBrowser(rS, { fetch: fS, location: envS.location, history: envS.history });
  await sleep(60);
  q(rS, '[data-page="next"]').click();
  await sleep(60);
  q(rS, '[data-page="prev"]').click();
  await sleep(60);
  check("dirs: navigation after restoring an offset returns to that exact page", fS.calls[fS.calls.length - 1] === "/t/files?offset=50&limit=160" && /Showing 51–120/.test(rS.textContent), fS.calls);
  // Previous from the second page reaches the first page exactly.
  var envT = fakeEnv("?offset=50", "", null);
  var fT = rootFetch();
  var rT = mkRoot("dirs");
  UI.initFileBrowser(rT, { fetch: fT, location: envT.location, history: envT.history });
  await sleep(60);
  q(rT, '[data-page="prev"]').click();
  await sleep(60);
  check("dirs: Previous to the start needs no extra requests when the page already reaches", fT.calls.length === 2 && /Showing 1–50 of 200/.test(rT.textContent), fT.calls);
  // A restored offset beyond the end falls back to the top with a notice.
  var envU = fakeEnv("?offset=5000", "", null);
  var fU = rootFetch();
  var rU = mkRoot("dirs");
  UI.initFileBrowser(rU, { fetch: fU, location: envU.location, history: envU.history });
  await sleep(80);
  check("dirs: a stale root offset falls back to the top with a notice", /no longer available/.test(rU.textContent) && /Showing 1–50/.test(rU.textContent), fU.calls);

  var failed = results.filter(function (r) { return !r.ok; });
  return { passed: results.length - failed.length, failed: failed.length, results: results };
};
