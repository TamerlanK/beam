import { state, on, isDone } from "./state.js";
import { $, icon, fmtRate, fmtLeft } from "./util.js";
import { rateOf, etaOf, queueFiles } from "./transfers.js";
import { sendShared, stageFiles } from "./share.js";

const PAD = 60;
const R_MIN = 150;
const R_GAP = 132;
const RINGS_MAX = 3;
const CIRC = 289;

const SVG = "http://www.w3.org/2000/svg";
const cards = new Map();
const beams = new Map();
let field, ringsSvg, sweep, pulses, beamsSvg, peersEl, selfNode, picker;
let geo = null;
let grid = false;
let onNote = () => {};

export function initRadar(opts) {
  onNote = opts.onNote;
  field = $("field");
  ringsSvg = $("rings");
  sweep = $("sweep");
  pulses = field.querySelectorAll(".pulse");
  beamsSvg = $("beams");
  peersEl = $("peers");
  selfNode = $("selfNode");
  picker = $("filePicker");

  new ResizeObserver(() => layout()).observe(field);
  on("layout", () => setTimeout(layout, 280));
  on("peers", syncPeers);
  on("transfer:add", (t) => updateCard(t.peerId));
  on("transfer:progress", (t) => updateCard(t.peerId));
  on("transfer:state", (t) => {
    updateCard(t.peerId);
    if (t.state === "active") {
      flash(t.peerId, "flash-link");
      flashSelf();
    }
    if (t.state === "done") flash(t.peerId, "flash-ok");
    else if (t.state === "failed" && t.note) flash(t.peerId, "flash-bad");
  });
  on("transfer:remove", (t) => updateCard(t.peerId));

  picker.addEventListener("change", () => {
    queueFiles(picker.dataset.target, picker.files);
    picker.value = "";
  });
  initDrag();
}

function measure() {
  const W = field.clientWidth,
    H = field.clientHeight,
    box = cardBox();
  const fr = field.getBoundingClientRect(),
    sr = selfNode.getBoundingClientRect();
  const cx = sr.left - fr.left + sr.width / 2;
  const cy = sr.top - fr.top + sr.height / 2;
  const room = cy - 84;
  if (room < 120 || W < 220)
    return { W, H, cx, cy, maxR: 0, rings: [], cap: 0 };
  const maxR = Math.max(R_MIN, room);
  const n = Math.max(
    1,
    Math.min(RINGS_MAX, Math.floor((maxR - R_MIN) / R_GAP) + 1),
  );
  const rings = [];
  for (let i = 0; i < n; i++) {
    const r =
      n === 1
        ? Math.min(maxR, R_MIN + 40)
        : R_MIN + ((maxR - R_MIN) * i) / (n - 1);
    const k = (W / 2 - PAD) / r;
    const lift = 0.24;
    const a0 = Math.min(
      Math.PI / 2 - 0.02,
      k >= Math.cos(lift) ? lift : Math.acos(Math.max(-1, Math.min(1, k))),
    );
    const ring = { r, a0, span: Math.PI - 2 * a0 };
    ring.cap = capacity(ring, box);
    rings.push(ring);
  }
  return {
    W,
    H,
    cx,
    cy,
    maxR,
    rings,
    cap: rings.reduce((s, x) => s + x.cap, 0),
  };
}

function slots(ring, m) {
  const out = [];
  for (let j = 0; j < m; j++) {
    const a = Math.PI - ring.a0 - (ring.span * (j + 0.5)) / m;
    out.push({ x: ring.r * Math.cos(a), y: -ring.r * Math.sin(a) });
  }
  return out;
}

function capacity(ring, box) {
  for (let m = Math.max(1, Math.floor((ring.r * ring.span) / 90)); m > 1; m--) {
    const p = slots(ring, m);
    const clash = p.some((a, i) =>
      p
        .slice(i + 1)
        .some(
          (b) => Math.abs(a.x - b.x) < box.w && Math.abs(a.y - b.y) < box.h,
        ),
    );
    if (!clash) return m;
  }
  return 1;
}

