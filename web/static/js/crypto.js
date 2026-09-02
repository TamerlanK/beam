const ECDH = { name: "ECDH", namedCurve: "P-256" };
const ALPHA = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ";

export const available = !!(globalThis.crypto && crypto.subtle);

const b64 = (buf) => btoa(String.fromCharCode(...new Uint8Array(buf)));
const unb64 = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));

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

export const seal = (key, n, aad, data) => crypto.subtle.encrypt({ name: "AES-GCM", iv: iv(n), additionalData: aad }, key, data);
export const open = (key, n, aad, data) => crypto.subtle.decrypt({ name: "AES-GCM", iv: iv(n), additionalData: aad }, key, data);
