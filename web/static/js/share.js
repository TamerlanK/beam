import { state, emit, on, toast } from "./state.js";
import { $ } from "./util.js";
import { queueFiles, leaveLink } from "./transfers.js";
import { fits } from "./drops.js";

const CACHE = "beam-share";
let socket;

export async function initShare(s) {
  socket = s;
  $("shareCancel").addEventListener("click", () => setShared(null));
  $("shareLink").addEventListener("click", () => {
    const v = state.shared;
    if (!v || v.files.length !== 1) return;
    leaveLink(v.files[0]);
    setShared(null);
  });
  on("peers", () => { if (state.shared) setShared(state.shared); });
  if (!("caches" in window) || !new URLSearchParams(location.search).has("share")) return;
  history.replaceState(null, "", location.pathname);
  const cache = await caches.open(CACHE);
  const files = [];
  let text = "";
  for (const req of await cache.keys()) {
    const res = await cache.match(req);
    if (new URL(req.url).pathname.endsWith("/text")) text = await res.text();
    else files.push(new File([await res.blob()], decodeURIComponent(res.headers.get("x-name") || "file"), { type: res.headers.get("content-type") || "" }));
    await cache.delete(req);
  }
  if (files.length || text) setShared({ files, text });
}

function setShared(v) {
  state.shared = v;
  const bar = $("shareBar");
  bar.hidden = !v;
  if (!v) return;
  const n = v.files.length;
  $("shareWhat").textContent = n === 0 ? "a note" : n === 1 ? v.files[0].name : `${v.files[0].name} and ${n - 1} more`;
  $("shareVerb").textContent = state.peers.size ? "Tap a device to send" : "Waiting for a device to send";
  $("shareLink").hidden = !(n === 1 && fits(v.files[0].size));
  emit("layout");
}

export function stageFiles(files) {
  const add = [...files].filter((f) => f.size > 0);
  if (!add.length) return;
  const cur = state.shared && state.shared.files ? state.shared.files : [];
  setShared({ files: [...cur, ...add], text: "" });
}

export function sendShared(peerId) {
  const v = state.shared;
  if (!v) return false;
  if (v.files.length) queueFiles(peerId, v.files);
  else if (v.text) { socket.send("snippet", { to: peerId, text: v.text.slice(0, 8000) }); toast("Note sent", "ok"); }
  setShared(null);
  return true;
}
