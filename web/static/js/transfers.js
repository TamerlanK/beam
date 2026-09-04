import { state, emit, on, isDone, peerName, toast } from "./state.js";
import { uuid, uuidBytes, bytesToUUID } from "./util.js";
import { fileKind } from "./icons.js";
import { available as e2e, keypair, shared, seal, open, randomKey, importSecret } from "./crypto.js";
import { openSink, reopenSink, reapParts, saveBlob } from "./sink.js";
import * as rtc from "./rtc.js";
import { previewOf, validPreview } from "./preview.js";
import { device, fits, secretFor, forgetSecret, ttlText } from "./drops.js";
import * as store from "./store.js";

export { saveBlob };

const CHUNK = 64 * 1024;
const INITIAL_CREDITS = 16;
export const OFFER_TTL = 30000;
const RESUME_TTL = 120000;
const MISSED_TTL = 60000;
const CREATE_TTL = 10000;
const SAVE_EVERY = 1000;
const RECORD_TTL = 24 * 3600 * 1000;
const MAX_INFLIGHT = 24; // ponytail: hub refuses more than 32 live transfers per sender

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

function next(peerId) {
  const q = queues.get(peerId);
  if (!q || !q.length) return;
  if (!state.peers.has(peerId)) { q.length = 0; return; }
  let live = 0;
  for (const t of state.transfers.values()) if (t.dir === "send" && !isDone(t)) live++;
  while (q.length && live++ < MAX_INFLIGHT) offer(peerId, q.shift());
}

function newSend(peerId, file, extra) {
  const id = uuid();
  return {
    id, idBytes: uuidBytes(id), dir: "send", peerId, link: socketLink,
    name: file.name, size: file.size, mime: file.type, kind: fileKind(file.name, file.type),
    state: "offered", file, offset: 0, bytes: 0, credits: 0, pumping: false, seq: 0, gen: 0, reofferedAt: -Infinity,
    priv: null, pub: "", key: null, sas: "", timer: 0, drop: "", peerName: "", ttl: 0,
    handle: file.handle || null, durable: false, needsClick: false, preparing: false, savedAt: 0, savedOffset: -1,
    samples: [], hist: [], lastPaint: 0, startedAt: 0, note: "", ...extra,
  };
}

// Durable resume: what a reload must not lose lives in IndexedDB. The sender
// keeps the file handle, key and offset; the receiver keeps the key and how
// many bytes its part file holds. The protocol already resumes by offset, so
// the server needs nothing.

const recordKey = (t) => `${t.dir === "send" ? "tx" : "rx"}:${t.id}`;

function nameOf(t) {
  const p = state.peers.get(t.peerId);
  return p ? p.name : t.peerName;
}

function saveSend(t) {
  t.savedAt = performance.now();
  t.savedOffset = t.offset;
  store.put(recordKey(t), {
    id: t.id, dir: "send", peerId: t.peerId, peerName: nameOf(t), name: t.name, size: t.size, mime: t.mime, kind: t.kind,
    handle: t.handle, key: t.key, pub: t.pub, sas: t.sas, offset: t.offset, at: Date.now(),
  }).catch(() => {});
}

function saveRecv(t) {
  t.savedBytes = t.sink.flushed;
  store.put(recordKey(t), {
    id: t.id, dir: "recv", peerId: t.peerId, peerName: nameOf(t), name: t.name, size: t.size, mime: t.mime, kind: t.kind,
    key: t.key, pub: t.pub, sas: t.sas, bytes: t.sink.flushed, target: t.sink.target, at: Date.now(),
  }).catch(() => {});
}

function forget(t) {
  if (t.durable) store.del(recordKey(t)).catch(() => {});
}

function revive(r) {
  const base = {
    id: r.id, idBytes: uuidBytes(r.id), dir: r.dir, peerId: r.peerId, peerName: r.peerName, link: socketLink,
    name: r.name, size: r.size, mime: r.mime || "", kind: r.kind || fileKind(r.name, r.mime || ""), state: "paused", durable: true,
    key: r.key, pub: r.pub || "", sas: r.sas || "", timer: 0, drop: "", preview: "", savedAt: 0, savedOffset: -1, needsClick: false, preparing: false,
    samples: [], hist: [], lastPaint: 0, startedAt: 0, note: "",
  };
  if (r.dir === "send") {
    return { ...base, file: null, handle: r.handle, offset: r.offset, bytes: r.offset, credits: 0, pumping: false, seq: r.offset / CHUNK, gen: 0, reofferedAt: -Infinity, priv: null, ttl: 0 };
  }
  return { ...base, bytes: r.bytes, seq: r.bytes / CHUNK, chain: Promise.resolve(), sink: null, target: r.target || null, blobUrl: null, saved: false };
}

