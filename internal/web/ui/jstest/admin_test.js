// Self-tests of internal/web/ui/static/admin.js. Run with Node (no packages):
//   docker run --rm -v "$PWD":/w -w /w node:22-alpine node internal/web/ui/jstest/admin_test.js
// (or `node internal/web/ui/jstest/admin_test.js` with a local Node). They are
// not part of `go test`: the repository has no JavaScript toolchain; the Go suite
// pins the source invariants (no innerHTML, one fetch, CSP-safe) statically.
"use strict";
const assert = require("assert");
const path = require("path");

let failed = 0;
function test(name, fn) {
  try { fn(); console.log("ok   " + name); } catch (e) { failed++; console.log("FAIL " + name + "\n     " + (e && e.stack || e)); }
}
const script = path.join(__dirname, "..", "static", "admin.js");

// ---- pure functions (no DOM present while loading) ----
const pure = require(script);

const series = {
  chart: "throughput", granularity: "day", period: "2026-11-24",
  buckets: ["2026-11-24T00:00:00Z", "2026-11-24T01:00:00Z", "2026-11-24T02:00:00Z", "2026-11-24T03:00:00Z"],
  partial: [false, true, false, false],
  lines: [
    { key: "a", label: "Alpha", unit: "records", group: "G1", values: [5, null, 0, 12345.678] },
    { key: "b", label: "Beta", unit: "records", group: "G1", stack: true, values: [1, 2, null, 4] },
    { key: "c", label: "Gamma", unit: "records", group: "G1", stack: true, values: [10, null, 3, 4] },
    { key: "r", label: "Rate", unit: "ratio", group: "G2", values: [0.5, null, null, null] },
  ],
};

test("nullBucketsRenderAsGaps", () => {
  const xs = pure.bucketSeconds(series);
  assert.deepStrictEqual(xs, [1779580800, 1779584400, 1779588000, 1779591600].map((_, i) => Date.parse(series.buckets[i]) / 1000));
  const g = pure.groupsOf(series)[0];
  const cols = pure.plotData(xs, g.lines);
  assert.strictEqual(cols[1][1], null, "a missing bucket is null, never 0");
  assert.strictEqual(cols[1][2], 0, "a measured zero stays 0");
  assert.strictEqual(cols[3][1], null, "a stacked line is null where its own value is missing");
});

test("stackedLinesAreCumulative", () => {
  const g = pure.groupsOf(series)[0];
  const cols = pure.plotData(pure.bucketSeconds(series), g.lines);
  assert.deepStrictEqual(cols[2], [1, 2, null, 4], "the first stacked line starts at zero");
  assert.deepStrictEqual(cols[3], [11, null, 3, 8], "the next stacked line adds the previous one");
});

test("groupsKeepOrderAndNeverMixUnits", () => {
  const groups = pure.groupsOf(series);
  assert.deepStrictEqual(groups.map((g) => g.title), ["G1", "G2"]);
  assert.strictEqual(groups[1].lines[0].unit, "ratio");
});

test("noDataStateShown", () => {
  assert.strictEqual(pure.hasData({ lines: [{ values: [null, null] }, { values: [] }] }), false);
  assert.strictEqual(pure.hasData({ lines: [{ values: [null, 0] }] }), true, "a measured zero is data");
  assert.strictEqual(pure.hasData({}), false);
  assert.strictEqual(pure.problemMessage(404), pure.NO_DATA);
});

test("errorStateShown", () => {
  for (const s of [0, 400, 401, 403, 500, 503]) {
    const m = pure.problemMessage(s);
    assert.ok(m.length > 5 && m.length < 100, "short message for " + s);
    assert.ok(!/sql|pgx|select|stack|error:/i.test(m), "no internals in the message for " + s);
  }
  assert.ok(pure.problemMessage(403).includes("administrator"));
});

test("summaryTableMatchesSeries", () => {
  const rows = pure.summaryText(series).trimEnd().split("\n");
  assert.strictEqual(rows.length, series.buckets.length + 1, "one row per bucket plus the header");
  assert.strictEqual(rows[0], "period\tAlpha\tBeta\tGamma\tRate");
  const cells = rows.slice(1).map((r) => r.split("\t"));
  assert.ok(cells[1][0].endsWith("(partial)") && !cells[0][0].includes("partial"));
  assert.strictEqual(cells[1][1], "–", "a gap is a dash");
  assert.strictEqual(cells[2][1], "0", "a measured zero is 0");
  assert.strictEqual(cells[3][1], "12,346");
  assert.strictEqual(cells[0][4], "0.5");
  for (const row of cells) assert.strictEqual(row.length, 5);
});

test("formatters", () => {
  assert.strictEqual(pure.fmtBytes(1536), "1.5 KiB");
  assert.strictEqual(pure.formatterFor("ratio")(0.25), "25 %");
  assert.strictEqual(pure.formatterFor("bytes")(0), "0 B");
});

