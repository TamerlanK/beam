function kv(mode, op) {
  return new Promise((resolve, reject) => {
    const open = indexedDB.open("beam", 1);
    open.onupgradeneeded = () => open.result.createObjectStore("kv");
    open.onerror = () => reject(open.error);
    open.onsuccess = () => {
      const db = open.result,
        tx = db.transaction("kv", mode),
        req = op(tx.objectStore("kv"));
      tx.oncomplete = () => {
        db.close();
        resolve(req.result);
      };
      tx.onerror = () => {
        db.close();
        reject(tx.error);
      };
    };
  });
}

export const get = (key) => kv("readonly", (s) => s.get(key));
export const put = (key, value) => kv("readwrite", (s) => s.put(value, key));
export const del = (key) => kv("readwrite", (s) => s.delete(key));
export const list = (prefix) =>
  kv("readonly", (s) => s.getAll(IDBKeyRange.bound(prefix, prefix + "￿")));
