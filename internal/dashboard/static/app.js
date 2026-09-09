/* Rate Limiter Gateway — dashboard client.
 *
 * Polls /api/stats once a second, turns the cumulative counters into rates, and
 * draws the last minute of traffic. No framework and no CDN: the whole UI is
 * embedded in the Go binary, so it has to work from a single static bundle.
 */

const POLL_MS = 1000;
const HISTORY = 60; // samples retained, one per poll — one minute of traffic

const el = (id) => document.getElementById(id);

const state = {
  previous: null,          // last stats payload, for delta-based rates
  history: [],             // [{allowed, throttled}] rates per second
  failures: 0,             // consecutive poll failures
};

/* --- formatting ------------------------------------------------------------ */

function formatCount(n) {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 10_000) return (n / 1000).toFixed(0) + "k";
  if (n >= 1000) return (n / 1000).toFixed(1) + "k";
  return String(Math.round(n));
}

function formatDuration(seconds) {
  const s = Math.floor(seconds);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  const h = Math.floor(s / 3600);
  return `${h}h ${Math.floor((s % 3600) / 60)}m`;
}

// Latency arrives in seconds; show whichever unit keeps it readable.
function formatLatency(seconds) {
  if (!seconds || seconds <= 0) return "—";
  const ms = seconds * 1000;
  if (ms < 1) return `${(ms * 1000).toFixed(0)}µs`;
  if (ms < 100) return `${ms.toFixed(2)}ms`;
  return `${ms.toFixed(0)}ms`;
}

/* --- rendering -------------------------------------------------------------- */

function renderHeader(stats) {
  el("algorithm").textContent = stats.info.algorithm;
  el("upstream").textContent = `→ ${stats.info.upstream}`;
  el("redis-addr").textContent = stats.info.redisAddr;
  el("fail-policy").textContent = stats.info.failOpen ? "fail-open" : "fail-closed";
  el("uptime").textContent = formatDuration(stats.uptimeSeconds);
  el("errors").textContent = formatCount(stats.errors);

  const breaker = el("breaker");
  breaker.textContent = stats.breakerState.replace("_", "-");
  const breakerPill = el("breaker-pill");
  breakerPill.classList.toggle("warn", stats.breakerState === "half_open");
  breakerPill.classList.toggle("bad", stats.breakerState === "open");

  // Redis being unreachable is not the same as the gateway being down: under
  // fail-open it keeps serving, so the status reads "degraded", not "down".
  const status = el("status");
  const text = el("status-text");
  status.classList.remove("ok", "degraded", "down");
  if (!stats.redisHealthy) {
    status.classList.add("degraded");
    text.textContent = "degraded — redis unreachable";
  } else {
    status.classList.add("ok");
    text.textContent = "healthy";
  }
}

function renderKpis(stats, rates) {
  el("kpi-allowed").textContent = formatCount(stats.allowed);
  el("kpi-throttled").textContent = formatCount(stats.throttled);
  el("kpi-degraded").textContent = formatCount(stats.degraded);
  el("degraded-card").classList.toggle("active", stats.degraded > 0);

  el("kpi-allowed-rate").textContent = rates.allowed.toFixed(0);
  el("kpi-throttled-rate").textContent = rates.throttled.toFixed(0);

  el("kpi-p50").textContent = formatLatency(stats.limiter.p50);
  el("kpi-p95").textContent = formatLatency(stats.limiter.p95);
  el("kpi-p99").textContent = formatLatency(stats.limiter.p99);
}