// Called whenever this tab becomes the device's live tab: bring back every
// unfinished transfer as a paused row and reconcile with what is in memory.
export async function restore() {
  let recs = [];
  try { recs = [...(await store.list("tx:")), ...(await store.list("rx:"))]; } catch {}
  const keep = new Set();
  for (const r of recs) {
    if (!r || !r.id || !(r.size > 0) || Date.now() - r.at > RECORD_TTL) {
      if (r && r.id) store.del(`${r.dir === "send" ? "tx" : "rx"}:${r.id}`).catch(() => {});
      continue;
    }
    keep.add(r.id);
    const t = state.transfers.get(r.id);
    if (t) {
      if (t.dir === "recv" && !t.sink) { t.bytes = r.bytes; t.seq = r.bytes / CHUNK; }
      continue;
    }
    const fresh = revive(r);
    state.transfers.set(r.id, fresh);
    emit("transfer:add", fresh);
  }
  for (const t of state.transfers.values()) if (t.durable && !isDone(t) && !keep.has(t.id)) end(t, "failed", "");
  reapParts(keep).catch(() => {});
}

// This tab is handing over to another: let go of part-file locks, keep the records.
export function release() {
  for (const t of state.transfers.values()) {
    if (t.durable && t.dir === "recv" && t.sink && t.sink.detach) { t.sink.detach(); t.sink = null; }
  }
}

// A restored sender has a handle but no File yet; reading it may need a click.
async function prepare(t) {
  if (t.file || !t.handle || t.preparing) return;
  t.preparing = true;
  try {
    if ((await t.handle.queryPermission({ mode: "read" })) !== "granted") {
      if (!t.needsClick) { t.needsClick = true; emit("transfer:state", t); }
      return;
    }
    const f = await t.handle.getFile();
    if (f.size !== t.size) return end(t, "failed", "the file changed on disk");
    t.file = f;
    t.needsClick = false;
    emit("transfer:state", t);
    retryPaused();
  } catch {
    end(t, "failed", "couldn't reopen the file");
  } finally {
    t.preparing = false;
  }
}

export async function resumeClick(t) {
  if (t.dir !== "send" || !t.handle || t.file) return;
  try { if ((await t.handle.requestPermission({ mode: "read" })) !== "granted") return; } catch { return; }
  t.needsClick = false;
  t.reofferedAt = -Infinity;
  prepare(t);
}

export async function saveClick(t) {
  if (!t.save) return;
  const save = t.save;
  t.save = null;
  Object.assign(t, await save());
  emit("transfer:state", t);
}

async function offer(peerId, file) {
  const t = newSend(peerId, file, { link: rtc.linkFor(peerId) || socketLink });
  const id = t.id;
  state.transfers.set(id, t);
  emit("transfer:add", t);
  const [kp, preview] = await Promise.all([e2e ? keypair().catch(() => null) : null, previewOf(file)]);
  if (kp) { t.priv = kp.priv; t.pub = kp.pub; }
  t.preview = preview;
  if (t.state !== "offered") return;
  try {
    t.link.send("transfer-offer", { id, to: peerId, name: file.name, size: file.size, mime: file.type, key: t.pub || undefined, preview: preview || undefined });
  } catch {
    return end(t, "failed", "couldn't reach them");
  }
  if (t.link.p2p) t.timer = setTimeout(() => {
    if (t.state !== "offered") return;
    if (!canLeave(t)) return cancel(t, "no answer in 30 seconds");
    try { t.link.send("transfer-cancel", { id: t.id, reason: "no answer in 30 seconds" }); } catch {}
    miss(t);
  }, OFFER_TTL);
}

// Nobody answered: keep the row around so the sender can leave it for later.
function miss(t) {
  t.state = "missed";
  t.note = "no answer";
  emit("transfer:state", t);
  t.timer = setTimeout(() => { if (t.state === "missed") end(t, "failed", ""); }, MISSED_TTL);
  next(t.peerId);
}

