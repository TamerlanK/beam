const ECDH = { name: "ECDH", namedCurve: "P-256" };
const ALPHA = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ";

export const available = !!(globalThis.crypto && crypto.subtle);

const b64 = (buf) => btoa(String.fromCharCode(...new Uint8Array(buf)));
const unb64 = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
const b64url = (buf) => b64(buf).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const unb64url = (s) => unb64(s.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - (s.length % 4)) % 4));

export async function keypair() {
  const k = await crypto.subtle.generateKey(ECDH, false, ["deriveKey"]);
  return { priv: k.privateKey, pub: b64(await crypto.subtle.exportKey("raw", k.publicKey)) };
}

export async function shared(priv, myPub, theirPub) {
  const pub = await crypto.subtle.importKey("raw", unb64(theirPub), ECDH, false, []);
  const key = await crypto.subtle.deriveKey({ name: "ECDH", public: pub }, priv, { name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"]);
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode([myPub, theirPub].sort().join("|"))));
  return { key, sas: [...digest.subarray(0, 4)].map((b) => ALPHA[b % 32]).join("") };
}

function iv(n) {
  const b = new Uint8Array(12);
  new DataView(b.buffer).setUint32(8, n);
  return b;
}

function kv(mode, op) {
  return new Promise((resolve, reject) => {
    const open = indexedDB.open("beam", 1);
    open.onupgradeneeded = () => open.result.createObjectStore("kv");
    open.onerror = () => reject(open.error);
    open.onsuccess = () => {
      const db = open.result, tx = db.transaction("kv", mode), req = op(tx.objectStore("kv"));
      tx.oncomplete = () => { db.close(); resolve(req.result); };
      tx.onerror = () => { db.close(); reject(tx.error); };
    };
  });
}

// The browser's long-lived ECDH keypair, kept in IndexedDB as a
// non-extractable CryptoKey so the private half never exists as bytes.
// ponytail: if IndexedDB is unavailable (private mode) the key lives one session
export async function deviceKeypair() {
  try {
    const k = await kv("readonly", (s) => s.get("ecdh"));
    if (k && k.priv && typeof k.pub === "string") return k;
  } catch {}
  const kp = await keypair();
  try { await kv("readwrite", (s) => s.put(kp, "ecdh")); } catch {}
  return kp;
}

export async function randomKey() {
  const key = await crypto.subtle.generateKey({ name: "AES-GCM", length: 256 }, true, ["encrypt", "decrypt"]);
  return { key, secret: b64url(await crypto.subtle.exportKey("raw", key)) };
}

export const importSecret = (secret) => crypto.subtle.importKey("raw", unb64url(secret), { name: "AES-GCM" }, false, ["encrypt", "decrypt"]);

export const seal = (key, n, aad, data) => crypto.subtle.encrypt({ name: "AES-GCM", iv: iv(n), additionalData: aad }, key, data);
export const open = (key, n, aad, data) => crypto.subtle.decrypt({ name: "AES-GCM", iv: iv(n), additionalData: aad }, key, data);
