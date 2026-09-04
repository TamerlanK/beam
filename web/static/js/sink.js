// Where received bytes go. Preference order:
//   1. a part file in the origin-private file system (durable: a closed tab
//      resumes; no RAM ceiling on any browser), delivered at the end into the
//      file the user picked, or as a download
//   2. straight into the picked file (File System Access, Chromium) when OPFS
//      is unavailable or out of quota
//   3. memory, assembled into a Blob
const PARTS = "parts";
const SLACK = 64 << 20;
const KEEP_PART_MS = 10 * 60 * 1000;
const STALE_PART_MS = 60 * 60 * 1000;

export const opfs = !!(navigator.storage && navigator.storage.getDirectory && typeof Worker === "function");

const partsDir = async () => (await navigator.storage.getDirectory()).getDirectoryHandle(PARTS, { create: true });

async function roomFor(size) {
  try {
    const { quota, usage } = await navigator.storage.estimate();
    return quota - usage >= size + SLACK;
  } catch { return false; }
}

// dir: undefined = ask per file, null = keep in memory, handle = write into that folder
export async function openSink(id, name, size, mime, dir) {
  let target = null;
  if (dir !== null && typeof showSaveFilePicker === "function") {
    // ponytail: same-named files in a batch overwrite each other in the folder
    try { target = dir ? await dir.getFileHandle(name, { create: true }) : await showSaveFilePicker({ suggestedName: name }); } catch { target = null; }
  }
  if (opfs && (await roomFor(size))) {
    try { return await partSink(id, name, mime, target); } catch {}
  }
  if (target) {
    try { return await fsaSink(target); } catch {}
  }
  return memorySink(name, mime);
}

// Reattach to a part file after a reload; throws when that is impossible.
export const reopenSink = (id, name, mime, target) => partSink(id, name, mime, target);

async function partSink(id, name, mime, target) {
  const w = new Worker("js/sink-worker.js");
  const pending = new Map();
  let seq = 0;
  const call = (msg, transfer) => new Promise((resolve, reject) => {
    const k = ++seq;
    pending.set(k, { resolve, reject });
    w.postMessage({ k, ...msg }, transfer || []);
  });
  w.onmessage = ({ data }) => {
    const p = pending.get(data.k);
    if (!p) return;
    pending.delete(data.k);
    if (data.error) p.reject(new Error(data.error)); else p.resolve(data);
  };
  w.onerror = (e) => { for (const p of pending.values()) p.reject(new Error(e.message || "worker failed")); pending.clear(); };
  const stop = () => w.terminate();
  await call({ op: "open", id });
  let cursor = 0;
  const sink = {
    durable: true, target, flushed: 0,
    seek(off) { cursor = off; },
    async write(buf) {
      const at = cursor;
      cursor += buf.byteLength;
      sink.flushed = (await call({ op: "write", at, buf }, [buf])).flushed;
    },
    async close() { await call({ op: "close" }); stop(); return deliver(id, name, mime, target); },
    abort() { call({ op: "drop" }).catch(() => {}).finally(stop); },
    // Release the file lock but keep the part, for another tab or a later session.
    detach() { call({ op: "close" }).catch(() => {}).finally(stop); },
  };
  return sink;
}

async function deliver(id, name, mime, target) {
  const dir = await partsDir();
  const file = await (await dir.getFileHandle(id)).getFile();
  const remove = () => dir.removeEntry(id).catch(() => {});
  const copy = async () => {
    await file.stream().pipeTo(await target.createWritable());
    await remove();
    return { saved: true };
  };
  const download = () => {
    const blobUrl = URL.createObjectURL(new File([file], name, { type: mime || "application/octet-stream" }));
    saveBlob({ blobUrl, name });
    setTimeout(remove, KEEP_PART_MS);
    return { blobUrl };
  };
  if (!target) return download();
  try { if ((await target.queryPermission({ mode: "readwrite" })) === "granted") return await copy(); } catch {}
  // The permission lapsed (a reload): finishing into the chosen file needs a click.
  return {
    saved: false,
    async save() {
      try { if ((await target.requestPermission({ mode: "readwrite" })) === "granted") return await copy(); } catch {}
      return download();
    },
  };
}

// Delete part files no live record refers to, once they are clearly abandoned.
export async function reapParts(keep) {
  if (!opfs) return;
  const dir = await partsDir();
  for await (const [entry, h] of dir.entries()) {
    if (keep.has(entry)) continue;
    try {
      if (Date.now() - (await h.getFile()).lastModified > STALE_PART_MS) await dir.removeEntry(entry);
    } catch {}
  }
}

async function fsaSink(handle) {
  const w = await handle.createWritable();
  let chain = Promise.resolve();
  return {
    durable: false, target: handle,
    seek(off) { chain = chain.then(() => w.seek(off)); },
    write(buf) { chain = chain.then(() => w.write(buf)); return chain; },
    async close() { await chain; await w.close(); return { saved: true }; },
    abort() { chain.then(() => w.abort(), () => w.abort()).catch(() => {}); },
  };
}

function memorySink(name, mime) {
  const parts = [];
  return {
    durable: false, target: null,
    // Every write is one 64KB chunk, so a chunk-aligned offset is a part index.
    seek(off) { parts.length = Math.min(parts.length, off / 65536); },
    write(buf) { parts.push(buf); },
    close() {
      const blobUrl = URL.createObjectURL(new Blob(parts, { type: mime || "application/octet-stream" }));
      parts.length = 0;
      saveBlob({ blobUrl, name });
      return { blobUrl };
    },
    abort() { parts.length = 0; },
  };
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
