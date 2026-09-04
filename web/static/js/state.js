export const state = {
  connected: false,
  self: null,
  code: null,
  codeExpires: 0,
  peers: new Map(),
  transfers: new Map(),
  offers: [],
  shared: null,
  drops: null,
};

const bus = new EventTarget();

export function emit(type, detail) {
  bus.dispatchEvent(new CustomEvent(type, { detail }));
}

export function on(type, fn) {
  bus.addEventListener(type, (e) => fn(e.detail));
}

export function peerName(id) {
  const p = state.peers.get(id);
  return p ? p.name : "that device";
}

export const isDone = (t) => t.state === "done" || t.state === "failed" || t.state === "declined" || t.state === "missed";

export const toast = (msg, kind = "info") => emit("toast", { msg, kind });