function cardBox() {
  const first = peersEl.querySelector(".peer:not(.is-leaving)");
  return {
    w: (first ? first.offsetWidth : 116) + 8,
    h: (first ? first.offsetHeight : 134) + 8,
  };
}

function svgEl(tag, attrs) {
  const n = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
  return n;
}

function drawRings() {
  const { cx, cy, W, H } = geo;
  ringsSvg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  const out = [];
  const radii = geo.rings.map((r) => r.r);
  const outer = radii[radii.length - 1] || 0;
  for (const r of [...radii].reverse())
    out.push(svgEl("circle", { class: "zone", cx, cy, r }));
  for (let a = 0; a <= 180; a += 30) {
    const t = (a * Math.PI) / 180;
    out.push(
      svgEl("line", {
        class: "spoke",
        x1: cx + 70 * Math.cos(t),
        y1: cy - 70 * Math.sin(t),
        x2: cx + (outer + 40) * Math.cos(t),
        y2: cy - (outer + 40) * Math.sin(t),
      }),
    );
  }
  radii.forEach((r, i) => {
    const last = i === radii.length - 1;
    out.push(
      svgEl("circle", {
        class: `ring-line${last ? " is-outer" : ""}`,
        cx,
        cy,
        r,
      }),
    );
    if (!last) {
      out.push(svgEl("circle", { class: "tick", cx, cy, r, pathLength: 360 }));
      out.push(
        svgEl("circle", { class: "tick-major", cx, cy, r, pathLength: 360 }),
      );
    }
  });
  ringsSvg.replaceChildren(...out);
  const size = geo.maxR * 2 + 120;
  sweep.style.cssText = `width:${size}px;height:${size}px;left:${cx - size / 2}px;top:${cy - size / 2}px`;
  for (const p of pulses)
    p.style.cssText = `left:${cx}px;top:${cy}px;--d:${(geo.maxR + 60) * 2}px`;
  beamsSvg.setAttribute("viewBox", `0 0 ${W} ${H}`);
}

function layout() {
  if (!field.clientWidth) return;
  geo = measure();
  drawRings();
  const ids = [...state.peers.keys()].filter(
    (id) => cards.has(id) && !cards.get(id).leaving,
  );
  grid = ids.length > geo.cap;
  field.classList.toggle("is-grid", grid);
  if (!grid) {
    let i = 0;
    for (const ring of geo.rings) {
      const m = Math.min(ring.cap, ids.length - i);
      for (const s of slots(ring, m))
        place(cards.get(ids[i++]), geo.cx + s.x, geo.cy + s.y);
    }
  }
  syncBeams();
}

function place(c, x, y) {
  c.x = x;
  c.y = y;
  c.el.style.setProperty("--x", `${x.toFixed(1)}px`);
  c.el.style.setProperty("--y", `${y.toFixed(1)}px`);
}

function syncPeers() {
  for (const [id, c] of cards) {
    if (!state.peers.has(id) && !c.leaving) removeCard(id);
  }
  for (const p of state.peers.values()) {
    const c = cards.get(p.id);
    if (!c) addCard(p);
    else {
      c.el.querySelector(".peer-emoji").textContent = p.emoji;
      c.el.querySelector(".peer-name").textContent = p.name;
    }
  }
  field.classList.toggle("has-peers", state.peers.size > 0);
  layout();
}

