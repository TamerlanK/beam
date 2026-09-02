import { state, on } from "./state.js";
import { $ } from "./util.js";

const baseTitle = document.title;
const ICON = document.querySelector("link[rel=icon]").href;
const shown = new Map();
let audio = null;

export function initNotify() {
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) return;
    document.title = baseTitle;
    for (const n of shown.values()) n.close();
    shown.clear();
  });
  addEventListener("pointerdown", () => { audio ||= new AudioContext(); }, { once: true });
  $("offerAccept").addEventListener("click", askPermission);

  on("offers", () => {
    const pending = new Set(state.offers.map((o) => o.id));
    for (const [id, n] of shown) if (!pending.has(id)) { n.close(); shown.delete(id); }
    const d = state.offers[state.offers.length - 1];
    if (!d || shown.has(d.id) || d.announced) return;
    d.announced = true;
    chime();
    if (!document.hidden) return;
    document.title = "Incoming file · beam";
    show(d.id, `${d.from ? d.from.name : "Someone"} wants to send you a file`, d.name);
  });
  on("snippet", (d) => {
    chime();
    if (!document.hidden) return;
    document.title = "New note · beam";
    show(`note-${Date.now()}`, `Note from ${d.from ? d.from.name : "someone"}`, d.text.slice(0, 120));
  });
}

function askPermission() {
  if ("Notification" in window && Notification.permission === "default") Notification.requestPermission();
}

function show(id, title, body) {
  if (!("Notification" in window) || Notification.permission !== "granted") return;
  const n = new Notification(title, { body, tag: id, icon: ICON });
  n.onclick = () => { window.focus(); n.close(); };
  shown.set(id, n);
}

function chime() {
  if (!audio || audio.state !== "running") return;
  const t = audio.currentTime;
  for (const [hz, at] of [[880, 0], [1320, 0.11]]) {
    const o = audio.createOscillator(), g = audio.createGain();
    o.frequency.value = hz;
    g.gain.setValueAtTime(0.0001, t + at);
    g.gain.exponentialRampToValueAtTime(0.12, t + at + 0.02);
    g.gain.exponentialRampToValueAtTime(0.0001, t + at + 0.3);
    o.connect(g).connect(audio.destination);
    o.start(t + at);
    o.stop(t + at + 0.32);
  }
}