// Drops: the server holds the sealed file until the addressee picks it up.

export function canLeave(t) {
  if (t.dir !== "send" || t.drop || !fits(t.size) || (t.state !== "offered" && t.state !== "missed")) return false;
  const p = state.peers.get(t.peerId);
  return !!(p && p.pub);
}

// Turn a pending offer into a drop for that device: the key is agreed with
// the device's long-lived public key, so only it can ever open the file.
export async function leaveFor(t) {
  if (!canLeave(t)) return;
  const peer = state.peers.get(t.peerId);
  if (t.state === "offered") try { t.link.send("transfer-cancel", { id: t.id, reason: "left-for-later" }); } catch {}
  clearTimeout(t.timer);
  Object.assign(t, { drop: "device", state: "offered", link: socketLink, peerName: peer.name, credits: 0, offset: 0, bytes: 0, seq: 0, samples: [], note: "" });
  t.gen++;
  emit("transfer:state", t);
  try {
    const kp = await keypair();
    t.priv = kp.priv; t.pub = kp.pub;
    t.key = (await shared(kp.priv, kp.pub, peer.pub)).key;
  } catch { return end(t, "failed", "couldn't set up encryption"); }
  if (t.state !== "offered") return;
  createDrop(t, { to: t.peerId, key: t.pub });
}

// A drop anyone can pick up once: the key is random and travels only in
// the link's fragment, which browsers never send to the server.
export async function leaveLink(file) {
  if (!fits(file.size)) return;
  const t = newSend("", file, { drop: "link", peerName: "anyone with the link" });
  state.transfers.set(t.id, t);
  emit("transfer:add", t);
  try { ({ key: t.key, secret: t.secret } = await randomKey()); } catch { return end(t, "failed", "couldn't set up encryption"); }
  t.preview = await previewOf(file);
  if (t.state !== "offered") return;
  createDrop(t, {});
}

function createDrop(t, extra) {
  try {
    socketLink.send("drop-create", { id: t.id, name: t.name, size: t.size, mime: t.mime, preview: t.preview || undefined, ...extra });
  } catch { return end(t, "failed", "connection lost"); }
  t.timer = setTimeout(() => { if (t.state === "offered") end(t, "failed", "the server didn't answer"); }, CREATE_TTL);
}

export function dismiss(t) {
  if (t.state === "missed") end(t, "failed", "");
}

