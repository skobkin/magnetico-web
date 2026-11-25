/* Charts of the administrator section: first-party vanilla JS over the vendored
   uPlot. It draws what /admin/api/series returns and nothing else: no HTML string
   injection (every dynamic string goes through textContent), styles only through
   the CSSOM, one fetch call site, same origin. The pure functions at the top are
   also exercised by internal/web/ui/jstest (run with Node, see docs/testing.md). */
(function (root) {
  "use strict";

  // ---- pure logic -------------------------------------------------------------

  var NO_DATA = "No data for this period.";

  // hasData: at least one value is present (a gap is null, a measured zero is 0).
  function hasData(series) {
    var lines = (series && series.lines) || [];
    for (var i = 0; i < lines.length; i++) {
      var v = lines[i].values || [];
      for (var j = 0; j < v.length; j++) if (v[j] !== null && v[j] !== undefined) return true;
    }
    return false;
  }

  // bucketSeconds converts RFC 3339 bucket starts to unix seconds.
  function bucketSeconds(series) {
    return (series.buckets || []).map(function (b) { return Math.floor(Date.parse(b) / 1000); });
  }

  // groupsOf splits the lines into plots, in order of first appearance.
  function groupsOf(series) {
    var order = [], by = {};
    (series.lines || []).forEach(function (l) {
      var g = l.group || l.unit || "";
      if (!by[g]) { by[g] = []; order.push(g); }
      by[g].push(l);
    });
    return order.map(function (g) { return { title: g, lines: by[g] }; });
  }

  // plotData builds uPlot's column data for one group. Gaps stay null (uPlot
  // draws a break); stacked lines are cumulative sums over the stacked lines
  // before them and stay null where their own value is missing.
  function plotData(xs, lines) {
    var cols = [xs], running = new Array(xs.length).fill(0);
    lines.forEach(function (l) {
      var col = new Array(xs.length);
      for (var i = 0; i < xs.length; i++) {
        var v = l.values ? l.values[i] : null;
        if (v === null || v === undefined) { col[i] = null; continue; }
        if (l.stack) { running[i] += v; col[i] = running[i]; } else col[i] = v;
      }
      cols.push(col);
    });
    return cols;
  }

  // summaryText is the plain-text rendering of a series (the no-JavaScript table):
  // tab-separated, one row per bucket, "–" for a gap, "(partial)" for a partial bucket.
  function summaryText(series) {
    var rows = [["period"].concat((series.lines || []).map(function (l) { return l.label; })).join("\t")];
    (series.buckets || []).forEach(function (b, i) {
      var label = periodLabel(series.granularity, b) + (series.partial && series.partial[i] ? " (partial)" : "");
      var cells = (series.lines || []).map(function (l) {
        var v = l.values ? l.values[i] : null;
        return v === null || v === undefined ? "–" : fmtNumber(v);
      });
      rows.push([label].concat(cells).join("\t"));
    });
    return rows.join("\n") + "\n";
  }

  function periodLabel(gran, iso) {
    var s = String(iso).replace("T", " ").replace(/:00Z$/, "").replace("Z", "");
    if (gran === "day") return s.slice(0, 16);
    return s.slice(0, gran === "year" ? 7 : 10);
  }

  function fmtNumber(v) {
    if (Math.abs(v) >= 1000) return Math.round(v).toLocaleString("en-US");
    return String(Math.round(v * 1000) / 1000);
  }

  function fmtBytes(v) {
    var units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"], i = 0;
    while (Math.abs(v) >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (i === 0 ? String(Math.round(v)) : v.toFixed(1)) + " " + units[i];
  }

  function formatterFor(unit) {
    if (unit === "bytes") return fmtBytes;
    if (unit === "ratio") return function (v) { return (v * 100).toFixed(0) + " %"; };
    if (unit === "ms") return function (v) { return fmtNumber(v) + " ms"; };
    return fmtNumber;
  }

  // problemMessage maps a failed answer to text that carries no server detail.
  function problemMessage(status) {
    if (status === 404) return NO_DATA;
    if (status === 401 || status === 403) return "You are not signed in as an administrator.";
    if (status === 503) return "The statistics are temporarily unavailable.";
    if (status === 400) return "That period or chart is not valid.";
    return "Could not load the data (HTTP " + status + ").";
  }

  var api = { hasData: hasData, bucketSeconds: bucketSeconds, groupsOf: groupsOf, plotData: plotData,
    summaryText: summaryText, formatterFor: formatterFor, problemMessage: problemMessage, fmtBytes: fmtBytes, NO_DATA: NO_DATA };
  if (typeof module === "object" && module && module.exports) { module.exports = api; }
  if (typeof document === "undefined" || !document.querySelectorAll) return;

  // ---- DOM ----------------------------------------------------------------------

  var doc = document, GRANS = [["day", "Day"], ["month", "Month"], ["year", "Year"]];

  function el(tag, cls, text) {
    var e = doc.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  function cssVar(name, fallback) {
    var v = getComputedStyle(doc.documentElement).getPropertyValue(name).trim();
    return v || fallback;
  }

  function rgba(hex, a) {
    var m = /^#([0-9a-f]{6})$/i.exec(hex);
    if (!m) return hex;
    var n = parseInt(m[1], 16);
    return "rgba(" + (n >> 16) + "," + ((n >> 8) & 255) + "," + (n & 255) + "," + a + ")";
  }

  function palette() {
    var out = [];
    for (var i = 1; i <= 6; i++) out.push(cssVar("--chart-" + i, "#4c9fd6"));
    return out;
  }

  function Card(node) {
    this.node = node;
    this.chart = node.getAttribute("data-chart");
    this.gran = "day";
    this.period = "";
    this.series = null;
    this.plots = [];
    this.status = node.querySelector(".chart-status");
    this.holder = node.querySelector(".chart-plots");
    this.text = node.querySelector(".chart-text");
    this.hidden = {};
    this.token = 0;
    this.buildControls();
  }

  Card.prototype.buildControls = function () {
    var self = this, bar = this.node.querySelector(".chart-controls");
    if (!bar) return;
    this.buttons = {};
    GRANS.forEach(function (g) {
      var b = el("button", "btn small ghost", g[1]);
      b.type = "button";
      b.addEventListener("click", function () { self.gran = g[0]; self.period = ""; self.load(); });
      self.buttons[g[0]] = b;
      bar.appendChild(b);
    });
    this.prev = el("button", "btn small ghost", "‹ Earlier");
    this.next = el("button", "btn small ghost", "Later ›");
    this.prev.type = this.next.type = "button";
    this.prev.addEventListener("click", function () { if (self.series && self.series.previous) { self.period = self.series.previous; self.load(); } });
    this.next.addEventListener("click", function () { if (self.series && self.series.next) { self.period = self.series.next; self.load(); } });
    this.label = el("span", "chart-period", "");
    bar.appendChild(this.prev);
    bar.appendChild(this.label);
    bar.appendChild(this.next);
  };

  Card.prototype.setStatus = function (text, isError) {
    this.status.textContent = text;
    this.status.className = "chart-status" + (isError ? " error" : "");
    this.status.hidden = !text;
  };

  Card.prototype.destroy = function () {
    this.plots.forEach(function (p) { p.destroy(); });
    this.plots = [];
    while (this.holder.firstChild) this.holder.removeChild(this.holder.firstChild);
  };

  Card.prototype.load = function () {
    var self = this, token = ++this.token;
    var url = "/admin/api/series?chart=" + encodeURIComponent(this.chart) + "&g=" + encodeURIComponent(this.gran) +
      (this.period ? "&period=" + encodeURIComponent(this.period) : "");
    this.setStatus("Loading…", false);
    fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" } }).then(function (resp) {
      if (token !== self.token) return null;
      if (!resp.ok) { self.fail(resp.status); return null; }
      return resp.json();
    }).then(function (data) {
      if (data && token === self.token) self.render(data);
    }).catch(function () {
      if (token === self.token) self.fail(0);
    });
  };

  Card.prototype.fail = function (status) {
    this.destroy();
    this.text.textContent = "";
    var msg = status === 0 ? "Could not reach the server." : problemMessage(status);
    this.setStatus(msg, status !== 404);
  };

  Card.prototype.render = function (series) {
    this.series = series;
    this.destroy();
    this.text.textContent = summaryText(series);
    if (this.buttons) {
      var self = this;
      Object.keys(this.buttons).forEach(function (g) { self.buttons[g].setAttribute("aria-pressed", g === series.granularity ? "true" : "false"); });
      this.prev.disabled = !series.previous;
      this.next.disabled = !series.next;
      this.label.textContent = series.period;
    }
    if (!hasData(series)) { this.setStatus(NO_DATA, false); return; }
    this.setStatus("", false);
    var xs = bucketSeconds(series), colors = palette(), self2 = this, idx = 0;
    groupsOf(series).forEach(function (g) {
      var box = el("div", "chart-plot"), title = el("h4", "chart-plot-title", g.title), canvas = el("div", "chart-canvas");
      box.appendChild(title);
      box.appendChild(canvas);
      self2.holder.appendChild(box);
      self2.plots.push(self2.draw(canvas, series, xs, g, colors, idx));
      idx += g.lines.length;
    });
  };

  Card.prototype.draw = function (canvas, series, xs, group, colors, offset) {
    var unit = group.lines[0].unit, fmt = formatterFor(unit), text = cssVar("--chart-text", "#adbfc5"), grid = cssVar("--chart-grid", "#2b393f");
    var shade = cssVar("--chart-partial", "rgba(128,128,128,0.15)"), partial = series.partial || [], bands = [], uSeries = [{}];
    group.lines.forEach(function (l, i) {
      var c = colors[(offset + i) % colors.length];
      uSeries.push({ label: l.label, stroke: c, width: 2, fill: l.stack ? rgba(c, 0.35) : undefined, points: { show: xs.length <= 40 },
        spanGaps: false, value: function (u, v) { return v === null || v === undefined ? "–" : fmt(v); } });
      if (l.stack && i > 0 && group.lines[i - 1].stack) bands.push({ series: [i + 1, i], fill: rgba(c, 0.35) });
    });
    var width = Math.max(canvas.clientWidth || 320, 280);
    var opts = {
      width: width, height: 220,
      scales: { x: { time: true }, y: { range: unit === "ratio" ? [0, 1] : function (u, min, max) { return [0, max > 0 ? max * 1.08 : 1]; } } },
      axes: [
        { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid } },
        { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid }, size: 70, values: function (u, vals) { return vals.map(function (v) { return fmt(v); }); } }
      ],
      series: uSeries, bands: bands, legend: { live: true },
      hooks: { drawClear: [function (u) {
        var half = xs.length > 1 ? (xs[1] - xs[0]) / 2 : 0, ctx = u.ctx;
        ctx.save();
        ctx.fillStyle = shade;
        partial.forEach(function (isPartial, i) {
          if (!isPartial) return;
          var left = u.valToPos(xs[i] - half, "x", true), right = u.valToPos(xs[i] + half, "x", true);
          ctx.fillRect(left, u.bbox.top, right - left, u.bbox.height);
        });
        ctx.restore();
      }] }
    };
    var plot = new root.uPlot(opts, plotData(xs, group.lines), canvas);
    return plot;
  };

  var cards = [];
  function start() {
    var nodes = doc.querySelectorAll("[data-chart]");
    for (var i = 0; i < nodes.length; i++) cards.push(new Card(nodes[i]));
    cards.forEach(function (c) { c.load(); });
  }
  function retheme() { cards.forEach(function (c) { if (c.series) c.render(c.series); }); }

  if (doc.readyState === "loading") doc.addEventListener("DOMContentLoaded", start); else start();
  if (root.matchMedia) {
    var mq = root.matchMedia("(prefers-color-scheme: light)");
    if (mq.addEventListener) mq.addEventListener("change", retheme);
  }
  if (root.MutationObserver) new root.MutationObserver(retheme).observe(doc.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  var resizeTimer = 0;
  root.addEventListener("resize", function () { root.clearTimeout(resizeTimer); resizeTimer = root.setTimeout(retheme, 200); });
})(typeof window !== "undefined" ? window : this);
