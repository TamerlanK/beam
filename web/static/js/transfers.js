import { state, emit, isDone, peerName, toast } from "./state.js";
import { uuidBytes, bytesToUUID } from "./util.js";
import { fileKind } from "./icons.js";

const CHUNK = 64 * 1024;
const INITIAL_CREDITS = 16;
export const OFFER_TTL = 30000;

let socket = null;
const queues = new Map();

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
  for (const t of state.transfers.values()) {
    if (t.dir === "send" && t.peerId === peerId && !isDone(t)) return;
  }
  const q = queues.get(peerId);
  if (!q || !q.length) return;
  if (!state.peers.has(peerId)) { q.length = 0; return; }
  const file = q.shift();
  const id = crypto.randomUUID();
  const t = {
    id, idBytes: uuidBytes(id), dir: "send", peerId,
    name: file.name, size: file.size, mime: file.type, kind: fileKind(file.name, file.type),
    state: "offered", file, offset: 0, bytes: 0, credits: 0, pumping: false,
    samples: [], lastPaint: 0, startedAt: 0, note: "",
  };
  state.transfers.set(id, t);
  socket.send("transfer-offer", { id, to: peerId, name: file.name, size: file.size, mime: file.type });
  emit("transfer:add", t);
}

async function pump(t) {
  if (t.pumping) return;
  t.pumping = true;
  try {
    while (t.state === "active" && t.credits > 0 && t.offset < t.size && socket.open) {
      const end = Math.min(t.offset + CHUNK, t.size);
      let buf;
      try {
        buf = await t.file.slice(t.offset, end).arrayBuffer();
      } catch {
        cancel(t, "couldn't read the file");
        return;
      }
      if (t.state !== "active") return;
      const frame = new Uint8Array(16 + buf.byteLength);
      frame.set(t.idBytes, 0);
      frame.set(new Uint8Array(buf), 16);
      socket.sendRaw(frame);
      t.credits--;
      t.offset = t.bytes = end;
      sample(t);
      paint(t);
    }
  } finally {
    t.pumping = false;
  }
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

export function onChunk(data) {
  if (data.byteLength <= 16) return;
  const t = state.transfers.get(bytesToUUID(new Uint8Array(data, 0, 16)));
  if (!t || t.dir !== "recv" || t.state !== "active") return;
  t.parts.push(data.slice(16));
  t.bytes += data.byteLength - 16;
  sample(t);
  paint(t);
  if (t.bytes >= t.size) finishReceive(t);
}

function finishReceive(t) {
  if (t.state !== "active") return;
  const blob = new Blob(t.parts, { type: t.mime || "application/octet-stream" });
  t.parts = [];
  t.blobUrl = URL.createObjectURL(blob);
  saveBlob(t);
  end(t, "done");
}

export function saveBlob(t) {
  if (!t.blobUrl) return;
  const a = document.createElement("a");
  a.href = t.blobUrl;
  a.download = t.name;
  document.body.append(a);
  a.click();
  a.remove();
}

export function answerOffer(accept) {
  const d = state.offers.shift();
  if (!d) return;
  socket.send("transfer-answer", { id: d.id, accept });
  if (accept) {
    const t = {
      id: d.id, dir: "recv", peerId: d.from ? d.from.id : "",
      name: d.name, size: d.size, mime: d.mime || "", kind: fileKind(d.name, d.mime || ""),
      state: "active", bytes: 0, parts: [], samples: [], lastPaint: 0,
      startedAt: performance.now(), note: "", blobUrl: null,
    };
    state.transfers.set(d.id, t);
    emit("transfer:add", t);
  }
  emit("offers");
}

export function cancel(t, note = "canceled") {
  if (isDone(t)) return;
  socket.send("transfer-cancel", { id: t.id, reason: note });
  end(t, "failed", note);
}

function end(t, st, note = "") {
  t.state = st;
  t.note = note;
  t.parts = [];
  if (st === "done") {
    toast(t.dir === "send" ? `Sent ${t.name} to ${peerName(t.peerId)}` : `Received ${t.name}`, "ok");
  } else if (note) {
    toast(`${t.name}: ${note}`, "bad");
  }
  emit("transfer:state", t);
  setTimeout(() => {
    state.transfers.delete(t.id);
    emit("transfer:remove", t);
  }, st === "done" ? 7000 : 9000);
  if (t.dir === "send") next(t.peerId);
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

function dropOffer(id) {
  const i = state.offers.findIndex((o) => o.id === id);
  if (i < 0) return null;
  const [d] = state.offers.splice(i, 1);
  emit("offers");
  return d;
}

export const handlers = {
  "transfer-offer"(d) {
    d.receivedAt = performance.now();
    state.offers.push(d);
    emit("offers");
  },
  "transfer-answer"(d) {
    const t = state.transfers.get(d.id);
    if (!t || t.dir !== "send" || t.state !== "offered") return;
    if (!d.accept) return end(t, "declined", `${peerName(t.peerId)} declined`);
    t.state = "active";
    t.credits = INITIAL_CREDITS;
    t.startedAt = performance.now();
    emit("transfer:state", t);
    pump(t);
  },
  "flow-credit"(d) {
    const t = state.transfers.get(d.id);
    if (!t || t.dir !== "send" || t.state !== "active") return;
    t.credits += d.n;
    paint(t);
    pump(t);
  },
  "transfer-complete"(d) {
    const t = state.transfers.get(d.id);
    if (!t || isDone(t)) return;
    if (t.dir === "recv") finishReceive(t);
    else end(t, "done");
  },
  "transfer-failed"(d) {
    if (dropOffer(d.id)) return;
    const t = state.transfers.get(d.id);
    if (t && !isDone(t)) end(t, "failed", REASONS[d.reason] || d.reason);
  },
  "transfer-cancel"(d) {
    const o = dropOffer(d.id);
    if (o) return toast(`${o.from ? o.from.name : "They"} withdrew ${o.name}`);
    const t = state.transfers.get(d.id);
    if (t && !isDone(t)) end(t, "failed", `${peerName(t.peerId)} canceled`);
  },
};
