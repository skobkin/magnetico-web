/* Magnetico web UI: first-party enhancements, no framework, no external requests.
 *
 * Everything except the file browser works without this script. Untrusted
 * text (torrent and file names) is only ever inserted with createTextNode /
 * textContent; this file never assigns HTML strings.
 *
 * Contract: docs/specs/web-v1.md (file-tree v1, file-dir-page v1). The server
 * decides whether a torrent is browsed as a complete tree (data-mode="tree")
 * or directory by directory (data-mode="dirs"); this script never estimates
 * sizes or chooses a mode itself.
 */
(function () {
  "use strict";

  var doc = document;
  var SVG_NS = "http://www.w3.org/2000/svg";
  var TREE_CHUNK = 120;
  var PAGE_LIMIT = 160;
  var NAME_DISPLAY_BYTES = 2048;
  var REQUEST_TIMEOUT_MS = 30000;

  function $(sel, root) { return (root || doc).querySelector(sel); }
  function $$(sel, root) { return Array.prototype.slice.call((root || doc).querySelectorAll(sel)); }

  /* ---- tiny DOM builder (text only, never HTML) ------------------------- */

  function h(tag, props, kids) {
    var node = doc.createElement(tag);
    if (props) {
      for (var k in props) {
        if (!Object.prototype.hasOwnProperty.call(props, k)) continue;
        var v = props[k];
        if (v === false || v === null || v === undefined) continue;
        if (k === "text") node.textContent = v;
        else if (k === "class") node.className = v;
        else node.setAttribute(k, v === true ? "" : String(v));
      }
    }
    (kids || []).forEach(function (c) {
      if (c === null || c === undefined || c === false) return;
      node.appendChild(typeof c === "string" ? doc.createTextNode(c) : c);
    });
    return node;
  }

  function icon(name, cls) {
    var svg = doc.createElementNS(SVG_NS, "svg");
    svg.setAttribute("class", "icon" + (cls ? " " + cls : ""));
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("focusable", "false");
    var use = doc.createElementNS(SVG_NS, "use");
    use.setAttribute("href", "#i-" + name);
    svg.appendChild(use);
    return svg;
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  /* ---- names: lossless display, identical to Go's pathfmt.Display --------- */

  var HEX = "0123456789abcdef";

  function needsMarker(cp) {
    return cp < 0x20 || (cp >= 0x7f && cp <= 0x9f) || cp === 0x061c || cp === 0x200e || cp === 0x200f ||
      cp === 0x2028 || cp === 0x2029 || (cp >= 0x202a && cp <= 0x202e) || (cp >= 0x2066 && cp <= 0x2069) ||
      cp === 0xfffd;
  }

  function runeMarker(cp) {
    var s = cp.toString(16).toUpperCase();
    while (s.length < 4) s = "0" + s;
    return "\\u{" + s + "}";
  }

  function byteMarker(b) { return "\\x" + HEX.charAt(b >> 4) + HEX.charAt(b & 15); }

  /* decodeAt follows Go's utf8.DecodeRune: it returns [codePoint, width], or
   * [-1, 1] for an invalid byte (any malformed or truncated sequence consumes
   * exactly one byte). */
  function decodeAt(b, i) {
    var c = b[i], n, cp, lo = 0x80, hi = 0xbf;
    if (c < 0x80) return [c, 1];
    if (c >= 0xc2 && c <= 0xdf) { n = 1; cp = c & 0x1f; }
    else if (c >= 0xe0 && c <= 0xef) { n = 2; cp = c & 0x0f; if (c === 0xe0) lo = 0xa0; if (c === 0xed) hi = 0x9f; }
    else if (c >= 0xf0 && c <= 0xf4) { n = 3; cp = c & 0x07; if (c === 0xf0) lo = 0x90; if (c === 0xf4) hi = 0x8f; }
    else return [-1, 1];
    for (var k = 1; k <= n; k++) {
      var x = b[i + k];
      if (x === undefined || x < (k === 1 ? lo : 0x80) || x > (k === 1 ? hi : 0xbf)) return [-1, 1];
      cp = (cp << 6) | (x & 0x3f);
    }
    return [cp, n + 1];
  }

  /* segments: [{text, marker}], text runs merged, every marker its own segment.
   * maxBytes > 0 truncates at the first unit boundary at or beyond maxBytes
   * with an explicit "…(N more bytes)" marker, like pathfmt.Component. */
  function segmentsOfBytes(b, maxBytes) {
    var out = [], run = "";
    function flush() { if (run) { out.push({ text: run, marker: false }); run = ""; } }
    function mark(t) { flush(); out.push({ text: t, marker: true }); }
    for (var i = 0; i < b.length;) {
      if (maxBytes > 0 && i >= maxBytes) { mark("…(" + (b.length - i) + " more bytes)"); return out; }
      var d = decodeAt(b, i);
      if (d[0] < 0) mark(byteMarker(b[i]));
      else if (needsMarker(d[0])) mark(runeMarker(d[0]));
      else run += String.fromCodePoint(d[0]);
      i += d[1];
    }
    flush();
    return out;
  }

  var encoder = typeof TextEncoder === "function" ? new TextEncoder() : null;

  function base64Bytes(s) {
    var bin = atob(s), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  /* entryBytes returns the exact name bytes of an entry or component: "name"
   * is valid UTF-8, otherwise "name" is null and "name_base64" carries them. */
  function entryBytes(e) {
    if (typeof e.name === "string") return encoder.encode(e.name);
    if (typeof e.name_base64 === "string") return base64Bytes(e.name_base64);
    throw new Error("entry without a name");
  }

  function entrySegments(e, maxBytes) {
    var b = entryBytes(e);
    if (b.length === 0) return [{ text: "(empty)", marker: true }];
    return segmentsOfBytes(b, maxBytes || 0);
  }

  function plain(segs) { return segs.map(function (s) { return s.text; }).join(""); }

  function nameNode(segs, extraClass) {
    var span = h("span", { class: "file-label" + (extraClass ? " " + extraClass : "") });
    segs.forEach(function (s) {
      if (s.marker) span.appendChild(h("span", { class: "marker", text: s.text }));
      else span.appendChild(doc.createTextNode(s.text));
    });
    return span;
  }

  function hasMarker(segs) { return segs.some(function (s) { return s.marker; }); }

  function formatSize(n) {
    if (n < 1024) return n + " B";
    var div = 1024, exp = 0;
    for (var m = Math.floor(n / 1024); m >= 1024 && exp < 5; m = Math.floor(m / 1024)) { div *= 1024; exp++; }
    return (n / div).toFixed(1) + " " + "KMGTPE".charAt(exp) + "iB";
  }

  function countText(n) { return n.toLocaleString("en-US") + (n === 1 ? " file" : " files"); }

  /* ---- requests ---------------------------------------------------------- */

  /* request resolves {status, body, problem}; it rejects with an Error whose
   * .kind is "timeout", "aborted" or "network". An aborted request is a
   * normal outcome of navigation and callers ignore it. */
  function makeRequester(opts) {
    var doFetch = opts.fetch || function (u, o) { return window.fetch(u, o); };
    var timeoutMs = opts.timeoutMs || REQUEST_TIMEOUT_MS;
    return function request(url, signalHolder) {
      var ctl = new AbortController();
      var timedOut = false;
      if (signalHolder.ctl) signalHolder.ctl.abort();
      signalHolder.ctl = ctl;
      var timer = setTimeout(function () { timedOut = true; ctl.abort(); }, timeoutMs);
      return doFetch(url, {
        credentials: "same-origin", cache: "no-store", headers: { Accept: "application/json" }, signal: ctl.signal
      }).then(function (res) {
        return res.text().then(function (text) {
          clearTimeout(timer);
          var json = null;
          try { json = text ? JSON.parse(text) : null; } catch (e) { json = null; }
          var isProblem = /problem\+json/.test(res.headers.get("Content-Type") || "");
          return { status: res.status, body: res.ok ? json : null, problem: !res.ok && isProblem ? json : null, bad: res.ok && json === null };
        });
      }).catch(function (err) {
        clearTimeout(timer);
        var kind = timedOut ? "timeout" : (err && err.name === "AbortError" ? "aborted" : "network");
        var e = new Error(kind);
        e.kind = kind;
        throw e;
      });
    };
  }

  /* failure maps a non-OK response or a rejection to {message, retry, switchToDirs, signIn}. */
  function failure(res, err) {
    if (err) {
      if (err.kind === "timeout") return { message: "The server did not answer in time.", retry: true };
      return { message: "The file list could not be loaded. Check your connection.", retry: true };
    }
    var type = res.problem && typeof res.problem.type === "string" ? res.problem.type : "";
    if (res.status === 401) return { message: "Your session has expired.", signIn: true };
    if (type === "urn:magnetico:problem:full-tree-unavailable") return { message: "", switchToDirs: true };
    if (type === "urn:magnetico:problem:index-too-complex") {
      return { message: "The file structure of this torrent exceeds this server's browsing limits." };
    }
    if (res.status === 503) return { message: "The server is busy. Please try again shortly.", retry: true };
    if (res.status === 404) return { message: "This location no longer exists.", notFound: true };
    if (res.status === 400) return { message: "This request was not valid.", notFound: true };
    if (res.bad) return { message: "The server sent an unreadable answer.", retry: true };
    return { message: "The file list could not be loaded.", retry: true };
  }

  /* ---- shared pieces of the browser panels ------------------------------- */

  function loadingBlock(text) {
    return h("div", { class: "file-loading", role: "status" }, [h("span", { class: "spinner", "aria-hidden": "true" }), text]);
  }

  function errorBlock(f, retryAct) {
    var kids = [icon("alert"), h("span", { text: f.message })];
    if (f.retry) kids.push(h("button", { type: "button", class: "btn small", "data-act": retryAct, text: "Retry" }));
    if (f.signIn) {
      var next = encodeURIComponent(location.pathname + location.search);
      kids.push(h("a", { class: "btn small", href: "/login?next=" + next, text: "Sign in" }));
    }
    return h("div", { class: "file-loading file-error", role: "alert" }, kids);
  }

  /* liveRegion appends one persistent polite status node to root (outside the
   * panel that is redrawn) and returns the function that sets its text. */
  function liveRegion(root) {
    var live = h("div", { class: "sr-only", role: "status", "aria-live": "polite" });
    root.appendChild(live);
    return function (text) { live.textContent = text; };
  }

  function tags(segs, dup) {
    var out = [];
    if (dup) out.push(h("span", { class: "file-marker", text: "Duplicate path" }));
    else if (hasMarker(segs)) out.push(h("span", { class: "file-marker", text: "Escaped name" }));
    return out;
  }

  /* ---- Mode A: complete tree held in the browser ------------------------- */

  function buildTree(treeDoc) {
    if (!treeDoc || treeDoc.version !== 1 || !treeDoc.root || !Array.isArray(treeDoc.root.dirs) || !Array.isArray(treeDoc.root.files)) {
      throw new Error("invalid tree");
    }
    var nodes = [];
    function add(parent, type, e) {
      var n = { id: nodes.length, parent: parent, type: type, e: e, kids: [], count: 0, size: 0, dup: false, key: "" };
      nodes.push(n);
      return n;
    }
    var root = add(-1, "dir", null);
    root.count = treeDoc.root.file_count;
    root.size = treeDoc.root.size;
    function walk(dir, node) {
      if (!Array.isArray(dir.dirs) || !Array.isArray(dir.files)) throw new Error("invalid tree");
      dir.dirs.forEach(function (d) {
        var c = add(node.id, "dir", d);
        c.count = d.file_count;
        c.size = d.size;
        node.kids.push(c.id);
        walk(d, c);
      });
      dir.files.forEach(function (f) {
        var c = add(node.id, "file", f);
        c.size = f.size;
        c.dup = !!f.duplicate;
        c.key = (typeof f.name === "string" ? f.name : plain(segmentsOfBytes(entryBytes(f), 0))).toLowerCase();
        node.kids.push(c.id);
      });
    }
    walk(treeDoc.root, root);
    return { nodes: nodes, files: treeDoc.root.file_count };
  }

  /* visibleRows returns the rows to render (at most limit) for the tree state. */
  function visibleRows(tree, state) {
    var nodes = tree.nodes, q = state.filter, keep = null, found = 0;
    if (q) {
      keep = new Set([0]);
      for (var i = 1; i < nodes.length; i++) {
        var n = nodes[i];
        if (n.type !== "file" || n.key.indexOf(q) < 0) continue;
        found++;
        for (var p = n.id; p !== -1 && !keep.has(p); p = nodes[p].parent) keep.add(p);
      }
    }
    var rows = [], truncated = false;
    function isOpen(n) { return q ? !state.filterClosed.has(n.id) : state.expanded.has(n.id); }
    function walk(id, depth) {
      var kids = nodes[id].kids;
      for (var k = 0; k < kids.length; k++) {
        var c = nodes[kids[k]];
        if (keep && !keep.has(c.id)) continue;
        if (rows.length >= state.limit) { truncated = true; return; }
        var open = c.type === "dir" && isOpen(c);
        rows.push({ n: c, depth: depth, open: open });
        if (open) { walk(c.id, depth + 1); if (truncated) return; }
      }
    }
    walk(0, 0);
    return { rows: rows, found: found, truncated: truncated };
  }

  function indent(depth) {
    var span = h("span", { class: "tree-indent", "aria-hidden": "true" });
    for (var i = 0; i < Math.min(16, depth); i++) span.appendChild(h("span", { class: "tree-indent-step" }));
    return span;
  }

  function treeRow(r) {
    var n = r.n, segs = entrySegments(n.e, NAME_DISPLAY_BYTES);
    if (n.type === "dir") {
      var btn = h("button", { type: "button", class: "folder-toggle", "data-id": n.id, "aria-expanded": r.open ? "true" : "false" }, [
        h("span", { class: "disclosure" }, [icon("chevron")]),
        icon(r.open ? "folder-open" : "folder"),
        nameNode(segs)
      ]);
      return h("div", { class: "tree-row folder-row", role: "listitem" }, [
        indent(r.depth), btn, h("span", { class: "file-count", text: countText(n.count) }), h("span", { class: "file-size", text: formatSize(n.size) })
      ]);
    }
    return h("div", { class: "tree-row", role: "listitem" }, [
      indent(r.depth),
      h("div", { class: "tree-leaf" }, [icon("file", "file-icon"), nameNode(segs, hasMarker(segs) ? "unusual-path" : "")].concat(tags(segs, n.dup))),
      h("span", { class: "file-count" }), h("span", { class: "file-size", text: formatSize(n.size) })
    ]);
  }

  function runTree(root, ctx) {
    var holder = {}, tree = null;
    var state = { expanded: new Set(), filter: "", filterClosed: new Set(), limit: TREE_CHUNK };
    var panel = h("div");
    root.appendChild(panel);
    var announce = liveRegion(root);
    var timer = null;

    function load() {
      tree = null;
      clear(panel);
      panel.appendChild(loadingBlock("Loading the file list…"));
      ctx.request(ctx.treeUrl, holder).then(function (res) {
        if (res.status !== 200 || res.bad || !res.body) {
          var f = failure(res);
          if (f.switchToDirs) { ctx.switchToDirs(); return; }
          return showError(f);
        }
        try { tree = buildTree(res.body); } catch (e) { return showError({ message: "The server sent an unreadable answer.", retry: true }); }
        if (tree.files <= 20) tree.nodes.forEach(function (n) { if (n.type === "dir") state.expanded.add(n.id); });
        ready();
      }, function (err) {
        if (err.kind === "aborted") return;
        showError(failure(null, err));
      });
    }

    function showError(f) {
      clear(panel);
      panel.appendChild(errorBlock(f, "retry"));
      var b = $('[data-act="retry"]', panel);
      if (b) b.focus();
    }

    function ready() {
      clear(panel);
      var input = h("input", { type: "search", "data-filter": "", placeholder: "Filter files by name", autocomplete: "off", "aria-label": "Filter files by name" });
      panel.appendChild(h("div", { class: "browser-tooling" }, [
        h("label", { class: "file-search" }, [h("span", { class: "sr-only", text: "Filter file names" }), icon("search"), input]),
        h("button", { type: "button", class: "btn small ghost", "data-act": "clear", hidden: true, text: "Clear" }),
        h("button", { type: "button", class: "btn small", "data-act": "expand", text: "Expand all" }),
        h("button", { type: "button", class: "btn small", "data-act": "collapse", text: "Collapse all" })
      ]));
      panel.appendChild(h("div", { class: "file-list-head" }, [
        h("span", { text: "Name" }), h("span", { class: "head-count", text: "Files" }), h("span", { class: "head-size", text: "Size" })
      ]));
      panel.appendChild(h("div", { class: "tree-rows", role: "list", "aria-label": "Torrent file tree" }));
      panel.appendChild(h("div", { class: "tree-bottom" }, [
        h("span", { class: "subtext small", "data-status": "" }),
        h("button", { type: "button", class: "btn small", "data-act": "more", hidden: true, text: "Show " + TREE_CHUNK + " more rows" })
      ]));
      input.addEventListener("input", function () {
        clearTimeout(timer);
        timer = setTimeout(function () { setFilter(input.value); }, 100);
      });
      input.addEventListener("keydown", function (ev) { if (ev.key === "Escape") { clearTimeout(timer); input.value = ""; setFilter(""); } });
      render();
    }

    function setFilter(v) {
      var q = v.trim().toLowerCase();
      if (q === state.filter) return;
      if (!q) state.filterClosed.clear();
      state.filter = q;
      state.limit = TREE_CHUNK;
      render();
    }

    function render() {
      var out = visibleRows(tree, state);
      var area = $(".tree-rows", panel);
      var focusedId = doc.activeElement && doc.activeElement.getAttribute ? doc.activeElement.getAttribute("data-id") : null;
      clear(area);
      if (!out.rows.length) {
        area.appendChild(h("div", { class: "files-empty", text: state.filter ? "No file names match this filter." : "This torrent has no files." }));
      }
      var frag = doc.createDocumentFragment();
      out.rows.forEach(function (r) { frag.appendChild(treeRow(r)); });
      area.appendChild(frag);
      var text = (state.filter ? out.found.toLocaleString("en-US") + " matching " + (out.found === 1 ? "file" : "files") + " · " : "") +
        out.rows.length.toLocaleString("en-US") + " visible " + (out.rows.length === 1 ? "row" : "rows");
      $("[data-status]", panel).textContent = text;
      $('[data-act="more"]', panel).hidden = !out.truncated;
      $('[data-act="clear"]', panel).hidden = !state.filter;
      if (focusedId !== null) {
        var again = $('[data-id="' + focusedId + '"]', area);
        if (again) again.focus({ preventScroll: true });
      }
      if (state.filter) announce(text);
    }

    root.addEventListener("click", function (ev) {
      var t = ev.target.closest ? ev.target.closest("button") : null;
      if (!t || !root.contains(t)) return;
      var act = t.getAttribute("data-act");
      if (act === "retry") { load(); return; }
      if (!tree) return;
      if (t.hasAttribute("data-id")) {
        var id = Number(t.getAttribute("data-id"));
        var set = state.filter ? state.filterClosed : state.expanded;
        var open = state.filter ? !state.filterClosed.has(id) : state.expanded.has(id);
        // Filtering tracks collapsed folders, normal browsing tracks expanded ones;
        // the manual expansion survives a filter session untouched.
        if (open === !!state.filter) set.add(id); else set.delete(id);
        render();
      } else if (act === "expand") {
        state.filterClosed.clear();
        tree.nodes.forEach(function (n) { if (n.type === "dir") state.expanded.add(n.id); });
        state.limit = TREE_CHUNK;
        render();
      } else if (act === "collapse") {
        state.expanded.clear();
        state.filterClosed.clear();
        if (state.filter) tree.nodes.forEach(function (n) { if (n.type === "dir") state.filterClosed.add(n.id); });
        state.limit = TREE_CHUNK;
        render();
      } else if (act === "clear") {
        var input = $("[data-filter]", panel);
        clearTimeout(timer);
        input.value = "";
        setFilter("");
        input.focus();
      } else if (act === "more") {
        state.limit += TREE_CHUNK;
        render();
      }
    });
    load();
    return { destroy: function () { clearTimeout(timer); if (holder.ctl) holder.ctl.abort(); } };
  }

  /* ---- Mode B: directory by directory ------------------------------------ */

  var PATH_RE = /^(0|[1-9][0-9]{0,5})(_(0|[1-9][0-9]{0,5}))*$/;
  var OFFSET_RE = /^(0|[1-9][0-9]{0,8})$/;
  var STATE_KEY = "magneticoFiles";
  var MAX_DEPTH = 256;

  function readLocation(loc) {
    var q = new URLSearchParams((loc || location).search), dir = q.get("dir") || "", off = q.get("offset") || "0";
    if (dir !== "" && (!PATH_RE.test(dir) || dir.split("_").length > MAX_DEPTH)) { dir = ""; off = "0"; }
    if (!OFFSET_RE.test(off)) off = "0";
    return { path: dir, offset: Number(off) };
  }

  /* writeLocation edits only dir and offset: other parameters, the fragment
   * and unrelated history state survive, and no history entry is added. */
  function writeLocation(path, offset, env) {
    env = env || { location: location, history: history };
    try {
      var u = new URL(env.location.href);
      if (path) u.searchParams.set("dir", path); else u.searchParams.delete("dir");
      if (offset > 0) u.searchParams.set("offset", String(offset)); else u.searchParams.delete("offset");
      var hs = env.history, st = hs.state && typeof hs.state === "object" ? Object.assign({}, hs.state) : {};
      st[STATE_KEY] = { path: path, offset: offset };
      hs.replaceState(st, "", u.pathname + u.search + u.hash);
    } catch (e) { /* history unavailable: navigation still works in memory */ }
  }

  function runDirs(root, ctx) {
    var holder = {}, seq = 0;
    var loc = readLocation(ctx.env.location);
    var cur = { path: loc.path, offset: loc.offset, data: null, status: "loading", failure: null, requested: null };
    var back = [];   // offsets of the earlier pages seen in this view; empty after a reload
    var restored = loc.path !== "" || loc.offset > 0;
    var notice = "";
    var panel = h("div");
    root.appendChild(panel);
    var announce = liveRegion(root);

    function dirUrl(path, offset) {
      var q = [];
      if (path) q.push("dir=" + path);
      if (offset > 0) q.push("offset=" + offset);
      q.push("limit=" + PAGE_LIMIT);
      return ctx.dirUrl + "?" + q.join("&");
    }

    /* reach is set when stepping back: the page must extend to at least that
     * offset, otherwise entries between its end and the page we left would be
     * skipped. Pages are byte-limited, so a page can hold fewer entries than
     * the limit; the start is then moved closer until the ranges meet. */
    function go(path, offset, reach) {
      var mine = ++seq;
      cur.status = "loading"; cur.failure = null;
      cur.requested = { path: path, offset: offset, reach: reach };
      draw(false);
      ctx.request(dirUrl(path, offset), holder).then(function (res) {
        if (mine !== seq) return;
        if (res.status !== 200 || res.bad || !res.body) {
          var f = failure(res);
          if (f.notFound && restored) {
            restored = false;
            notice = "That location is no longer available; showing the top level.";
            back = [];
            go("", 0);
            return;
          }
          cur.status = "error"; cur.failure = f;
          draw(false);
          return;
        }
        var got = res.body.entries || [];
        if (restored && offset > 0 && !got.length) {
          restored = false;
          notice = "That location is no longer available; showing the top level.";
          back = [];
          go("", 0);
          return;
        }
        if (reach !== undefined && got.length && res.body.next_offset != null && res.body.next_offset < reach) {
          go(path, reach - got.length, reach);
          return;
        }
        restored = false;
        cur.status = "ready"; cur.data = res.body; cur.path = path; cur.offset = offset;
        writeLocation(path, offset, ctx.env);
        draw(true);
        var entries = res.body.entries || [];
        announce("Directory loaded: " + (entries.length ? "entries " + (offset + 1) + " to " + (offset + entries.length) : "no entries") +
          " of " + (res.body.dir ? res.body.dir.entry_count : 0) + ".");
      }, function (err) {
        if (mine !== seq || err.kind === "aborted") return;
        cur.status = "error"; cur.failure = failure(null, err);
        draw(false);
      });
    }

    function draw(focusCrumbs) {
      var d = cur.data, ready = cur.status === "ready";
      clear(panel);
      if (notice) panel.appendChild(h("div", { class: "file-warning", role: "status" }, [icon("info"), h("div", {}, [h("p", { text: notice })])]));
      var ancestors = d ? d.ancestors || [] : [];
      var crumbs = [h("button", { type: "button", "data-path": "", "aria-current": ready && !ancestors.length ? "location" : false }, ["Root"])];
      ancestors.forEach(function (a, i) {
        crumbs.push(icon("right"));
        crumbs.push(h("button", { type: "button", "data-path": a.path, "aria-current": ready && i === ancestors.length - 1 ? "location" : false },
          [nameNode(entrySegments(a, NAME_DISPLAY_BYTES))]));
      });
      panel.appendChild(h("nav", { class: "file-breadcrumbs", "aria-label": "Current file directory", tabindex: "-1", "data-crumbs": "" }, crumbs));
      var parent = ancestors.length ? (ancestors.length > 1 ? ancestors[ancestors.length - 2].path : "") : null;
      panel.appendChild(h("div", { class: "browser-tooling server-tooling" }, [
        h("div", { class: "subtext small", text: ancestors.length ? "Entries in the selected directory" : "Choose a directory to browse" }),
        parent !== null ? h("button", { type: "button", class: "btn small ghost", "data-path": parent }, [icon("left"), " Up one level"]) : null
      ]));
      if (cur.status === "loading") { panel.appendChild(loadingBlock("Loading directory…")); return; }
      if (cur.status === "error") { panel.appendChild(errorBlock(cur.failure, "retry")); return; }
      var entries = d.entries || [];
      panel.appendChild(h("div", { class: "file-list-head" }, [h("span", { text: "Name" }), h("span", { class: "head-count", text: "Files" }), h("span", { class: "head-size", text: "Size" })]));
      var rows = h("div", { class: "tree-rows server-rows", role: "list", "aria-label": "Directory entries" });
      if (!entries.length) rows.appendChild(h("div", { class: "files-empty", text: "No entries in this directory." }));
      entries.forEach(function (e) {
        var segs = entrySegments(e, NAME_DISPLAY_BYTES);
        if (e.type === "dir") {
          rows.appendChild(h("div", { class: "tree-row", role: "listitem" }, [
            h("div", { class: "tree-leaf" }, [icon("folder"), h("button", { type: "button", class: "server-folder-link", "data-path": e.path }, [nameNode(segs)])]),
            h("span", { class: "file-count", text: countText(e.file_count) }), h("span", { class: "file-size", text: formatSize(e.size) })
          ]));
        } else {
          rows.appendChild(h("div", { class: "tree-row", role: "listitem" }, [
            h("div", { class: "tree-leaf" }, [icon("file", "file-icon"), nameNode(segs, hasMarker(segs) ? "unusual-path" : "")].concat(tags(segs, e.duplicate))),
            h("span", { class: "file-count" }), h("span", { class: "file-size", text: formatSize(e.size) })
          ]));
        }
      });
      panel.appendChild(rows);
      var total = d.dir ? d.dir.entry_count : entries.length;
      var from = entries.length ? cur.offset + 1 : 0, to = cur.offset + entries.length;
      var hasPrev = cur.offset > 0, hasNext = d.next_offset !== null && d.next_offset !== undefined;
      panel.appendChild(h("div", { class: "tree-bottom" }, [
        h("span", { class: "subtext small", text: "Showing " + from + "–" + to + " of " + total.toLocaleString("en-US") + " entries" }),
        h("div", { class: "pagination" }, [
          h("button", { type: "button", class: "btn small", "data-page": "prev", disabled: !hasPrev }, [icon("left"), " Previous"]),
          h("button", { type: "button", class: "btn small", "data-page": "next", disabled: !hasNext }, ["Next ", icon("right")])
        ])
      ]));
      if (focusCrumbs) { var c = $("[data-crumbs]", panel); if (c) c.focus({ preventScroll: true }); }
    }

    root.addEventListener("click", function (ev) {
      var t = ev.target.closest ? ev.target.closest("button") : null;
      if (!t || !root.contains(t)) return;
      if (t.hasAttribute("data-path")) {
        notice = ""; back = [];
        go(t.getAttribute("data-path"), 0);
      } else if (t.getAttribute("data-page") === "next" && cur.data && cur.data.next_offset != null) {
        back.push(cur.offset); notice = "";
        go(cur.path, cur.data.next_offset);
      } else if (t.getAttribute("data-page") === "prev") {
        notice = "";
        if (back.length) go(cur.path, back.pop());
        else go(cur.path, Math.max(0, cur.offset - PAGE_LIMIT), cur.offset);
      } else if (t.getAttribute("data-act") === "retry") {
        var r = cur.requested || { path: cur.path, offset: cur.offset };
        go(r.path, r.offset, r.reach);
      }
    });

    go(loc.path, loc.offset);
    return { destroy: function () { seq++; if (holder.ctl) holder.ctl.abort(); } };
  }

  /* ---- file browser entry point ------------------------------------------ */

  function initFileBrowser(root, opts) {
    opts = opts || {};
    var request = makeRequester(opts);
    var ctx = {
      treeUrl: root.getAttribute("data-tree-url"), dirUrl: root.getAttribute("data-dir-url"), request: request,
      files: Number(root.getAttribute("data-files")) || 0,
      env: { location: opts.location || location, history: opts.history || history },
      switchToDirs: function () { current && current.destroy(); clear(root); current = runDirs(root, ctx); }
    };
    var current = null;
    clear(root); // drops the no-JavaScript notice
    current = root.getAttribute("data-mode") === "tree" ? runTree(root, ctx) : runDirs(root, ctx);
    return { destroy: function () { current && current.destroy(); } };
  }

  /* ---- theme, copy, small conveniences ----------------------------------- */

  var THEMES = ["system", "light", "dark"];
  var THEME_ICON = { system: "monitor", light: "sun", dark: "moon" };

  function currentTheme() {
    var t = doc.documentElement.getAttribute("data-theme");
    return t === "light" || t === "dark" ? t : "system";
  }

  function applyTheme(t) {
    if (t === "system") doc.documentElement.removeAttribute("data-theme");
    else doc.documentElement.setAttribute("data-theme", t);
    try {
      if (t === "system") localStorage.removeItem("magnetico-theme"); else localStorage.setItem("magnetico-theme", t);
    } catch (e) { /* storage blocked: the choice lasts for this page only */ }
    var b = $("#theme-toggle");
    if (!b) return;
    var label = t.charAt(0).toUpperCase() + t.slice(1);
    b.setAttribute("aria-label", "Theme: " + t + ". Activate to change.");
    b.setAttribute("title", "Theme: " + t);
    var use = $("use", b), txt = $(".theme-label", b);
    if (use) use.setAttribute("href", "#i-" + THEME_ICON[t]);
    if (txt) txt.textContent = label;
  }

  var toastTimer = null;
  function toast(msg) {
    var t = $("#toast");
    if (!t) return;
    t.textContent = msg;
    t.classList.add("visible");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.classList.remove("visible"); }, 2600);
  }

  function copyText(value) {
    if (navigator.clipboard && navigator.clipboard.writeText) return navigator.clipboard.writeText(value);
    return new Promise(function (resolve, reject) {
      var ta = h("textarea", { "aria-hidden": "true", readonly: true, tabindex: "-1" });
      ta.value = value;
      doc.body.appendChild(ta);
      ta.select();
      var ok = false;
      try { ok = doc.execCommand("copy"); } catch (e) { ok = false; }
      doc.body.removeChild(ta);
      ok ? resolve() : reject(new Error("copy failed"));
    });
  }

  function initPage() {
    var tb = $("#theme-toggle");
    if (tb) {
      tb.hidden = false;
      applyTheme(currentTheme());
      tb.addEventListener("click", function () {
        applyTheme(THEMES[(THEMES.indexOf(currentTheme()) + 1) % THEMES.length]);
      });
    }
    $$("[data-copy]").forEach(function (b) {
      b.hidden = false;
      b.addEventListener("click", function () {
        copyText(b.getAttribute("data-copy")).then(function () { toast(b.getAttribute("data-copied") || "Copied"); },
          function () { toast("Copy failed. Select the text and copy it manually."); });
      });
    });
    $$("select[data-autosubmit]").forEach(function (s) {
      s.addEventListener("change", function () { if (s.form) s.form.submit(); });
    });
    doc.addEventListener("keydown", function (ev) {
      if (ev.key !== "/" || ev.ctrlKey || ev.metaKey || ev.altKey) return;
      var tag = ev.target && ev.target.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || (ev.target && ev.target.isContentEditable)) return;
      var q = $('input[type="search"][name="q"]');
      if (q) { ev.preventDefault(); q.focus(); q.select(); }
    });
    var fb = $("#file-browser");
    if (fb) initFileBrowser(fb, {});
  }

  if (doc.documentElement.getAttribute("data-selftest") === "1") {
    window.MagneticoUI = {
      segmentsOfBytes: segmentsOfBytes, base64Bytes: base64Bytes, entrySegments: entrySegments, buildTree: buildTree,
      formatSize: formatSize, initFileBrowser: initFileBrowser, readLocation: readLocation, writeLocation: writeLocation
    };
  } else if (doc.readyState === "loading") {
    doc.addEventListener("DOMContentLoaded", initPage);
  } else {
    initPage();
  }
})();