function renderTiers(stats) {
  const list = el("tier-list");

  if (!stats.tiers || stats.tiers.length === 0) {
    list.innerHTML = '<p class="empty">No tiers configured.</p>';
    return;
  }

  list.innerHTML = stats.tiers.map((tier) => {
    const total = tier.allowed + tier.throttled;
    // Widths are shares of this tier's own traffic, so a quiet tier still
    // reads clearly next to a busy one.
    const allowedPct = total > 0 ? (tier.allowed / total) * 100 : 0;
    const throttledPct = total > 0 ? (tier.throttled / total) * 100 : 0;

    return `
      <div class="tier">
        <div class="tier-top">
          <span class="tier-name">${escapeHtml(tier.name)}</span>
          <span class="tier-quota">${tier.limit}/${escapeHtml(tier.window)} · burst ${tier.burst}</span>
        </div>
        <div class="tier-bar">
          <div class="tier-bar-allowed" style="width:${allowedPct}%"></div>
          <div class="tier-bar-throttled" style="width:${throttledPct}%"></div>
        </div>
        <div class="tier-counts">
          <span class="n-allowed">${formatCount(tier.allowed)} allowed</span>
          <span class="n-throttled">${formatCount(tier.throttled)} throttled</span>
        </div>
      </div>`;
  }).join("");
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[c]);
}

/* --- chart ------------------------------------------------------------------ */

function drawChart() {
  const canvas = el("chart");
  const ctx = canvas.getContext("2d");

  // Match the backing store to the CSS size so lines stay crisp on retina.
  const dpr = window.devicePixelRatio || 1;
  const width = canvas.clientWidth;
  const height = canvas.clientHeight || 180;
  if (canvas.width !== width * dpr || canvas.height !== height * dpr) {
    canvas.width = width * dpr;
    canvas.height = height * dpr;
  }
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, width, height);

  const styles = getComputedStyle(document.body);
  const allowedColor = styles.getPropertyValue("--allowed").trim();
  const throttledColor = styles.getPropertyValue("--throttled").trim();
  const gridColor = styles.getPropertyValue("--border").trim();

  const peak = Math.max(1, ...state.history.map((s) => s.allowed + s.throttled));
  el("chart-peak").textContent = `peak ${peak.toFixed(0)}/s`;
  el("chart-empty").hidden = state.history.some((s) => s.allowed + s.throttled > 0);

  // Horizontal grid.
  ctx.strokeStyle = gridColor;
  ctx.lineWidth = 1;
  for (let i = 0; i <= 4; i++) {
    const y = Math.round((height / 4) * i) + 0.5;
    ctx.beginPath();
    ctx.moveTo(0, y);
    ctx.lineTo(width, y);
    ctx.stroke();
  }

  if (state.history.length < 2) return;

  const stepX = width / (HISTORY - 1);
  const scaleY = (v) => height - (v / peak) * (height - 6);

  // Throttled is drawn stacked on top of allowed, so the outer edge is the
  // total offered load and the gap between the two lines is the rejection.
  const series = [
    { key: (s) => s.allowed, color: allowedColor },
    { key: (s) => s.allowed + s.throttled, color: throttledColor },
  ];

  for (const { key, color } of series.reverse()) {
    ctx.beginPath();
    ctx.moveTo(0, height);
    state.history.forEach((sample, i) => {
      // Right-align: the newest sample sits at the right edge.
      const x = (i + (HISTORY - state.history.length)) * stepX;
      ctx.lineTo(x, scaleY(key(sample)));
    });
    ctx.lineTo((HISTORY - 1) * stepX, height);
    ctx.closePath();

    ctx.fillStyle = color + "26"; // ~15% alpha
    ctx.fill();

    ctx.beginPath();
    state.history.forEach((sample, i) => {
      const x = (i + (HISTORY - state.history.length)) * stepX;
      const y = scaleY(key(sample));
      if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    });
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.75;
    ctx.stroke();
  }
}

/* --- polling ---------------------------------------------------------------- */

function ratesFrom(stats) {
  if (!state.previous) return { allowed: 0, throttled: 0 };

  const elapsed = stats.uptimeSeconds - state.previous.uptimeSeconds;
  if (elapsed <= 0) return { allowed: 0, throttled: 0 };

  // Counters only ever increase, but a gateway restart resets them to zero.
  // Clamping at zero keeps a restart from drawing a huge negative spike.
  return {
    allowed: Math.max(0, stats.allowed - state.previous.allowed) / elapsed,
    throttled: Math.max(0, stats.throttled - state.previous.throttled) / elapsed,
  };
}

