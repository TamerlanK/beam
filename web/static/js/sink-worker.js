// Writes received chunks straight into a part file in the origin-private
// file system. Sync access handles only exist in workers; every write lands
// on disk, so a transfer survives a closed tab. `flushed` is the byte count
// the main thread may safely record as resumable.
let dir = null, handle = null, id = "", writes = 0, flushed = 0;

onmessage = async ({ data }) => {
  const { k, op } = data;
  try {
    if (op === "open") {
      id = data.id;
      dir = await (await navigator.storage.getDirectory()).getDirectoryHandle("parts", { create: true });
      handle = await (await dir.getFileHandle(id, { create: true })).createSyncAccessHandle();
      postMessage({ k, size: handle.getSize() });
    } else if (op === "write") {
      const buf = new Uint8Array(data.buf);
      if (data.at < flushed) flushed = data.at;
      let off = 0;
      while (off < buf.length) off += handle.write(buf.subarray(off), { at: data.at + off });
      if (++writes % 64 === 0) { handle.flush(); flushed = data.at + buf.length; }
      postMessage({ k, flushed });
    } else if (op === "close") {
      handle.flush();
      handle.close();
      postMessage({ k });
    } else if (op === "drop") {
      try { handle.close(); } catch {}
      await dir.removeEntry(id);
      postMessage({ k });
    }
  } catch (e) {
    postMessage({ k, error: String((e && e.message) || e) });
  }
};
