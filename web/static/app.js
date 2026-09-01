"use strict";

const PROTO_V = 1;
const CHUNK = 64 * 1024;
const INITIAL_CREDITS = 16;
const CODE_RE = /^[2-9A-HJ-NP-Z]{4}$/;

const $ = (id) => document.getElementById(id);

const ui = {
  peers: $("peers"), empty: $("empty"), field: $("field"),
  selfName: $("selfName"), selfEmoji: $("selfEmoji"), connState: $("connState"),
  netBtn: $("netBtn"), emptyNetBtn: $("emptyNetBtn"), netModal: $("netModal"),
  netClose: $("netClose"), roomCode: $("roomCode"), qr: $("qr"),
  joinForm: $("joinForm"), joinInput: $("joinInput"),
  offerModal: $("offerModal"), offerSender: $("offerSender"), offerEmoji: $("offerEmoji"),
  offerName: $("offerName"), offerSize: $("offerSize"),
  offerAccept: $("offerAccept"), offerDecline: $("offerDecline"),
  snippetModal: $("snippetModal"), snipTo: $("snipTo"), snipText: $("snipText"),
  snipSend: $("snipSend"), snipCancel: $("snipCancel"),
  snippetRecvModal: $("snippetRecvModal"), snipFrom: $("snipFrom"),
  snipRecvText: $("snipRecvText"), snipCopy: $("snipCopy"), snipRecvClose: $("snipRecvClose"),
  transfersPanel: $("transfersPanel"), transferList: $("transferList"),
  overallBar: document.querySelector("#overallBar .bar-fill"), overallPct: $("overallPct"),
  toasts: $("toasts"), filePicker: $("filePicker"), peerCardT: $("peerCardT"),
};

const DEV_ICONS = {
  phone: '<svg viewBox="0 0 24 24"><rect x="7" y="2.5" width="10" height="19" rx="2.5"/><path d="M10.5 18.5h3"/></svg>',
  tablet: '<svg viewBox="0 0 24 24"><rect x="4.5" y="3" width="15" height="18" rx="2.5"/><path d="M10 18h4"/></svg>',
  laptop: '<svg viewBox="0 0 24 24"><rect x="4" y="5" width="16" height="11" rx="1.5"/><path d="M2.5 19h19"/></svg>',
};

let ws = null;
let self = null;
let reconnectDelay = 1000;
let pendingJoinCode = null;

const peers = new Map();
const transfers = new Map();
const sendQueues = new Map();
const pendingOffers = [];

if (CODE_RE.test(location.hash.slice(1).toUpperCase())) {
  pendingJoinCode = location.hash.slice(1).toUpperCase();
  history.replaceState(null, "", location.pathname);
}

function connect() {
  const scheme = location.protocol === "https:" ? "wss" : "ws";
  ws = new WebSocket(`${scheme}://${location.host}/ws`);
  ws.binaryType = "arraybuffer";

  ws.onopen = () => {
    reconnectDelay = 1000;
    setConn(true);
  };

  ws.onmessage = (ev) => {
    if (typeof ev.data === "string") {
      let env;
      try { env = JSON.parse(ev.data); } catch { return; }
      if (env.v !== PROTO_V || !env.type) return;
      const h = handlers[env.type];
      if (h) h(env.data || {});
    } else {
      onChunk(ev.data);
    }
  };

  ws.onclose = () => {
    setConn(false);
    self = null;
    peers.clear();
    renderPeers();
    for (const t of transfers.values()) {
      if (!isDone(t)) endTransfer(t, "failed", "connection lost");
    }
    setTimeout(connect, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, 15000);
  };
}

function send(type, data) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ v: PROTO_V, type, data }));
  }
}

function setConn(on) {
  ui.connState.className = on ? "conn conn-on" : "conn conn-off";
  ui.connState.setAttribute("aria-label", on ? "connected" : "connecting");
  if (!on) ui.selfName.textContent = "…";
}

