import { state, emit, isDone, peerName, toast } from "./state.js";
import { uuidBytes, bytesToUUID } from "./util.js";
import { fileKind } from "./icons.js";
import { available as e2e, keypair, shared, seal, open } from "./crypto.js";
import { openSink, saveBlob } from "./sink.js";
import * as rtc from "./rtc.js";
import { previewOf, validPreview } from "./preview.js";

export { saveBlob };

const CHUNK = 64 * 1024;
const INITIAL_CREDITS = 16;
export const OFFER_TTL = 30000;

let socket = null;
const queues = new Map();

export const socketLink = {
  p2p: false,
  send: (type, data) => socket.send(type, data),
  sendRaw: (buf) => socket.sendRaw(buf),
  ready: (t) => socket.open && t.credits > 0,
  sent: (t) => { t.credits--; },
};

export function bindSocket(s) { socket = s; }

export function queueFiles(peerId, files) {
  if (!files || !files.length || !state.peers.has(peerId)) return;
  const q = queues.get(peerId) || [];
  let added = 0;
  for (const f of files) {
    if (f.size === 0) { toast(`"${f.name}" is empty, skipped`, "bad"); continue; }
    q.push(f);
    added++;
  }
  queues.set(peerId, q);
  if (added > 1) toast(`${added} files queued for ${peerName(peerId)}`);
  next(peerId);
}

export function forgetPeer(id) { queues.delete(id); }

async function next(peerId) {
  for (const t of state.transfers.values()) {
    if (t.dir === "send" && t.peerId === peerId && !isDone(t)) return;
  }
  const q = queues.get(peerId);
  if (!q || !q.length) return;
  if (!state.peers.has(peerId)) { q.length = 0; return; }
  const file = q.shift();
  const id = crypto.randomUUID();
  const t = {
    id, idBytes: uuidBytes(id), dir: "send", peerId, link: rtc.linkFor(peerId) || socketLink,
    name: file.name, size: file.size, mime: file.type, kind: fileKind(file.name, file.type),
    state: "offered", file, offset: 0, bytes: 0, credits: 0, pumping: false, seq: 0,
    priv: null, pub: "", key: null, sas: "", timer: 0,
    samples: [], lastPaint: 0, startedAt: 0, note: "",
  };
  state.transfers.set(id, t);
  emit("transfer:add", t);
  const [kp, preview] = await Promise.all([e2e ? keypair().catch(() => null) : null, previewOf(file)]);
  if (kp) { t.priv = kp.priv; t.pub = kp.pub; }
  if (t.state !== "offered") return;
  try {
    t.link.send("transfer-offer", { id, to: peerId, name: file.name, size: file.size, mime: file.type, key: t.pub || undefined, preview: preview || undefined });
  } catch {
    return end(t, "failed", "couldn't reach them");
  }
  if (t.link.p2p) t.timer = setTimeout(() => { if (t.state === "offered") cancel(t, "no answer in 30 seconds"); }, OFFER_TTL);
}

async function pump(t) {
  if (t.pumping) return;
  t.pumping = true;
  try {
    while (t.state === "active" && t.offset < t.size && t.link.ready(t)) {
      const end = Math.min(t.offset + CHUNK, t.size);
      let buf;
      try {
        buf = await t.file.slice(t.offset, end).arrayBuffer();
        if (t.key) buf = await seal(t.key, t.seq, t.idBytes, buf);
      } catch {
        cancel(t, "couldn't read the file");
        return;
      }
      if (t.state !== "active") return;
      const frame = new Uint8Array(16 + buf.byteLength);
      frame.set(t.idBytes, 0);
      frame.set(new Uint8Array(buf), 16);
      try { t.link.sendRaw(frame); } catch { cancel(t, "connection lost"); return; }
      t.link.sent(t);
      t.seq++;
      t.offset = t.bytes = end;
      sample(t);
      paint(t);
    }
  } finally {
    t.pumping = false;
  }
}

