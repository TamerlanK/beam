export async function openSink(name, mime) {
  if (typeof showSaveFilePicker !== "function") return memorySink(name, mime);
  try {
    const handle = await showSaveFilePicker({ suggestedName: name });
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