const handlers = {
  "room-state"(d) {
    self = d.self;
    ui.selfName.textContent = self.name;
    ui.selfEmoji.textContent = self.emoji;
    peers.clear();
    for (const p of d.peers || []) peers.set(p.id, { peer: p, el: null });
    renderPeers();
    if (pendingJoinCode) {
      send("room-join", { code: pendingJoinCode });
      pendingJoinCode = null;
    }
  },

  "peer-joined"(p) {
    if (!peers.has(p.id)) {
      peers.set(p.id, { peer: p, el: null });
      renderPeers();
    }
  },

  "peer-left"(d) {
    peers.delete(d.id);
    renderPeers();
  },

  "room-created"(d) {
    ui.roomCode.textContent = d.code;
    const url = `${location.origin}${location.pathname}#${d.code}`;
    try {
      const qr = qrcode(0, "M");
      qr.addData(url);
      qr.make();
      ui.qr.innerHTML = qr.createSvgTag({ cellSize: 4, margin: 0 });
    } catch {
      ui.qr.textContent = url;
    }
  },

  "transfer-offer"(d) {
    pendingOffers.push(d);
    maybeShowOffer();
  },

  "transfer-answer"(d) {
    const t = transfers.get(d.id);
    if (!t || t.dir !== "send" || t.state !== "offered") return;
    if (d.accept) {
      t.state = "active";
      t.credits = INITIAL_CREDITS;
      t.startedAt = performance.now();
      updateRow(t);
      updatePeerCard(t.peerId);
      pump(t);
    } else {
      endTransfer(t, "declined", `${peerName(t.peerId)} declined`);
    }
  },

  "flow-credit"(d) {
    const t = transfers.get(d.id);
    if (!t || t.dir !== "send" || t.state !== "active") return;
    t.credits += d.n;
    paintProgress(t);
    pump(t);
  },

  "transfer-complete"(d) {
    const t = transfers.get(d.id);
    if (!t || isDone(t)) return;
    if (t.dir === "recv") finishReceive(t);
    else endTransfer(t, "done");
  },

  "transfer-failed"(d) {
    const t = transfers.get(d.id);
    if (!t || isDone(t)) return;
    const why = d.reason === "offer-timeout" ? "offer timed out" :
      d.reason === "peer-disconnected" ? "peer disconnected" :
      d.reason === "server-shutdown" ? "server shut down" : d.reason;
    endTransfer(t, "failed", why);
  },

  "transfer-cancel"(d) {
    const t = transfers.get(d.id);
    if (!t || isDone(t)) return;
    endTransfer(t, "failed", `${peerName(t.peerId)} canceled`);
  },

  snippet(d) {
    ui.snipFrom.textContent = d.from ? `${d.from.name} ${d.from.emoji}` : "someone";
    ui.snipRecvText.textContent = d.text;
    if (!ui.snippetRecvModal.open) ui.snippetRecvModal.showModal();
  },

  error(d) {
    toast(d.message || d.code || "something went wrong", "bad");
  },
};

function renderPeers() {
  ui.peers.replaceChildren();
  for (const entry of peers.values()) {
    entry.el = makePeerCard(entry.peer);
    ui.peers.appendChild(entry.el);
  }
  ui.empty.hidden = peers.size > 0;
}

function makePeerCard(p) {
  const el = ui.peerCardT.content.firstElementChild.cloneNode(true);
  el.dataset.id = p.id;
  el.querySelector(".peer-emoji").textContent = p.emoji;
  el.querySelector(".peer-name").textContent = p.name;
  setSub(el, p.device);

  const hit = el.querySelector(".peer-hit");
  hit.addEventListener("click", () => {
    ui.filePicker.dataset.target = p.id;
    ui.filePicker.click();
  });
  hit.addEventListener("dragover", (e) => { e.preventDefault(); el.classList.add("dragover"); });
  hit.addEventListener("dragleave", () => el.classList.remove("dragover"));
  hit.addEventListener("drop", (e) => {
    e.preventDefault();
    el.classList.remove("dragover");
    queueFiles(p.id, e.dataTransfer.files);
  });

  el.querySelector(".snip-btn").addEventListener("click", () => {
    ui.snipTo.textContent = p.name;
    ui.snippetModal.dataset.target = p.id;
    ui.snipText.value = "";
    ui.snippetModal.showModal();
    ui.snipText.focus();
  });
  return el;
}

function setSub(el, text, withIcon = true) {
  const sub = el.querySelector(".peer-sub");
  sub.replaceChildren();
  if (withIcon && DEV_ICONS[text]) {
    const span = document.createElement("span");
    span.innerHTML = DEV_ICONS[text];
    sub.appendChild(span.firstElementChild);
  }
  sub.appendChild(document.createTextNode(text));
}