export function resume(link) {
  for (const t of state.transfers.values()) if (t.link === link && t.dir === "send" && t.state === "active") pump(t);
}

function sample(t) {
  t.samples.push([performance.now(), t.bytes]);
  if (t.samples.length > 40) t.samples.shift();
}

export function rateOf(t) {
  const s = t.samples;
  if (s.length < 2) return 0;
  const [t0, b0] = s[0];
  const [t1, b1] = s[s.length - 1];
  return t1 > t0 ? (b1 - b0) / ((t1 - t0) / 1000) : 0;
}

export function etaOf(t) {
  const r = rateOf(t);
  return r > 0 ? (t.size - t.bytes) / r : Infinity;
}

function paint(t) {
  const now = performance.now();
  if (now - t.lastPaint < 90 && t.bytes < t.size) return;
  t.lastPaint = now;
  emit("transfer:progress", t);
}

export function onChunk(data, link) {
  if (data.byteLength <= 16) return;
  const t = state.transfers.get(bytesToUUID(new Uint8Array(data, 0, 16)));
  if (!t || t.dir !== "recv" || t.state !== "active" || t.link !== link) return;
  const n = t.seq++;
  t.chain = t.chain.then(async () => {
    if (t.state !== "active") return;
    let buf = data.slice(16);
    if (t.key) buf = await open(t.key, n, t.idBytes, buf);
    t.bytes += buf.byteLength;
    if (t.bytes > t.size) throw new Error("overflow");
    await t.sink.write(buf);
    sample(t);
    paint(t);
    if (t.bytes >= t.size) await finishReceive(t);
  }).catch(() => {
    if (t.state === "active") { t.sink.abort(); cancel(t, t.key ? "integrity check failed" : "couldn't save the file"); }
  });
}

async function finishReceive(t) {
  if (t.state !== "active") return;
  try {
    Object.assign(t, await t.sink.close());
  } catch {
    return end(t, "failed", "couldn't save the file");
  }
  if (t.link.p2p) try { t.link.send("transfer-complete", { id: t.id, bytes: t.bytes }); } catch {}
  end(t, "done");
}

export async function answerOffer(accept) {
  const d = state.offers[0];
  if (!d || d.answering) return;
  if (!accept) {
    state.offers.shift();
    emit("offers");
    try { d.link.send("transfer-answer", { id: d.id, accept: false }); } catch {}
    return;
  }
  d.answering = true;
  const sink = await openSink(d.name, d.mime || "");
  if (state.offers[0] !== d) { sink.abort(); return; }
  let pub = "", key = null, sas = "";
  if (d.key && e2e) {
    try {
      const kp = await keypair();
      ({ key, sas } = await shared(kp.priv, kp.pub, d.key));
      pub = kp.pub;
    } catch { pub = ""; key = null; sas = ""; }
  }
  if (state.offers[0] !== d) { sink.abort(); return; }
  state.offers.shift();
  emit("offers");
  const t = {
    id: d.id, idBytes: uuidBytes(d.id), dir: "recv", peerId: d.from ? d.from.id : "", link: d.link,
    name: d.name, size: d.size, mime: d.mime || "", kind: fileKind(d.name, d.mime || ""),
    state: "active", bytes: 0, seq: 0, chain: Promise.resolve(), sink, key, sas,
    samples: [], lastPaint: 0, startedAt: performance.now(), note: "", blobUrl: null, saved: false,
  };
  state.transfers.set(d.id, t);
  try { d.link.send("transfer-answer", { id: d.id, accept: true, key: pub || undefined }); } catch { sink.abort(); return end(t, "failed", "connection lost"); }
  emit("transfer:add", t);
}

export function cancel(t, note = "canceled") {
  if (isDone(t)) return;
  try { t.link.send("transfer-cancel", { id: t.id, reason: note }); } catch {}
  end(t, "failed", note);
}