function addCard(p) {
  const el = $("peerT").content.firstElementChild.cloneNode(true);
  el.dataset.id = p.id;
  el.querySelector(".peer-emoji").textContent = p.emoji;
  el.querySelector(".peer-name").textContent = p.name;
  const hit = el.querySelector(".peer-hit");
  hit.setAttribute("aria-label", `Send files to ${p.name}`);
  hit.addEventListener("click", () => {
    if (!sendShared(p.id)) pickFiles(p.id);
  });
  hit.addEventListener("dragover", (e) => {
    e.preventDefault();
    el.classList.add("is-target");
  });
  hit.addEventListener("dragleave", () => el.classList.remove("is-target"));
  hit.addEventListener("drop", (e) => {
    e.preventDefault();
    e.stopPropagation();
    el.classList.remove("is-target");
    endDrag();
    filesOf(e.dataTransfer).then((files) => queueFiles(p.id, files));
  });
  const note = el.querySelector(".note-btn");
  note.setAttribute("aria-label", `Send a note to ${p.name}`);
  note.addEventListener("click", () => onNote(p));

  const c = { el, x: geo ? geo.cx : 0, y: geo ? geo.cy : 0, leaving: false };
  cards.set(p.id, c);
  place(c, c.x, c.y);
  peersEl.append(el);
  void el.offsetWidth;
  updateCard(p.id);
}

function removeCard(id) {
  const c = cards.get(id);
  c.leaving = true;
  c.el.classList.add("is-leaving");
  const done = () => {
    c.el.remove();
    if (cards.get(id) === c) cards.delete(id);
  };
  c.el
    .querySelector(".peer-body")
    .addEventListener("animationend", done, { once: true });
  setTimeout(done, 400);
}

function updateCard(peerId) {
  const c = cards.get(peerId);
  if (!c) return;
  const el = c.el;
  const p = state.peers.get(peerId);
  let active = null,
    offered = null;
  for (const t of state.transfers.values()) {
    if (t.peerId !== peerId || isDone(t)) continue;
    if (t.state === "active") {
      active = t;
      break;
    }
    if (t.state === "offered") offered = t;
  }
  el.classList.toggle("is-busy", !!active);
  el.classList.toggle("is-waiting", !!offered && !active);
  const pct = el.querySelector(".peer-pct");
  const fill = el.querySelector(".ring-fill");
  const dir = el.querySelector(".peer-dir");
  if (active) {
    const frac = active.size ? active.bytes / active.size : 0;
    pct.hidden = false;
    pct.textContent = `${Math.floor(frac * 100)}%`;
    fill.style.strokeDashoffset = (CIRC * (1 - frac)).toFixed(1);
    dir.replaceChildren(icon(active.dir === "send" ? "up" : "down"));
    const rate = fmtRate(rateOf(active)),
      left = fmtLeft(etaOf(active));
    setSub(el, rate ? `${left || "…"} at ${rate}` : "starting…");
  } else if (offered) {
    pct.hidden = true;
    fill.style.strokeDashoffset = CIRC;
    setSub(el, "waiting to accept…");
  } else {
    pct.hidden = true;
    fill.style.strokeDashoffset = CIRC;
    if (p) setSub(el, p.device, p.device);
  }
  syncBeams();
}

function setSub(el, text, iconName) {
  const sub = el.querySelector(".peer-sub");
  sub.replaceChildren();
  if (iconName) sub.append(icon(iconName));
  sub.append(document.createTextNode(text));
}

function flashSelf() {
  selfNode.classList.remove("flash-link");
  void selfNode.offsetWidth;
  selfNode.classList.add("flash-link");
  setTimeout(() => selfNode.classList.remove("flash-link"), 1000);
}

function flash(peerId, cls) {
  const c = cards.get(peerId);
  if (!c) return;
  c.el.classList.remove(cls);
  void c.el.offsetWidth;
  c.el.classList.add(cls);
  setTimeout(() => c.el.classList.remove(cls), 1000);
}

