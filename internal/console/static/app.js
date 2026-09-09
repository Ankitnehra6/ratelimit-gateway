/* Gateway console — the caller's view of the rate limiter.
 *
 * Every number on this page comes from the X-RateLimit-* headers on real
 * proxied responses. The page is served from the proxy's own origin, so those
 * headers are readable without any CORS exposure; nothing here needs a
 * privileged endpoint.
 */

const el = (id) => document.getElementById(id);

const state = {
  config: { basePath: "/__gateway/", probePath: "/get", dashboardPort: "", algorithm: "" },
  sent: 0,
  allowed: 0,
  throttled: 0,
  flooding: false,
  resetTimer: null,
  resetAt: null,
};

const MAX_LOG_ROWS = 60;
const MAX_DOTS = 200;

/* --- setup ------------------------------------------------------------------ */

async function loadConfig() {
  try {
    const res = await fetch("config", { cache: "no-store" });
    if (res.ok) state.config = { ...state.config, ...(await res.json()) };
  } catch {
    // Fall back to the defaults; the page still works.
  }

  el("probe-label").textContent = "→ " + state.config.probePath;
  el("algorithm").textContent = state.config.algorithm || "—";

  const link = el("dashboard-link");
  if (state.config.dashboardPort) {
    // The server cannot know the externally visible hostname, so the browser
    // supplies it and the server supplies only the port.
    link.href = `${location.protocol}//${location.hostname}:${state.config.dashboardPort}/`;
  } else {
    link.hidden = true;
  }
}

/* --- sending ---------------------------------------------------------------- */

function currentKey() {
  const choice = el("api-key").value;
  if (choice === "__custom") return el("custom-key").value.trim();
  return choice;
}

async function sendOne() {
  const headers = {};
  const key = currentKey();
  if (key) headers["X-API-Key"] = key;

  const started = performance.now();

  try {
    const res = await fetch(state.config.probePath, {
      headers,
      cache: "no-store",
      // The limiter keys anonymous callers by IP; credentials are irrelevant
      // here and omitting them keeps the request simple.
      credentials: "omit",
    });

    const elapsed = performance.now() - started;
    record({
      status: res.status,
      limit: numberOrNull(res.headers.get("X-RateLimit-Limit")),
      remaining: numberOrNull(res.headers.get("X-RateLimit-Remaining")),
      reset: numberOrNull(res.headers.get("X-RateLimit-Reset")),
      retryAfter: numberOrNull(res.headers.get("Retry-After")),
      degraded: res.headers.get("X-RateLimit-Degraded") === "true",
      elapsed,
    });
  } catch (err) {
    record({ status: 0, elapsed: performance.now() - started, error: true });
  }
}

