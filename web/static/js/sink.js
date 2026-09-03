// dir: undefined = ask per file, null = keep in memory, handle = write into that folder
export async function openSink(name, mime, dir) {
  if (dir === null || typeof showSaveFilePicker !== "function") return memorySink(name, mime);
  try {
    // ponytail: same-named files in a batch overwrite each other in the folder
    const handle = dir ? await dir.getFileHandle(name, { create: true }) : await showSaveFilePicker({ suggestedName: name });
    const w = await handle.createWritable();
    let chain = Promise.resolve();
    return {
      write(buf) { chain = chain.then(() => w.write(buf)); return chain; },
      async close() { await chain; await w.close(); return { saved: true }; },
      abort() { chain.then(() => w.abort(), () => w.abort()).catch(() => {}); },
    };
  } catch {
    return memorySink(name, mime);
  }
}

function memorySink(name, mime) {
  const parts = [];
  return {
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