function peerName(id) {
  const e = peers.get(id);
  return e ? e.peer.name : "peer";
}

function updatePeerCard(peerId) {
  const entry = peers.get(peerId);
  if (!entry || !entry.el) return;
  const el = entry.el;
  let active = null, offered = null;
  for (const t of transfers.values()) {
    if (t.peerId !== peerId || isDone(t)) continue;
    if (t.state === "active") { active = t; break; }
    if (t.state === "offered") offered = t;
  }
  const t = active || offered;
  el.classList.toggle("busy", !!active);
  el.classList.toggle("waiting", !!offered && !active);
  const pct = el.querySelector(".peer-pct");
  const ring = el.querySelector(".ring-fg");
  if (active) {
    const frac = active.bytes / active.size;
    pct.hidden = false;
    pct.textContent = `${Math.floor(frac * 100)}%`;
    ring.style.strokeDashoffset = 339.3 * (1 - frac);
    setSub(el, `${fmtRate(rateOf(active))} · ${fmtETA(active)}`, false);
  } else if (offered) {
    pct.hidden = true;
    ring.style.strokeDashoffset = 339.3;
    setSub(el, "waiting to accept…", false);
  } else {
    pct.hidden = true;
    ring.style.strokeDashoffset = 339.3;
    setSub(el, entry.peer.device);
  }
}

function flashPeer(peerId, cls) {
  const entry = peers.get(peerId);
  if (!entry || !entry.el) return;
  entry.el.classList.add(cls);
  setTimeout(() => entry.el && entry.el.classList.remove(cls), 1000);
}

function queueFiles(peerId, files) {
  if (!files || files.length === 0) return;
  const q = sendQueues.get(peerId) || [];
  for (const f of files) {
    if (f.size === 0) { toast(`"${f.name}" is empty, skipped`, "bad"); continue; }
    q.push(f);
  }
  sendQueues.set(peerId, q);
  nextSend(peerId);
}

function nextSend(peerId) {
  for (const t of transfers.values()) {
    if (t.dir === "send" && t.peerId === peerId && !isDone(t)) return;
  }
  const q = sendQueues.get(peerId);
  if (!q || q.length === 0) return;
  if (!peers.has(peerId)) { q.length = 0; return; }
  const file = q.shift();

  const id = crypto.randomUUID();
  const t = {
    id, idBytes: uuidBytes(id), dir: "send", peerId,
    name: file.name, size: file.size, mime: file.type,
    state: "offered", file, offset: 0, bytes: 0, credits: 0,
    pumping: false, samples: [], lastPaint: 0, row: null,
  };
  transfers.set(id, t);
  send("transfer-offer", { id, to: peerId, name: file.name, size: file.size, mime: file.type });
  addRow(t);
  updatePeerCard(peerId);
}

async function pump(t) {
  if (t.pumping) return;
  t.pumping = true;
  try {
    while (t.state === "active" && t.credits > 0 && t.offset < t.size &&
           ws && ws.readyState === WebSocket.OPEN) {
      const end = Math.min(t.offset + CHUNK, t.size);
      let buf;
      try {
        buf = await t.file.slice(t.offset, end).arrayBuffer();
      } catch {
        cancelTransfer(t, "could not read file");
        return;
      }
      if (t.state !== "active") return;
      const frame = new Uint8Array(16 + buf.byteLength);
      frame.set(t.idBytes, 0);
      frame.set(new Uint8Array(buf), 16);
      ws.send(frame);
      t.credits--;
      t.offset = end;
      t.bytes = end;
      t.samples.push([performance.now(), end]);
      if (t.samples.length > 30) t.samples.shift();
      paintProgress(t);
    }
  } finally {
    t.pumping = false;
  }
}

function maybeShowOffer() {
  if (ui.offerModal.open || pendingOffers.length === 0) return;
  const d = pendingOffers[0];
  ui.offerSender.textContent = d.from ? d.from.name : "someone";
  ui.offerEmoji.textContent = d.from ? d.from.emoji : "📦";
  ui.offerName.textContent = d.name;
  ui.offerSize.textContent = fmtSize(d.size);
  ui.offerModal.showModal();
}

