import { state, emit, on, toast, isDone } from "./state.js";
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
import { initRTC, onSignal, reset as resetRTC } from "./rtc.js";
import { initShare } from "./share.js";
import { initTabs, takeover, isLeader, debug as tabsDebug } from "./tabs.js";
import { initDrops, device, parseLink, remember } from "./drops.js";

const root = document.documentElement;
const themeBtn = $("themeBtn");
function setTheme(t, persist) {
  root.dataset.theme = t;
  themeBtn.setAttribute(
    "aria-label",
    t === "dark" ? "Switch to light theme" : "Switch to dark theme",
  );
  if (persist)
    try {
      localStorage.setItem("beam:theme", t);
    } catch {}
}
setTheme(root.dataset.theme, false);
themeBtn.addEventListener("click", () =>
  setTheme(root.dataset.theme === "dark" ? "light" : "dark", true),
);
matchMedia("(prefers-color-scheme: light)").addEventListener("change", (e) => {
  try {
    if (localStorage.getItem("beam:theme")) return;
  } catch {}
  setTheme(e.matches ? "light" : "dark", false);
});

if (coarsePointer.matches)
  $("selfHint").textContent = "Tap a device to send it files";

let hashCode = null;
let joining = null;
let claim = parseLink(location.hash);
const m = location.hash
  .slice(1)
  .toUpperCase()
  .match(/^[2-9A-HJ-NP-Z]{4}$/);
if (m) hashCode = m[0];
if (m || claim) history.replaceState(null, "", location.pathname);
on("joining", (c) => {
  joining = c;
});

const ERRORS = {
  "bad-code": "That code isn't active. Check it and try again.",
  "rate-limited": "Too many tries. Wait a moment and try again.",
  "unknown-peer": "That device isn't here anymore.",
  "bad-transfer": "That transfer is no longer valid.",
  "too-many-connections":
    "Too many devices from your network are connected. Try again later.",
  replaced:
    "This device connected from another tab, so this one went idle. Reload to take over.",
};

const room = {
  "room-state"(d) {
    state.self = d.self;
    state.drops = d.drops && d.drops.maxBytes > 0 ? d.drops : null;
    if (!identity().name || !identity().emoji)
      saveIdentity({ name: d.self.name, emoji: d.self.emoji });
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
    if (claim) {
      remember(claim.id, claim.secret);
      socket.send("drop-claim", { id: claim.id });
      claim = null;
    }
  },
  "peer-joined"(p) {
    if (state.peers.has(p.id)) return;
    state.peers.set(p.id, p);
    emit("peers");
    toast(`${p.name} ${p.emoji} is here`);
    if (state.code) socket.send("room-create", {});
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
    state.codeExpires = Date.now() + d.expiresIn * 1000;
    emit("code");
  },
  snippet(d) {
    emit("snippet", d);
  },
  rtc: onSignal,
  error(d) {
    if (d.code === "bad-code" || d.code === "rate-limited") {
      joining = null;
      emit("join-failed");
    }
    toast(ERRORS[d.code] || d.message || "Something went wrong", "bad");
    if (d.code === "too-many-connections" || d.code === "replaced")
      socket.close();
  },
};

const conn = $("connState");
let warnedDown = false;
function setConn(onLine) {
  state.connected = onLine;
  conn.dataset.state = onLine ? "on" : "off";
  conn.setAttribute("aria-label", onLine ? "Connected" : "Reconnecting");
  if (!onLine) $("selfName").textContent = "…";
}

const socket = createSocket({
  params: () => (device ? { ...identity(), pub: device.pub } : identity()),
  onOpen() {
    setConn(true);
  },
  onClose(intentional) {
    const wasUp = state.connected;
    if (!wasUp && !intentional && !warnedDown) {
      warnedDown = true;
      toast(`Can't reach the server at ${location.host}, retrying…`, "bad");
    }
    setConn(false);
    state.self = null;
    state.code = null;
    state.peers.clear();
    joining = null;
    emit("peers");
    emit("code");
    resetRTC();
    const paused = transfers.pauseAll();
    if (wasUp && !intentional)
      toast(
        paused
          ? "Connection lost, resuming…"
          : "Connection lost, reconnecting…",
        "bad",
      );
  },
  onMessage(type, data) {
    const h = room[type] || transfers.handlers[type];
    if (h) h(data, transfers.socketLink);
  },
  onBinary: (buf) => transfers.onChunk(buf, transfers.socketLink),
});
transfers.bindSocket(socket);

on("self", () => {
  $("selfName").textContent = state.self.name;
  $("selfEmoji").textContent = state.self.emoji;
});

await initDrops();
initToasts();
initNotify();
initHistory();
initPanel();
initRTC(socket);
initShare(socket);
const { openNote } = initDialogs(socket);
initRadar({ onNote: openNote });
if ("serviceWorker" in navigator)
  navigator.serviceWorker.register("sw.js").catch(() => {});

const GATE = {
  held: "This browser already has beam open in another tab. Use that tab, or take over here.",
  yielded: "Another tab took over. Take it back whenever you like.",
  asking: "Asking the other tab to hand over…",
  busy: "The other tab is in the middle of a transfer. Try again when it finishes.",
};
const gateEl = $("tabGate"),
  gateBody = $("gateBody"),
  gateBtn = $("gateUse");
function gate(status) {
  const show = !!status;
  gateEl.hidden = !show;
  document.querySelector(".app").inert = show;
  $("panel").inert = show;
  if (show) gateBody.textContent = GATE[status] || GATE.held;
  gateBtn.disabled = status === "asking";
}
gateBtn.addEventListener("click", takeover);

initTabs({
  onLead() {
    gate(null);
    transfers.restore().finally(() => socket.connect());
  },
  onWait(status) {
    gate(status);
  },
  onYield() {
    for (const d of document.querySelectorAll("dialog[open]")) d.close();
    return socket.close().then(transfers.release);
  },
  isBusy() {
    if (state.offers.length) return true;
    for (const t of state.transfers.values())
      if (!isDone(t) && t.state !== "paused") return true;
    return false;
  },
});

window.__beam = {
  state,
  emit,
  transfers,
  tabs: { takeover, isLeader, debug: tabsDebug },
};
