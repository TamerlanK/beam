import { state, emit, toast } from "./state.js";
import { $ } from "./util.js";
import { queueFiles } from "./transfers.js";

const CACHE = "beam-share";
let socket;

export async function initShare(s) {
  socket = s;
  $("shareCancel").addEventListener("click", () => setShared(null));
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
  const what = v.files.length ? (v.files.length === 1 ? v.files[0].name : `${v.files.length} files`) : "a note";
  $("shareWhat").textContent = what;
  emit("layout");
}

export function sendShared(peerId) {
  const v = state.shared;
  if (!v) return false;
  if (v.files.length) queueFiles(peerId, v.files);
  else if (v.text) { socket.send("snippet", { to: peerId, text: v.text.slice(0, 8000) }); toast("Note sent", "ok"); }
  setShared(null);
  return true;
}