function syncBeams() {
  if (!geo) return;
  const alive = new Set();
  for (const t of state.transfers.values()) {
    if (t.state !== "active" || grid) continue;
    const c = cards.get(t.peerId);
    if (!c || alive.has(t.peerId)) continue;
    alive.add(t.peerId);
    let g = beams.get(t.peerId);
    if (!g) {
      g = document.createElementNS(SVG, "g");
      g.setAttribute("class", "beam");
      for (const cls of ["beam-glow", "beam-base", "beam-line"]) {
        const l = document.createElementNS(SVG, "line");
        l.setAttribute("class", cls);
        l.setAttribute("pathLength", "1");
        g.append(l);
      }
      for (let i = 0; i < 3; i++) {
        const d = document.createElementNS(SVG, "circle");
        d.setAttribute("class", "beam-dot");
        g.append(d);
      }
      beamsSvg.append(g);
      beams.set(t.peerId, g);
    }
    g.classList.toggle("is-recv", t.dir === "recv");
    const dx = c.x - geo.cx,
      dy = c.y - geo.cy,
      len = Math.hypot(dx, dy) || 1;
    const ux = dx / len,
      uy = dy / len;
    const hw = c.el.offsetWidth / 2 + 2,
      hh = c.el.offsetHeight / 2 + 2;
    const tx = ux ? (dx - Math.sign(ux) * hw) / ux : -Infinity;
    const ty = uy ? (dy - Math.sign(uy) * hh) / uy : -Infinity;
    const dist = Math.max(52, Math.max(tx, ty) - 6);
    const pts = {
      x1: geo.cx + ux * 44,
      y1: geo.cy + uy * 44,
      x2: geo.cx + ux * dist,
      y2: geo.cy + uy * dist,
    };
    for (const l of g.querySelectorAll("line"))
      for (const [k, v] of Object.entries(pts)) l.setAttribute(k, v.toFixed(1));
    const [sx, sy, ex, ey] =
      t.dir === "send"
        ? [pts.x1, pts.y1, pts.x2, pts.y2]
        : [pts.x2, pts.y2, pts.x1, pts.y1];
    const path = `path("M ${sx.toFixed(1)} ${sy.toFixed(1)} L ${ex.toFixed(1)} ${ey.toFixed(1)}")`;
    for (const d of g.querySelectorAll(".beam-dot")) d.style.offsetPath = path;
  }
  for (const [id, g] of beams)
    if (!alive.has(id)) {
      g.remove();
      beams.delete(id);
    }
  selfNode.classList.toggle("is-busy", alive.size > 0);
}

let dragDepth = 0;

function hasFiles(e) {
  return e.dataTransfer && [...e.dataTransfer.types].includes("Files");
}

function endDrag() {
  dragDepth = 0;
  document.body.classList.remove("is-dragging");
}

function initDrag() {
  window.addEventListener("dragenter", (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dragDepth++;
    document.body.classList.add("is-dragging");
  });
  window.addEventListener("dragover", (e) => {
    if (hasFiles(e)) e.preventDefault();
  });
  window.addEventListener("dragleave", (e) => {
    if (!hasFiles(e)) return;
    if (--dragDepth <= 0) endDrag();
  });
  window.addEventListener("drop", (e) => {
    e.preventDefault();
    if (!hasFiles(e)) return endDrag();
    endDrag();
    filesOf(e.dataTransfer).then(stageFiles);
  });
}

// Files that come with a file-system handle can be reopened after a reload,
// which is what lets a send resume. The picker and drops give one on Chromium.
async function pickFiles(target) {
  if (typeof showOpenFilePicker === "function") {
    try {
      const handles = await showOpenFilePicker({ multiple: true });
      queueFiles(target, await Promise.all(handles.map(withHandle)));
      return;
    } catch (e) {
      if (e && e.name === "AbortError") return;
    }
  }
  picker.dataset.target = target;
  picker.click();
}

async function withHandle(h) {
  const f = await h.getFile();
  f.handle = h;
  return f;
}

function filesOf(dt) {
  const files = [...dt.files];
  const items = [...dt.items].filter((i) => i.kind === "file");
  if (
    items.length !== files.length ||
    !items.length ||
    typeof items[0].getAsFileSystemHandle !== "function"
  )
    return Promise.resolve(files);
  return Promise.all(
    items.map((i) => i.getAsFileSystemHandle().catch(() => null)),
  ).then((hs) => {
    hs.forEach((h, i) => {
      if (h && h.kind === "file") files[i].handle = h;
    });
    return files;
  });
}