function end(t, st, note = "") {
  t.state = st;
  t.note = note;
  clearTimeout(t.timer);
  if (st === "done") {
    toast(t.dir === "send" ? `Sent ${t.name} to ${peerName(t.peerId)}` : `Received ${t.name}`, "ok");
  } else if (note) {
    if (t.sink) t.sink.abort();
    toast(`${t.name}: ${note}`, "bad");
  }
  emit("transfer:state", t);
  setTimeout(() => {
    state.transfers.delete(t.id);
    emit("transfer:remove", t);
  }, st === "done" ? 7000 : 9000);
  if (t.dir === "send") next(t.peerId);
}

export function dropLink(link, note) {
  for (const t of state.transfers.values()) if (t.link === link && !isDone(t)) end(t, "failed", note);
  const before = state.offers.length;
  for (let i = state.offers.length - 1; i >= 0; i--) if (state.offers[i].link === link) state.offers.splice(i, 1);
  if (before !== state.offers.length) emit("offers");
}

export function dropAll(note) {
  for (const t of state.transfers.values()) if (!isDone(t)) end(t, "failed", note);
  state.offers.length = 0;
  queues.clear();
  emit("offers");
}

const REASONS = {
  "offer-timeout": "no answer in 30 seconds",
  "peer-disconnected": "they disconnected",
  "peer-left-room": "they left the room",
  "receiver-backpressure": "their connection stalled",
  "sender-backpressure": "your connection stalled",
  "server-shutdown": "the server shut down",
};

function dropOffer(id, link) {
  const i = state.offers.findIndex((o) => o.id === id && o.link === link);
  if (i < 0) return null;
  const [d] = state.offers.splice(i, 1);
  emit("offers");
  return d;
}

function mine(d, link) {
  const t = state.transfers.get(d.id);
  return t && t.link === link ? t : null;
}

export const handlers = {
  "transfer-offer"(d, link) {
    d.link = link;
    d.receivedAt = performance.now();
    d.name = String(d.name || "").split(/[\\/]/).pop().slice(0, 255) || "file";
    d.size = Number(d.size);
    if (!validPreview(d.preview)) delete d.preview;
    if (!(d.size > 0) || !isFinite(d.size) || state.transfers.has(d.id) || state.offers.some((o) => o.id === d.id)) return;
    state.offers.push(d);
    emit("offers");
  },
  async "transfer-answer"(d, link) {
    const t = mine(d, link);
    if (!t || t.dir !== "send" || t.state !== "offered") return;
    if (!d.accept) return end(t, "declined", `${peerName(t.peerId)} declined`);
    clearTimeout(t.timer);
    if (t.pub && d.key) {
      try { ({ key: t.key, sas: t.sas } = await shared(t.priv, t.pub, d.key)); } catch { return cancel(t, "key exchange failed"); }
    }
    if (t.state !== "offered") return;
    t.state = "active";
    t.credits = INITIAL_CREDITS;
    t.startedAt = performance.now();
    emit("transfer:state", t);
    pump(t);
  },
  "flow-credit"(d, link) {
    const t = mine(d, link);
    if (!t || t.dir !== "send" || t.state !== "active") return;
    t.credits += d.n;
    paint(t);
    pump(t);
  },
  "transfer-complete"(d, link) {
    const t = mine(d, link);
    if (t && t.dir === "send" && !isDone(t)) end(t, "done");
  },
  "transfer-failed"(d, link) {
    if (dropOffer(d.id, link)) return;
    const t = mine(d, link);
    if (t && !isDone(t)) end(t, "failed", REASONS[d.reason] || d.reason);
  },
  "transfer-cancel"(d, link) {
    const o = dropOffer(d.id, link);
    if (o) return toast(`${o.from ? o.from.name : "They"} withdrew ${o.name}`);
    const t = mine(d, link);
    if (t && !isDone(t)) end(t, "failed", `${peerName(t.peerId)} canceled`);
  },
};