function answerOffer(accept) {
  const d = pendingOffers.shift();
  if (!d) return;
  send("transfer-answer", { id: d.id, accept });
  if (accept) {
    const t = {
      id: d.id, dir: "recv", peerId: d.from ? d.from.id : "",
      name: d.name, size: d.size, mime: d.mime || "",
      state: "active", bytes: 0, parts: [],
      samples: [], lastPaint: 0, row: null, startedAt: performance.now(),
    };
    transfers.set(d.id, t);
    addRow(t);
    updatePeerCard(t.peerId);
  }
  if (ui.offerModal.open) ui.offerModal.close("answered");
  maybeShowOffer();
}

function onChunk(data) {
  if (data.byteLength <= 16) return;
  const id = bytesToUUID(new Uint8Array(data, 0, 16));
  const t = transfers.get(id);
  if (!t || t.dir !== "recv" || t.state !== "active") return;
  t.parts.push(data.slice(16));
  t.bytes += data.byteLength - 16;
  t.samples.push([performance.now(), t.bytes]);
  if (t.samples.length > 30) t.samples.shift();
  paintProgress(t);
  if (t.bytes >= t.size) finishReceive(t);
}

function finishReceive(t) {
  if (t.state !== "active") return;
  const blob = new Blob(t.parts, { type: t.mime || "application/octet-stream" });
  t.parts = [];
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = t.name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 60000);
  endTransfer(t, "done");
}

function isDone(t) {
  return t.state === "done" || t.state === "failed" || t.state === "declined";
}

function cancelTransfer(t, note) {
  if (isDone(t)) return;
  send("transfer-cancel", { id: t.id, reason: note || "canceled" });
  endTransfer(t, "failed", note || "canceled");
}

function endTransfer(t, state, note) {
  t.state = state;
  t.parts = [];
  if (state === "done") {
    flashPeer(t.peerId, "done");
    toast(t.dir === "send" ? `Sent ${t.name} ✓` : `Received ${t.name} ✓`, "ok");
  } else if (note) {
    flashPeer(t.peerId, "failed");
    toast(`${t.name}: ${note}`, "bad");
  }
  updateRow(t);
  updatePeerCard(t.peerId);
  renderOverall();
  setTimeout(() => {
    transfers.delete(t.id);
    if (t.row) t.row.remove();
    renderOverall();
  }, state === "done" ? 5000 : 8000);
  if (t.dir === "send") nextSend(t.peerId);
}

function rateOf(t) {
  const s = t.samples;
  if (!s || s.length < 2) return 0;
  const [t0, b0] = s[0];
  const [t1, b1] = s[s.length - 1];
  return t1 > t0 ? (b1 - b0) / ((t1 - t0) / 1000) : 0;
}

function paintProgress(t) {
  const now = performance.now();
  if (now - t.lastPaint < 100 && t.bytes < t.size) return;
  t.lastPaint = now;
  updatePeerCard(t.peerId);
  updateRow(t);
  renderOverall();
}

function addRow(t) {
  const li = document.createElement("li");
  const name = document.createElement("span");
  name.className = "t-name";
  name.textContent = `${t.dir === "send" ? "↑" : "↓"} ${t.name}`;
  const status = document.createElement("span");
  status.className = "t-status";
  const cancel = document.createElement("button");
  cancel.className = "t-cancel";
  cancel.textContent = "✕";
  cancel.title = "Cancel";
  cancel.addEventListener("click", () => cancelTransfer(t, "canceled"));
  li.append(name, status, cancel);
  t.row = li;
  t.rowStatus = status;
  t.rowCancel = cancel;
  ui.transferList.appendChild(li);
  ui.transfersPanel.hidden = false;
  updateRow(t);
  renderOverall();
}

function updateRow(t) {
  if (!t.row) return;
  const s = t.rowStatus;
  s.className = "t-status";
  switch (t.state) {
    case "offered":
      s.textContent = `waiting for ${peerName(t.peerId)}…`;
      break;
    case "active":
      s.textContent = `${fmtSize(t.bytes)} / ${fmtSize(t.size)} · ${fmtRate(rateOf(t))} · ${fmtETA(t)}`;
      break;
    case "done":
      s.textContent = t.dir === "send" ? `sent to ${peerName(t.peerId)} ✓` : "received ✓";
      s.classList.add("ok");
      break;
    default:
      s.textContent = t.state;
      s.classList.add("bad");
  }
  t.rowCancel.hidden = isDone(t);
}