function numberOrNull(v) {
  if (v === null || v === "") return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

async function sendBurst(count) {
  setBusy(true);
  // Sent sequentially rather than in parallel so the dot grid and the log read
  // in the order the limiter actually saw them. A parallel burst would race and
  // the "first rejected" position would jitter between runs.
  for (let i = 0; i < count; i++) {
    await sendOne();
  }
  setBusy(false);
}

function setBusy(busy) {
  document.querySelectorAll("[data-send]").forEach((b) => { b.disabled = busy; });
}

/* --- flood mode -------------------------------------------------------------- */

async function floodLoop() {
  while (state.flooding) {
    await sendOne();
    // A small gap keeps the browser responsive and makes the refill visible,
    // rather than saturating the connection pool.
    await new Promise((r) => setTimeout(r, 60));
  }
}

function toggleFlood() {
  state.flooding = !state.flooding;
  const button = el("flood");
  button.classList.toggle("active", state.flooding);
  button.textContent = state.flooding ? "Stop" : "Flood";
  if (state.flooding) floodLoop();
}

/* --- rendering ---------------------------------------------------------------- */

function record(result) {
  state.sent++;
  if (result.status === 200) state.allowed++;
  else if (result.status === 429) state.throttled++;

  updateMeter(result);
  appendDot(result);
  appendLog(result);

  el("total-sent").textContent = state.sent;
  el("total-allowed").textContent = state.allowed;
  el("total-throttled").textContent = state.throttled;
}

function updateMeter(result) {
  if (result.limit === null || result.limit === undefined) return;

  const remaining = result.remaining ?? 0;
  const limit = result.limit;
  const pct = limit > 0 ? Math.max(0, Math.min(100, (remaining / limit) * 100)) : 0;

  const remainingEl = el("meter-remaining");
  const fillEl = el("meter-fill");

  remainingEl.textContent = remaining;
  el("meter-limit").textContent = limit;
  fillEl.style.width = pct + "%";

  // Colour shifts as headroom disappears, so the throttle is visible coming.
  const level = remaining === 0 ? "empty" : pct <= 25 ? "low" : "";
  remainingEl.className = "meter-remaining " + level;
  fillEl.className = "meter-fill " + level;

  // The tier name is not on the wire, so it is inferred from the limit. This
  // is a display convenience only.
  el("tier-badge").textContent = describeTier(limit);
  el("tier-detail").textContent = `limit ${limit} per window`;

  const status = el("last-status");
  if (result.degraded) {
    status.textContent = "degraded — quota not enforced";
    status.className = "meter-status warn";
  } else if (result.status === 429) {
    status.textContent = `throttled · retry in ${result.retryAfter ?? "?"}s`;
    status.className = "meter-status warn";
  } else if (result.status === 200) {
    status.textContent = "allowed";
    status.className = "meter-status ok";
  }

  startResetCountdown(result.reset);
}

// describeTier maps a limit back to the demo tier names in tenants.json.
function describeTier(limit) {
  switch (limit) {
    case 20: return "anonymous";
    case 150: return "free";
    case 10000: return "pro";
    case 200000: return "internal";
    default: return "custom";
  }
}

function startResetCountdown(resetSeconds) {
  if (resetSeconds === null || resetSeconds === undefined) return;

  state.resetAt = Date.now() + resetSeconds * 1000;
  if (state.resetTimer) return; // one ticker is enough

  state.resetTimer = setInterval(() => {
    const left = Math.max(0, Math.ceil((state.resetAt - Date.now()) / 1000));
    el("reset-text").textContent = left > 0
      ? `full quota restored in ${left}s`
      : "quota restored";
  }, 250);
}

function appendDot(result) {
  const dots = el("dots");
  const dot = document.createElement("span");
  dot.className = "dot" +
    (result.status === 429 ? " rejected" : result.status === 200 ? "" : " failed");
  dot.title = result.status === 200
    ? `200 · ${result.remaining ?? "?"} remaining`
    : result.status === 429
      ? `429 · retry in ${result.retryAfter ?? "?"}s`
      : "request failed";
  dots.appendChild(dot);

  while (dots.children.length > MAX_DOTS) dots.removeChild(dots.firstChild);
}

function appendLog(result) {
  const log = el("log");
  log.querySelector(".empty")?.remove();

  const statusClass = result.status === 200 ? "s200" : result.status === 429 ? "s429" : "err";
  const statusText = result.status === 0 ? "ERR" : result.status;

  const detail = result.status === 429
    ? `throttled · Retry-After ${result.retryAfter ?? "?"}s`
    : result.error
      ? "request failed"
      : result.degraded
        ? "allowed (degraded — quota unenforced)"
        : "allowed";

  const row = document.createElement("div");
  row.className = "log-row";
  row.innerHTML = `
    <span class="log-time">${new Date().toLocaleTimeString()}</span>
    <span class="log-status ${statusClass}">${statusText}</span>
    <span class="log-detail">${detail}</span>
    <span class="log-remaining">${result.remaining ?? "—"} left · ${result.elapsed.toFixed(0)}ms</span>`;

  log.prepend(row);
  while (log.children.length > MAX_LOG_ROWS) log.removeChild(log.lastChild);
}

/* --- boot ---------------------------------------------------------------------- */

el("api-key").addEventListener("change", (e) => {
  el("custom-key-field").hidden = e.target.value !== "__custom";
});

document.querySelectorAll("[data-send]").forEach((button) => {
  button.addEventListener("click", () => sendBurst(Number(button.dataset.send)));
});

el("flood").addEventListener("click", toggleFlood);

el("clear-log").addEventListener("click", () => {
  el("log").innerHTML = '<p class="empty">No requests yet.</p>';
  el("dots").innerHTML = "";
});

loadConfig();
