import { state, on, emit, isDone, toast } from "./state.js";
import { $, el, icon, thumb, fmtSize, fmtAgo } from "./util.js";
import { queueFiles } from "./transfers.js";
import { saveBlob } from "./sink.js";

const KEY = "beam:history";
const MAX = 50;
const KEEP_BLOB_MS = 2 * 60 * 1000;
const live = new Map();
let entries = load();

export function initHistory() {
  const modal = $("historyModal"), list = $("historyList"), empty = $("historyEmpty"), clear = $("historyClear");

  on("transfer:state", (t) => {
    if (!isDone(t) || t.state === "missed" || entries.some((e) => e.id === t.id)) return;
    const peer = state.peers.get(t.peerId);
    entries.unshift({
      id: t.id, dir: t.dir, name: t.name, size: t.size, kind: t.kind, state: t.state, note: t.note, preview: t.preview || undefined, // ponytail: ≤40KB each, 50 max; drop from history if localStorage quota bites
      peerId: t.peerId, peerName: peer ? peer.name : t.peerName || "that device", peerEmoji: peer ? peer.emoji : "", at: Date.now(), drop: !!t.drop,
    });
    entries.length = Math.min(entries.length, MAX);
    save();
    if (t.state === "done" && t.dir === "send") live.set(t.id, { file: t.file });
    if (t.state === "done" && t.dir === "recv" && t.blobUrl) {
      live.set(t.id, { blobUrl: t.blobUrl });
      setTimeout(() => { URL.revokeObjectURL(t.blobUrl); live.delete(t.id); emit("history"); }, KEEP_BLOB_MS);
    }
    emit("history");
  });

  function render() {
    empty.hidden = entries.length > 0;
    clear.hidden = entries.length === 0;
    list.replaceChildren(...entries.map(row));
  }

  function row(e) {
    const keep = live.get(e.id);
    const who = `${e.dir === "send" ? "To" : "From"} ${e.peerName}${e.peerEmoji ? " " + e.peerEmoji : ""}`;
    const outcome = e.state === "done" ? (e.dir === "send" ? (e.drop ? "Left" : "Sent") : "Received") : e.note ? e.note[0].toUpperCase() + e.note.slice(1) : "Failed";
    const act = el("span", { class: "tr-act" });
    if (keep && keep.file) act.append(el("button", { class: "btn btn-ghost", type: "button", onclick: () => again(e, keep.file) }, "Send again"));
    if (keep && keep.blobUrl) act.append(el("button", { class: "btn btn-ghost", type: "button", onclick: () => saveBlob({ blobUrl: keep.blobUrl, name: e.name }) }, icon("save"), "Save"));
    return el("li", { class: `tr is-${e.state}` },
      el("span", { class: "tr-icon" }, thumb(e)),
      el("span", { class: "tr-name", title: e.name, text: e.name }),
      el("span", { class: "tr-meta", text: `${outcome} · ${who} · ${fmtSize(e.size)} · ${fmtAgo(e.at)}` }),
      act);
  }

  function again(e, file) {
    if (!state.peers.has(e.peerId)) { toast(`${e.peerName} isn't here anymore`, "bad"); return; }
    queueFiles(e.peerId, [file]);
    modal.close();
  }

  on("history", () => { if (modal.open) render(); });
  $("historyBtn").addEventListener("click", () => { render(); modal.showModal(); });
  $("historyClose").addEventListener("click", () => modal.close());
  clear.addEventListener("click", () => {
    for (const k of live.values()) if (k.blobUrl) URL.revokeObjectURL(k.blobUrl);
    live.clear();
    entries = [];
    save();
    render();
  });
}

function load() {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) || "[]");
    return Array.isArray(v) ? v.filter((e) => e && typeof e.name === "string") : [];
  } catch { return []; }
}

function save() {
  try { localStorage.setItem(KEY, JSON.stringify(entries)); } catch {}
}