function renderOverall() {
  let total = 0, done = 0, live = 0;
  for (const t of transfers.values()) {
    if (isDone(t)) continue;
    live++;
    total += t.size;
    done += t.bytes;
  }
  if (live === 0) {
    ui.overallPct.textContent = "";
    ui.overallBar.style.width = "0";
    let any = false;
    for (const t of transfers.values()) if (t.row) any = true;
    if (!any) ui.transfersPanel.hidden = true;
    return;
  }
  ui.transfersPanel.hidden = false;
  const pct = total > 0 ? Math.floor((done / total) * 100) : 0;
  ui.overallPct.textContent = `${pct}%`;
  ui.overallBar.style.width = `${pct}%`;
}

function fmtSize(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1048576) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1073741824) return `${(n / 1048576).toFixed(1)} MB`;
  return `${(n / 1073741824).toFixed(2)} GB`;
}

function fmtRate(bps) {
  return bps > 0 ? `${fmtSize(bps)}/s` : "—";
}

function fmtETA(t) {
  const r = rateOf(t);
  if (r <= 0) return "…";
  const sec = Math.ceil((t.size - t.bytes) / r);
  if (sec < 60) return `${sec}s`;
  return `${Math.floor(sec / 60)}m ${sec % 60}s`;
}

function uuidBytes(str) {
  const hex = str.replace(/-/g, "");
  const b = new Uint8Array(16);
  for (let i = 0; i < 16; i++) b[i] = parseInt(hex.substr(i * 2, 2), 16);
  return b;
}

function bytesToUUID(b) {
  const h = [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

function toast(msg, kind) {
  const el = document.createElement("div");
  el.className = `toast ${kind || ""}`;
  el.textContent = msg;
  ui.toasts.appendChild(el);
  setTimeout(() => {
    el.classList.add("leaving");
    setTimeout(() => el.remove(), 350);
  }, 4000);
}

ui.filePicker.addEventListener("change", () => {
  queueFiles(ui.filePicker.dataset.target, ui.filePicker.files);
  ui.filePicker.value = "";
});

for (const btn of [ui.netBtn, ui.emptyNetBtn]) {
  btn.addEventListener("click", () => {
    ui.roomCode.textContent = "····";
    ui.qr.replaceChildren();
    send("room-create", {});
    ui.netModal.showModal();
  });
}
ui.netClose.addEventListener("click", () => ui.netModal.close());

ui.joinForm.addEventListener("submit", (e) => {
  e.preventDefault();
  const code = ui.joinInput.value.trim().toUpperCase();
  if (!CODE_RE.test(code)) {
    toast("Codes are 4 letters or digits", "bad");
    return;
  }
  send("room-join", { code });
  ui.joinInput.value = "";
  ui.netModal.close();
});

ui.offerAccept.addEventListener("click", () => answerOffer(true));
ui.offerDecline.addEventListener("click", () => answerOffer(false));
ui.offerModal.addEventListener("cancel", (e) => {
  e.preventDefault();
  answerOffer(false);
});

ui.snipCancel.addEventListener("click", () => ui.snippetModal.close());
ui.snipSend.addEventListener("click", () => {
  const text = ui.snipText.value.trim();
  const to = ui.snippetModal.dataset.target;
  if (text && to) {
    send("snippet", { to, text });
    toast("Note sent", "ok");
  }
  ui.snippetModal.close();
});

ui.snipRecvClose.addEventListener("click", () => ui.snippetRecvModal.close());
ui.snipCopy.addEventListener("click", async () => {
  const text = ui.snipRecvText.textContent;
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = document.createElement("textarea");
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
  }
  toast("Copied", "ok");
  ui.snippetRecvModal.close();
});

window.addEventListener("dragover", (e) => e.preventDefault());
window.addEventListener("drop", (e) => e.preventDefault());

ui.field.addEventListener("drop", (e) => {
  if (peers.size === 1 && e.dataTransfer.files.length > 0) {
    e.preventDefault();
    queueFiles(peers.keys().next().value, e.dataTransfer.files);
  }
});

connect();
