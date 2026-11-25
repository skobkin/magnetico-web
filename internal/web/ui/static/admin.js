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
      var ns = series.notes && series.notes[i];
      if (ns && ns.length) label += " [" + ns.join(", ") + "]";
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

  // ---- bars, defaults, notes, freshness (pure) ----------------------------------

  // KIND_OF says how a chart draws its lines: the throughput chart as grouped bars
  // (one bar per series in every bucket), the outcome chart's mutually exclusive
  // settlements as stacked bars, everything else as lines.
  var GROUPED_CHARTS = { throughput: true };

  // visibleByDefault: the throughput chart opens with the series that show a
  // backlog growing (enqueued against settled); received and acknowledged are one
  // click away in the legend. Every other line is visible.
  function visibleByDefault(chart, key) {
    if (chart === "throughput") return key === "enqueued" || key === "settled";
    return true;
  }

  // lineKind: "grouped", "stacked" or "line" for one line of a series.
  function lineKind(series, line) {
    if (series && GROUPED_CHARTS[series.chart] && !line.stack) return "grouped";
    if (line.stack && series && series.chart === "outcomes") return "stacked";
    return line.stack ? "area" : "line";
  }

  // bucketEnds: the end (unix seconds, exclusive) of every bucket, in UTC.
  function bucketEnds(series) {
    return (series.buckets || []).map(function (b) {
      var d = new Date(Date.parse(b));
      if (series.granularity === "day") d.setUTCHours(d.getUTCHours() + 1);
      else if (series.granularity === "month") d.setUTCDate(d.getUTCDate() + 1);
      else d.setUTCMonth(d.getUTCMonth() + 1);
      return Math.floor(d.getTime() / 1000);
    });
  }

  // barCentres: the x values of a bar plot, so that the cursor snaps to the bar
  // it is over rather than to the nearest bucket start.
  function barCentres(xs, ends) {
    return xs.map(function (x, i) { return Math.floor((x + ends[i]) / 2); });
  }

  // groupedSlot: horizontal position and width (same unit as left/right) of bar
  // `index` of `count` inside a bucket [left, right); 10 % margin on each side.
  function groupedSlot(left, right, count, index) {
    var span = right - left, inner = span * 0.8, slot = inner / Math.max(count, 1);
    return [left + span * 0.1 + slot * index, slot * 0.9];
  }

  // stackedSlot: one bar over the middle 80 % of the bucket.
  function stackedSlot(left, right) {
    var span = right - left;
    return [left + span * 0.1, span * 0.8];
  }

  // stackBases: for each stacked line, the cumulative total below it in every
  // bucket (a missing value adds nothing and leaves its own bar missing).
  function stackBases(lines, n) {
    var running = new Array(n).fill(0), out = {};
    lines.forEach(function (l, li) {
      if (!l.stack) return;
      var base = new Array(n);
      for (var i = 0; i < n; i++) {
        base[i] = running[i];
        var v = l.values ? l.values[i] : null;
        if (v !== null && v !== undefined) running[i] += v;
      }
      out[li] = base;
    });
    return out;
  }

  var NOTE_TEXT = {
    approximate_counts: "queue or quarantine row counts are estimates in",
    incomplete_breakdown: "the storage breakdown is incomplete (a measure was unavailable), so no stack is drawn, in",
    reconciliation_mismatch: "the storage categories did not add up to the total in",
    reconciliation_not_evaluated: "the storage categories could not be checked against the total (a measure was unavailable) in"
  };

  // notesText: one sentence per kind of note, with the number of buckets it
  // concerns; "" when no bucket carries a note. Unknown codes are ignored.
  function notesText(series) {
    var counts = {}, order = [];
    (series.notes || []).forEach(function (ns) {
      (ns || []).forEach(function (n) {
        if (!NOTE_TEXT[n]) return;
        if (!counts[n]) { counts[n] = 0; order.push(n); }
        counts[n]++;
      });
    });
    return order.map(function (n) {
      return "Note: " + NOTE_TEXT[n] + " " + counts[n] + (counts[n] === 1 ? " bucket." : " buckets.");
    }).join(" ");
  }

  // freshnessCaption says when the data was read and, if the refresh failed, that
  // it is the last known state. The time is the server's, in UTC.
  function freshnessCaption(f) {
    if (!f || !f.fetched_at) return { text: "", stale: false };
    var t = String(f.fetched_at).replace("T", " ").replace(/Z$/, "").slice(0, 19) + " UTC";
    if (f.stale) return { text: "The database could not be read just now: last known data, read at " + t + ".", stale: true };
    return { text: "Data read at " + t + ".", stale: false };
  }

  // utcTzDate gives uPlot a clock that is UTC whatever the browser's time zone is.
  function utcTzDate(uplot) {
    return function (ts) { return uplot.tzDate(new Date(ts * 1e3), "Etc/UTC"); };
  }

  var api = { hasData: hasData, bucketSeconds: bucketSeconds, groupsOf: groupsOf, plotData: plotData,
    summaryText: summaryText, formatterFor: formatterFor, visibleByDefault: visibleByDefault, lineKind: lineKind, bucketEnds: bucketEnds,
    barCentres: barCentres, groupedSlot: groupedSlot, stackedSlot: stackedSlot, stackBases: stackBases, notesText: notesText,
    freshnessCaption: freshnessCaption, utcTzDate: utcTzDate, periodLabel: periodLabel, problemMessage: problemMessage, fmtBytes: fmtBytes, NO_DATA: NO_DATA };
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
    this.asof = node.querySelector(".chart-asof");
    this.notes = node.querySelector(".chart-notes");
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

  // showMeta states when the data was read, that it is the last known state after a
  // failed refresh, and which buckets carry a note: cached data never reads as current.
  Card.prototype.showMeta = function (series) {
    var f = freshnessCaption(series.freshness), n = notesText(series);
    if (this.asof) {
      this.asof.textContent = f.text;
      this.asof.className = "chart-asof" + (f.stale ? " stale" : "");
      this.asof.hidden = !f.text;
    }
    if (this.notes) {
      this.notes.textContent = n;
      this.notes.hidden = !n;
    }
  };

  Card.prototype.fail = function (status) {
    this.destroy();
    this.text.textContent = "";
    if (this.asof) { this.asof.textContent = ""; this.asof.hidden = true; }
    if (this.notes) { this.notes.textContent = ""; this.notes.hidden = true; }
    var msg = status === 0 ? "Could not reach the server." : problemMessage(status);
    this.setStatus(msg, status !== 404);
  };

  Card.prototype.render = function (series) {
    this.series = series;
    this.destroy();
    this.text.textContent = summaryText(series);
    this.showMeta(series);
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

  // barPaths returns a uPlot paths function that draws one series as bars over its
  // bucket: grouped (slot `index` of `count` in the bucket) or stacked (from the
  // base below to its cumulative value). A missing value draws nothing.
  function barPaths(kind, starts, ends, count, index, bases) {
    return function (u, seriesIdx) {
      var ys = u.data[seriesIdx], path = new root.Path2D(), zero = u.valToPos(0, "y", true);
      for (var i = 0; i < starts.length; i++) {
        var v = ys[i];
        if (v === null || v === undefined) continue;
        var left = u.valToPos(starts[i], "x", true), right = u.valToPos(ends[i], "x", true);
        var slot = kind === "stacked" ? stackedSlot(left, right) : groupedSlot(left, right, count, index);
        var top = u.valToPos(v, "y", true), bottom = bases ? u.valToPos(bases[i], "y", true) : zero;
        path.rect(slot[0], Math.min(top, bottom), Math.max(slot[1], 1), Math.abs(bottom - top));
      }
      return { stroke: path, fill: path };
    };
  }

  Card.prototype.draw = function (canvas, series, xs, group, colors, offset) {
    var unit = group.lines[0].unit, fmt = formatterFor(unit), text = cssVar("--chart-text", "#adbfc5"), grid = cssVar("--chart-grid", "#2b393f");
    var shade = cssVar("--chart-partial", "rgba(128,128,128,0.15)"), partial = series.partial || [];
    var ends = bucketEnds(series), starts = xs;
    var kinds = group.lines.map(function (l) { return lineKind(series, l); });
    var barred = kinds.some(function (k) { return k === "grouped" || k === "stacked"; });
    var plotXs = barred ? barCentres(xs, ends) : xs;
    var bases = stackBases(group.lines, xs.length);
    var groupedCount = kinds.filter(function (k) { return k === "grouped"; }).length, groupedIndex = 0;
    var uSeries = [barred ? { value: function (u, v, si, i) { return i === null || i === undefined ? "–" : periodLabel(series.granularity, series.buckets[i]) + (partial[i] ? " (partial)" : ""); } } : {}], bands = [];
    group.lines.forEach(function (l, i) {
      var c = colors[(offset + i) % colors.length], kind = kinds[i];
      var o = { label: l.label, stroke: c, width: 2, show: visibleByDefault(series.chart, l.key), spanGaps: false,
        value: function (u, v) { return v === null || v === undefined ? "–" : fmt(v); } };
      if (kind === "grouped") {
        o.fill = rgba(c, 0.75); o.width = 1; o.points = { show: false };
        o.paths = barPaths("grouped", starts, ends, groupedCount, groupedIndex++, null);
      } else if (kind === "stacked") {
        o.fill = rgba(c, 0.75); o.width = 1; o.points = { show: false };
        o.paths = barPaths("stacked", starts, ends, 1, 0, bases[i]);
      } else {
        o.fill = kind === "area" ? rgba(c, 0.35) : undefined;
        o.points = { show: xs.length <= 40 };
      }
      uSeries.push(o);
      if (kind === "area" && i > 0 && group.lines[i - 1].stack) bands.push({ series: [i + 1, i], fill: rgba(c, 0.35) });
    });
    var width = Math.max(canvas.clientWidth || 320, 280);
    var xRange = [xs[0], ends[ends.length - 1]];
    var opts = {
      width: width, height: 220,
      // the clock of every axis, tick and legend is UTC, whatever the browser's zone is
      tzDate: root.uPlot && root.uPlot.tzDate ? utcTzDate(root.uPlot) : undefined,
      scales: { x: { time: true, range: function () { return xRange; } },
        y: { range: unit === "ratio" ? [0, 1] : function (u, min, max) { return [0, max > 0 ? max * 1.08 : 1]; } } },
      axes: [
        { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid } },
        { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid }, size: 70, values: function (u, vals) { return vals.map(function (v) { return fmt(v); }); } }
      ],
      series: uSeries, bands: bands, legend: { live: true },
      hooks: { drawClear: [function (u) {
        var ctx = u.ctx;
        ctx.save();
        ctx.fillStyle = shade;
        partial.forEach(function (isPartial, i) {
          if (!isPartial) return;
          var left = u.valToPos(starts[i], "x", true), right = u.valToPos(ends[i], "x", true);
          ctx.fillRect(left, u.bbox.top, right - left, u.bbox.height);
        });
        ctx.restore();
      }] }
    };
    return new root.uPlot(opts, plotData(plotXs, group.lines), canvas);
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
