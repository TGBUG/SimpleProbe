"use strict";

/*
 * 两个页面共用的工具。刻意用全局对象而不是 ES module：
 * 这样直接用 file:// 打开也能跑，不需要起服务。
 */
const Probe = (function () {
  // 数据本身就是 30 秒粒度的，响应也带 Cache-Control: max-age=15。
  const REFRESH_MS = 15000;

  function formatBytes(n) {
    if (!Number.isFinite(n) || n <= 0) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB", "PB"];
    const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length - 1);
    const v = n / Math.pow(1024, i);
    return (v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)) + " " + units[i];
  }

  function formatUptime(sec) {
    if (!Number.isFinite(sec) || sec <= 0) return "—";
    const d = Math.floor(sec / 86400);
    const h = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    if (d > 0) return d + " 天 " + h + " 小时";
    if (h > 0) return h + " 小时 " + m + " 分";
    return m + " 分";
  }

  function formatRate(r) {
    if (typeof r !== "number" || !Number.isFinite(r)) return "—";
    const pct = r * 100;
    // 满勤显示 100%，其余保留两位小数——99.99% 和 100% 不该看起来一样。
    return (pct >= 99.995 ? "100" : pct.toFixed(2)) + "%";
  }

  function pct(used, total) {
    if (!total) return 0;
    return Math.min(100, Math.max(0, (used / total) * 100));
  }

  function barClass(p) {
    if (p >= 90) return "bad";
    if (p >= 75) return "warn";
    return "";
  }

  function escapeHtml(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function row(key, value) {
    return '<div class="row"><span class="k">' + key + '</span><span class="v">' + value + "</span></div>";
  }

  function bar(p) {
    return '<div class="bar"><i class="' + barClass(p) + '" style="width:' + p.toFixed(1) + '%"></i></div>';
  }

  async function getJSON(url) {
    const resp = await fetch(url, { headers: { Accept: "application/json" } });
    if (!resp.ok) {
      let detail = "";
      try {
        const body = await resp.json();
        detail = body.message || body.code || "";
      } catch (e) {
        /* 响应不是 JSON，忽略 */
      }
      throw new Error("HTTP " + resp.status + (detail ? " · " + detail : ""));
    }
    return resp.json();
  }

  function param(name) {
    return new URLSearchParams(location.search).get(name);
  }

  function showBanner(msg) {
    const el = document.getElementById("banner");
    if (!el) return;
    el.textContent = msg;
    el.classList.add("show");
  }

  function hideBanner() {
    const el = document.getElementById("banner");
    if (el) el.classList.remove("show");
  }

  return {
    REFRESH_MS: REFRESH_MS,
    formatBytes: formatBytes,
    formatUptime: formatUptime,
    formatRate: formatRate,
    pct: pct,
    barClass: barClass,
    escapeHtml: escapeHtml,
    row: row,
    bar: bar,
    getJSON: getJSON,
    param: param,
    showBanner: showBanner,
    hideBanner: hideBanner,
  };
})();