async function pump(t) {
  if (t.pumping) return;
  t.pumping = true;
  try {
    while (t.state === "active" && t.offset < t.size && t.link.ready(t)) {
      const gen = t.gen, off = t.offset, seq = t.seq;
      const end = Math.min(off + CHUNK, t.size);
      let buf;
      try {
        buf = await t.file.slice(off, end).arrayBuffer();
        if (t.key) buf = await seal(t.key, seq, t.idBytes, buf);
      } catch {
        cancel(t, "couldn't read the file");
        return;
      }
      if (t.gen !== gen) continue;
      if (t.state !== "active") return;
      const frame = new Uint8Array(16 + buf.byteLength);
      frame.set(t.idBytes, 0);
      frame.set(new Uint8Array(buf), 16);
      try { t.link.sendRaw(frame); } catch { cancel(t, "connection lost"); return; }
      t.link.sent(t);
      t.seq = seq + 1;
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

function record(t) {
  t.hist.push(rateOf(t));
  if (t.hist.length > 60) t.hist.shift();
}

setInterval(() => {
  const now = performance.now();
  for (const t of state.transfers.values()) {
    if (t.state !== "active") continue;
    record(t);
    emit("transfer:progress", t);
    if (t.durable && t.dir === "send" && t.offset !== t.savedOffset && now - t.savedAt > SAVE_EVERY) saveSend(t);
  }
  retryPaused();
}, 500);

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
    if (t.durable && t.sink.flushed !== t.savedBytes) saveRecv(t);
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

function sameSender(a, b) {
  return a.link === b.link && (a.from ? a.from.id : "") === (b.from ? b.from.id : "");
}

// Pending offers from the same peer as the first one: answered together.
export function offerGroup() {
  const first = state.offers[0];
  return first ? state.offers.filter((o) => sameSender(o, first)) : [];
}

export async function answerOffer(accept) {
  const group = offerGroup().filter((o) => !o.answering);
  if (!group.length) return;
  if (!accept) {
    for (const d of group) {
      state.offers.splice(state.offers.indexOf(d), 1);
      forgetSecret(d.id);
      try { d.link.send(d.drop ? "drop-cancel" : "transfer-answer", d.drop ? { id: d.id } : { id: d.id, accept: false }); } catch {}
    }
    emit("offers");
    return;
  }
  for (const d of group) d.answering = true;
  let dir;
  if (group.length > 1 && typeof showDirectoryPicker === "function") {
    try { dir = await showDirectoryPicker({ mode: "readwrite" }); } catch { dir = null; }
  }
  for (const d of group) if (state.offers.includes(d)) await acceptOne(d, dir);
}

async function acceptOne(d, dir) {
  const sink = await openSink(d.id, d.name, d.size, d.mime || "", dir);
  if (!state.offers.includes(d)) { sink.abort(); return; }
  let pub = "", key = null, sas = "";
  if (d.drop) {
    try { key = d.anyone ? await importSecret(d.secret) : (await shared(device.priv, device.pub, d.key)).key; } catch { key = null; }
    if (!key) {
      sink.abort();
      if (dropOffer(d.id, d.link)) toast(`Can't decrypt ${d.name} on this device`, "bad");
      return;
    }
  } else if (d.key && e2e) {
    try {
      const kp = await keypair();
      ({ key, sas } = await shared(kp.priv, kp.pub, d.key));
      pub = kp.pub;
    } catch { pub = ""; key = null; sas = ""; }
  }
  if (!state.offers.includes(d)) { sink.abort(); return; }
  state.offers.splice(state.offers.indexOf(d), 1);
  emit("offers");
  const t = {
    id: d.id, idBytes: uuidBytes(d.id), dir: "recv", peerId: d.from ? d.from.id : "", link: d.link,
    name: d.name, size: d.size, mime: d.mime || "", kind: d.kind, drop: d.drop ? "device" : "", peerName: d.from ? d.from.name : "",
    state: "active", bytes: 0, seq: 0, chain: Promise.resolve(), sink, key, sas, pub,
    durable: !d.drop && sink.durable === true, target: sink.target || null, savedBytes: -1, needsClick: false,
    samples: [], hist: [], lastPaint: 0, startedAt: performance.now(), note: "", blobUrl: null, saved: false, preview: d.preview || "",
  };
  state.transfers.set(d.id, t);
  try {
    if (d.drop) d.link.send("drop-accept", { id: d.id });
    else d.link.send("transfer-answer", { id: d.id, accept: true, key: pub || undefined });
  } catch { sink.abort(); return end(t, "failed", "connection lost"); }
  emit("transfer:add", t);
  if (t.durable) saveRecv(t);
}

export function cancel(t, note = "canceled") {
  if (isDone(t)) return;
  if (t.state !== "paused") try { t.link.send(t.drop ? "drop-cancel" : "transfer-cancel", { id: t.id, reason: note }); } catch {}
  end(t, "failed", note);
}

function doneToast(t) {
  if (t.drop === "link") return `Link ready for ${t.name}`;
  if (t.drop && t.dir === "send") return `Left ${t.name} for ${t.peerName} · they have ${ttlText(t.ttl)}`;
  return t.dir === "send" ? `Sent ${t.name} to ${peerName(t.peerId)}` : `Received ${t.name}`;
}

function end(t, st, note = "") {
  const quiet = t.state === "missed";
  if (t.state === "active" && t.hist.length) record(t);
  t.state = st;
  t.note = note;
  clearTimeout(t.timer);
  forgetSecret(t.id);
  forget(t);
  if (st === "done") {
    toast(doneToast(t), "ok");
  } else if (note && !quiet) {
    if (t.sink) t.sink.abort();
    toast(`${t.name}: ${note}`, "bad");
  }
  emit("transfer:state", t);
  setTimeout(() => {
    if (state.transfers.get(t.id) !== t) return;
    state.transfers.delete(t.id);
    emit("transfer:remove", t);
  }, st === "done" ? (t.save || (t.drop && t.dir === "send") ? 60000 : 7000) : 9000);
  if (t.dir === "send") next(t.peerId);
}

function arm(t) {
  clearTimeout(t.timer);
  if (t.durable) return;
  t.timer = setTimeout(() => { if (t.state === "paused") end(t, "failed", "couldn't resume within 2 minutes"); }, RESUME_TTL);
}

function pause(t) {
  if (t.state !== "active" || (t.drop && t.dir === "send")) return false;
  if (t.hist.length) record(t);
  t.state = "paused";
  t.gen++;
  t.credits = 0;
  t.samples = [];
  arm(t);
  emit("transfer:state", t);
  return true;
}

export function pauseLink(link) {
  let paused = 0;
  for (const t of state.transfers.values()) {
    if (t.link !== link) continue;
    if (pause(t)) paused++;
    else if (t.state === "offered") end(t, "failed", "direct connection lost");
  }
  const before = state.offers.length;
  for (let i = state.offers.length - 1; i >= 0; i--) if (state.offers[i].link === link) state.offers.splice(i, 1);
  if (before !== state.offers.length) emit("offers");
  if (paused && socket && socket.open) {
    toast("Direct link lost, resuming through the server…");
    retryPaused();
  }
}

export function pauseAll() {
  let paused = 0;
  for (const t of state.transfers.values()) {
    if (pause(t)) paused++;
    else if (t.state === "offered" || (t.drop && t.dir === "send" && !isDone(t))) end(t, "failed", "connection lost");
  }
  state.offers.length = 0;
  queues.clear();
  emit("offers");
  return paused;
}

function retryPaused() {
  if (!socket || !socket.open) return;
  const now = performance.now();
  for (const t of state.transfers.values()) {
    if (t.dir !== "send" || t.state !== "paused" || !state.peers.has(t.peerId) || now - t.reofferedAt < OFFER_TTL) continue;
    if (!t.file) { prepare(t); continue; }
    t.link = rtc.linkFor(t.peerId) || socketLink;
    t.reofferedAt = now;
    const hint = Math.min(t.offset, Math.floor((t.size - 1) / CHUNK) * CHUNK);
    try {
      t.link.send("transfer-offer", { id: t.id, to: t.peerId, name: t.name, size: t.size, mime: t.mime, key: t.pub || undefined, offset: hint });
    } catch {}
  }
}
on("peers", retryPaused);

function resumable(t, d) {
  return t.dir === "recv" && (t.state === "paused" || t.state === "active") && !!d.from && t.peerId === d.from.id && t.name === d.name && t.size === d.size;
}

async function resumeReceive(t, link, hint = t.bytes) {
  clearTimeout(t.timer);
  t.link = link;
  t.state = "paused";
  await t.chain.catch(() => {});
  if (isDone(t) || t.link !== link) return;
  if (!t.sink) {
    try { t.sink = await reopenSink(t.id, t.name, t.mime, t.target); } catch { return end(t, "failed", "couldn't reopen the saved part"); }
    if (isDone(t) || t.link !== link) return;
  }
  // The sender's hint may sit below what we hold (it saves its offset lazily);
  // the server only accepts an answer at or below the hint, so rewind to it.
  const off = Math.min(t.bytes, Math.max(0, Math.floor(hint / CHUNK) * CHUNK));
  if (off < t.bytes) t.sink.seek(off);
  t.bytes = off;
  t.chain = Promise.resolve();
  t.seq = off / CHUNK;
  t.samples = [];
  try {
    if (t.drop) link.send("drop-accept", { id: t.id, offset: off });
    else link.send("transfer-answer", { id: t.id, accept: true, key: t.pub || undefined, offset: off });
  } catch {
    return arm(t);
  }
  t.state = "active";
  emit("transfer:state", t);
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

function normalize(d, link) {
  d.link = link;
  d.receivedAt = performance.now();
  d.name = String(d.name || "").split(/[\\/]/).pop().slice(0, 255) || "file";
  d.size = Number(d.size);
  d.kind = fileKind(d.name, d.mime || "");
  if (!validPreview(d.preview)) delete d.preview;
  return d.size > 0 && isFinite(d.size);
}

export const handlers = {
  "transfer-offer"(d, link) {
    if (!normalize(d, link)) return;
    const t = state.transfers.get(d.id);
    if (t) {
      if (resumable(t, d)) resumeReceive(t, link, Number(d.offset) || 0);
      return;
    }
    if (state.offers.some((o) => o.id === d.id)) return;
    state.offers.push(d);
    emit("offers");
  },
  async "transfer-answer"(d, link) {
    const t = mine(d, link);
    if (!t || t.drop || t.dir !== "send" || (t.state !== "offered" && t.state !== "paused")) return;
    if (!d.accept) return end(t, "declined", `${peerName(t.peerId)} declined`);
    clearTimeout(t.timer);
    if (t.pub && d.key && !t.key) {
      try { ({ key: t.key, sas: t.sas } = await shared(t.priv, t.pub, d.key)); } catch { return cancel(t, "key exchange failed"); }
    }
    if (t.state !== "offered" && t.state !== "paused") return;
    const off = Number(d.offset) || 0;
    if (off < 0 || off > t.offset || off % CHUNK) return cancel(t, "bad resume offset");
    t.offset = t.bytes = off;
    t.seq = off / CHUNK;
    t.samples = [];
    t.state = "active";
    t.credits = INITIAL_CREDITS;
    if (!t.startedAt) t.startedAt = performance.now();
    if (t.handle && !t.durable) { t.durable = true; saveSend(t); }
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
    if (!t || isDone(t)) return;
    if (d.reason === "peer-disconnected" && pause(t)) return toast("Connection lost, resuming…", "bad");
    if (t.state === "paused" && (d.reason === "peer-disconnected" || d.reason === "offer-timeout")) { t.reofferedAt = -Infinity; return; }
    if (d.reason === "offer-timeout" && canLeave(t)) return miss(t);
    end(t, "failed", REASONS[d.reason] || d.reason);
  },
  "transfer-cancel"(d, link) {
    const o = dropOffer(d.id, link);
    if (o) { if (d.reason !== "left-for-later") toast(`${o.from ? o.from.name : "They"} withdrew ${o.name}`); return; }
    const t = mine(d, link);
    if (t && !isDone(t)) end(t, "failed", `${peerName(t.peerId)} canceled`);
  },
  "drop-created"(d, link) {
    const t = mine(d, link);
    if (!t || !t.drop || t.state !== "offered") return;
    clearTimeout(t.timer);
    t.ttl = Number(d.expiresIn) || 0;
    t.state = "active";
    t.credits = INITIAL_CREDITS;
    t.startedAt = performance.now();
    emit("transfer:state", t);
    pump(t);
  },
  "drop-stored"(d, link) {
    const t = mine(d, link);
    if (!t || !t.drop || t.state !== "active") return;
    t.ttl = Number(d.expiresIn) || t.ttl;
    end(t, "done");
    if (t.drop === "link") emit("drop:link", t);
  },
  "drop-waiting"(d, link) {
    if (!normalize(d, link)) return;
    d.drop = true;
    d.ttl = Math.max(1, Number(d.expiresIn) || 0) * 1000;
    if (d.anyone) { d.secret = secretFor(d.id); if (!d.secret) return; }
    const t = state.transfers.get(d.id);
    if (t) {
      if (!isDone(t)) { if (t.dir === "recv" && t.drop && t.state === "paused") resumeReceive(t, link); return; }
      state.transfers.delete(t.id);
      emit("transfer:remove", t);
    }
    const o = state.offers.find((o) => o.id === d.id);
    if (o) { o.receivedAt = d.receivedAt; o.ttl = d.ttl; emit("offers"); return; }
    state.offers.push(d);
    emit("offers");
  },
  "drop-gone"(d, link) {
    const o = dropOffer(d.id, link);
    if (o) {
      forgetSecret(d.id);
      if (d.reason === "expired") toast(`${o.name} expired before you picked it up`);
      else if (d.reason === "canceled") toast(`${o.from ? o.from.name : "They"} took back ${o.name}`);
      return;
    }
    const t = mine(d, link);
    if (!t || !t.drop) return;
    if (t.dir === "send") {
      if (t.state !== "done" || !["picked-up", "expired", "canceled"].includes(d.reason)) return;
      t.note = d.reason;
      emit("transfer:state", t);
      if (d.reason === "picked-up") toast(`${t.drop === "link" ? "Someone" : t.peerName} picked up ${t.name}`, "ok");
      else if (d.reason === "canceled") toast(`${t.drop === "link" ? "Someone" : t.peerName} declined ${t.name}`, "bad");
      return;
    }
    // picked-up is our own pickup completing; the sink may still be flushing the tail.
    if (!isDone(t) && d.reason !== "picked-up") end(t, "failed", d.reason === "expired" ? "expired before you could finish" : "taken back by the sender");
  },
};