async function poll() {
  try {
    const res = await fetch("api/stats", { cache: "no-store" });
    if (!res.ok) throw new Error(`stats returned ${res.status}`);
    const stats = await res.json();

    state.failures = 0;

    const rates = ratesFrom(stats);
    if (state.previous) {
      state.history.push(rates);
      if (state.history.length > HISTORY) state.history.shift();
    }
    state.previous = stats;

    renderHeader(stats);
    renderKpis(stats, rates);
    renderTiers(stats);
    drawChart();
    populateTierOptions(stats.tiers);
  } catch (err) {
    state.failures++;
    // One missed poll is a blip; several in a row means the gateway is gone.
    if (state.failures >= 3) {
      const status = el("status");
      status.classList.remove("ok", "degraded");
      status.classList.add("down");
      el("status-text").textContent = "gateway unreachable";
    }
  }
}

/* --- simulator -------------------------------------------------------------- */

let tierOptionsKey = "";

function populateTierOptions(tiers) {
  if (!tiers) return;
  const key = tiers.map((t) => t.name).join(",");
  if (key === tierOptionsKey) return; // avoid clobbering the user's selection
  tierOptionsKey = key;

  const select = el("sim-tier");
  const current = select.value;
  select.innerHTML = tiers
    .map((t) => `<option value="${escapeHtml(t.name)}">${escapeHtml(t.name)} — ${t.limit}/${escapeHtml(t.window)}</option>`)
    .join("");
  if (current) select.value = current;
}

function renderSimulation(result) {
  el("sim-summary").hidden = false;
  el("sim-allowed").textContent = result.allowed;
  el("sim-throttled").textContent = result.throttled;
  el("sim-duration").textContent = result.durationMs.toFixed(1);

  const dots = el("sim-dots");
  dots.innerHTML = result.results
    .map((r, i) => {
      const cls = r.allowed ? "dot" : "dot rejected";
      const title = r.allowed
        ? `#${r.index} allowed · ${r.remaining} remaining`
        : `#${r.index} throttled · retry in ${r.retryAfterMs}ms`;
      // Stagger the pop animation so the burst visibly fills left to right.
      const delay = Math.min(i * 6, 600);
      return `<span class="${cls}" style="animation-delay:${delay}ms" title="${title}"></span>`;
    })
    .join("");

  const first = result.results.findIndex((r) => !r.allowed);
  el("sim-note").textContent = first === -1
    ? `All ${result.allowed} requests fit inside the ${result.tier} quota.`
    : `Request #${first + 1} was the first rejected — the ${result.tier} tier's burst was exhausted there.`;
}

async function runSimulation(event) {
  event.preventDefault();

  const button = el("sim-run");
  button.disabled = true;
  button.textContent = "Sending…";

  try {
    const res = await fetch("api/simulate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        tier: el("sim-tier").value,
        count: Number(el("sim-count").value),
      }),
    });

    const body = await res.json();
    if (!res.ok) {
      el("sim-note").textContent = body.error || "Simulation failed.";
      return;
    }
    renderSimulation(body);
    poll(); // reflect the new counters immediately rather than waiting a tick
  } catch (err) {
    el("sim-note").textContent = "Could not reach the gateway.";
  } finally {
    button.disabled = false;
    button.textContent = "Send burst";
  }
}

/* --- boot -------------------------------------------------------------------- */

el("sim-count").addEventListener("input", (e) => {
  el("sim-count-out").textContent = e.target.value;
});
el("sim-form").addEventListener("submit", runSimulation);
window.addEventListener("resize", drawChart);

poll();
setInterval(poll, POLL_MS);