// ---- the DOM part, against a tiny fake DOM ----
class FakeElement {
  constructor(tag) {
    this.tag = tag; this.children = []; this.attrs = {}; this.listeners = {}; this._text = ""; this.className = ""; this.hidden = false;
    this.style = {}; this.clientWidth = 600;
    const self = this;
    Object.defineProperty(this, "innerHTML", { set() { throw new Error("innerHTML was assigned"); }, get() { return ""; } });
    Object.defineProperty(this, "outerHTML", { set() { throw new Error("outerHTML was assigned"); } });
    Object.defineProperty(this, "textContent", { get() { return self._text + self.children.map((c) => c.textContent).join(""); },
      set(v) { self._text = String(v); self.children = []; } });
  }
  get firstChild() { return this.children[0] || null; }
  appendChild(c) { this.children.push(c); return c; }
  removeChild(c) { this.children = this.children.filter((x) => x !== c); return c; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k] === undefined ? null : this.attrs[k]; }
  addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); }
  querySelector(sel) { return this.found[sel] || null; }
}

function runDom(responder) {
  const created = [];
  const card = new FakeElement("article");
  card.attrs["data-chart"] = "throughput";
  card.found = { ".chart-status": new FakeElement("p"), ".chart-plots": new FakeElement("div"), ".chart-text": new FakeElement("pre"), ".chart-controls": new FakeElement("div") };
  const plots = [];
  const sandbox = {
    document: {
      readyState: "complete",
      documentElement: new FakeElement("html"),
      querySelectorAll: (sel) => (sel === "[data-chart]" ? [card] : []),
      createElement: (tag) => { const e = new FakeElement(tag); created.push(e); return e; },
      addEventListener() {},
    },
    getComputedStyle: () => ({ getPropertyValue: () => "" }),
    fetch: responder,
    uPlot: function (opts, data, target) { plots.push({ opts, data, target }); this.destroy = () => {}; },
    addEventListener() {}, clearTimeout() {}, setTimeout() { return 0; },
  };
  sandbox.window = sandbox;
  const src = require("fs").readFileSync(script, "utf8");
  new Function(...Object.keys(sandbox), "module", src).call(sandbox, ...Object.values(sandbox), undefined);
  return { card, plots, created };
}

const tick = () => new Promise((r) => setImmediate(r));

async function domTests() {
  const hostile = { chart: "x", granularity: "day", period: "2026-11-24", buckets: series.buckets, partial: series.partial,
    previous: "2026-11-23", lines: [{ key: "h", label: "<img src=x onerror=alert(1)>", unit: "records", group: "<script>alert(2)</script>", values: [1, null, 2, 3] }] };
  let env = runDom(() => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(hostile) }));
  await tick(); await tick();
  test("untrustedStringsAreTextContent", () => {
    assert.ok(env.card.found[".chart-text"].textContent.includes("<img src=x onerror=alert(1)>"), "the label is shown as text");
    assert.strictEqual(env.plots.length, 1, "one plot was drawn");
    assert.strictEqual(env.plots[0].opts.series[1].label, "<img src=x onerror=alert(1)>");
    const title = env.created.find((e) => e.className === "chart-plot-title");
    assert.strictEqual(title.textContent, "<script>alert(2)</script>", "the group title is text, not markup");
    // no element ever received markup (the fake throws on innerHTML) and none carries a style attribute
    assert.ok(env.created.every((e) => e.attrs.style === undefined));
  });

  env = runDom(() => Promise.resolve({ ok: false, status: 503, json: () => Promise.resolve({}) }));
  await tick(); await tick();
  test("errorStateRendersMessageAndNoPlot", () => {
    const st = env.card.found[".chart-status"];
    assert.strictEqual(st.textContent, "The statistics are temporarily unavailable.");
    assert.ok(st.className.includes("error"));
    assert.strictEqual(env.plots.length, 0);
  });

  env = runDom(() => Promise.reject(new Error("network")));
  await tick(); await tick();
  test("networkFailureIsShownNotThrown", () => {
    assert.strictEqual(env.card.found[".chart-status"].textContent, "Could not reach the server.");
  });

  const empty = { chart: "x", granularity: "day", period: "2026-11-24", buckets: series.buckets, partial: series.partial,
    lines: [{ key: "h", label: "H", unit: "records", group: "G", values: [null, null, null, null] }] };
  env = runDom(() => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(empty) }));
  await tick(); await tick();
  test("emptySeriesShowsNoDataAndNoPlot", () => {
    assert.strictEqual(env.card.found[".chart-status"].textContent, pure.NO_DATA);
    assert.strictEqual(env.plots.length, 0);
    assert.ok(env.card.found[".chart-text"].textContent.startsWith("period\tH"), "the text table is still there");
  });

  env = runDom(() => Promise.resolve({ ok: false, status: 404, json: () => Promise.resolve({}) }));
  await tick(); await tick();
  test("outOfRangePeriodShowsNoData", () => {
    assert.strictEqual(env.card.found[".chart-status"].textContent, pure.NO_DATA);
  });
}

domTests().then(() => {
  if (failed) { console.log(failed + " test(s) failed"); process.exit(1); }
  console.log("all front-end self-tests passed");
});
