import { state, emit, on, toast } from "./state.js";
import { $, coarsePointer } from "./util.js";
import { createSocket } from "./socket.js";
import * as transfers from "./transfers.js";
import { initRadar } from "./radar.js";
import { initPanel } from "./panel.js";
import { initDialogs } from "./dialogs.js";
import { initToasts } from "./toast.js";
import { identity, saveIdentity } from "./identity.js";
import { initNotify } from "./notify.js";
import { initHistory } from "./history.js";

const root = document.documentElement;
const themeBtn = $("themeBtn");
function setTheme(t, persist) {
  root.dataset.theme = t;
  themeBtn.setAttribute("aria-label", t === "dark" ? "Switch to light theme" : "Switch to dark theme");
  if (persist) try { localStorage.setItem("beam:theme", t); } catch {}
}
setTheme(root.dataset.theme, false);
themeBtn.addEventListener("click", () => setTheme(root.dataset.theme === "dark" ? "light" : "dark", true));
matchMedia("(prefers-color-scheme: light)").addEventListener("change", (e) => {
  try { if (localStorage.getItem("beam:theme")) return; } catch {}
  setTheme(e.matches ? "light" : "dark", false);
});

if (coarsePointer.matches) $("selfHint").textContent = "Tap a device to send it files";

let hashCode = null;
let joining = null;
const m = location.hash.slice(1).toUpperCase().match(/^[2-9A-HJ-NP-Z]{4}$/);
if (m) { hashCode = m[0]; history.replaceState(null, "", location.pathname); }
on("joining", (c) => { joining = c; });

const ERRORS = {
  "bad-code": "That code isn't active. Check it and try again.",
  "rate-limited": "Too many tries. Wait a moment and try again.",
  "unknown-peer": "That device isn't here anymore.",
  "bad-transfer": "That transfer is no longer valid.",
};

const room = {
  "room-state"(d) {
    state.self = d.self;
    if (!identity().name || !identity().emoji) saveIdentity({ name: d.self.name, emoji: d.self.emoji });
    state.peers.clear();
    for (const p of d.peers || []) state.peers.set(p.id, p);
    emit("self");
    emit("peers");
    if (joining) {
      state.code = joining;
      emit("code");
      emit("joined", joining);
      joining = null;
    }
    if (hashCode) {
      joining = hashCode;
      hashCode = null;
      socket.send("room-join", { code: joining });
    }
  },
  "peer-joined"(p) {
    if (state.peers.has(p.id)) return;
    state.peers.set(p.id, p);
    emit("peers");
    toast(`${p.name} ${p.emoji} is here`);
  },
  "peer-updated"(p) {
    if (state.self && p.id === state.self.id) {
      state.self = p;
      saveIdentity({ name: p.name, emoji: p.emoji });
      emit("self");
    } else if (state.peers.has(p.id)) {
      state.peers.set(p.id, p);
      emit("peers");
    }
  },
  "peer-left"(d) {
    const p = state.peers.get(d.id);
    state.peers.delete(d.id);
    transfers.forgetPeer(d.id);
    emit("peers");
    if (p) toast(`${p.name} left`);
  },
  "room-created"(d) {
    state.code = d.code;
    emit("code");
  },
  snippet(d) { emit("snippet", d); },
  error(d) {
    if (d.code === "bad-code" || d.code === "rate-limited") { joining = null; emit("join-failed"); }
    toast(ERRORS[d.code] || d.message || "Something went wrong", "bad");
  },
};

const conn = $("connState");
function setConn(onLine) {
  state.connected = onLine;
  conn.dataset.state = onLine ? "on" : "off";
  conn.setAttribute("aria-label", onLine ? "Connected" : "Reconnecting");
  if (!onLine) $("selfName").textContent = "…";
}

const socket = createSocket({
  params: () => identity(),
  onOpen() { setConn(true); },
  onClose() {
    const wasUp = state.connected;
    setConn(false);
    state.self = null;
    state.code = null;
    state.peers.clear();
    joining = null;
    emit("peers");
    emit("code");
    transfers.dropAll("connection lost");
    if (wasUp) toast("Connection lost, reconnecting…", "bad");
  },
  onMessage(type, data) {
    const h = room[type] || transfers.handlers[type];
    if (h) h(data);
  },
  onBinary: transfers.onChunk,
});
transfers.bindSocket(socket);

on("self", () => {
  $("selfName").textContent = state.self.name;
  $("selfEmoji").textContent = state.self.emoji;
});

initToasts();
initNotify();
initHistory();
initPanel();
const { openNote } = initDialogs(socket);
initRadar({ onNote: openNote });

window.__beam = { state, emit, transfers };
