import { state } from "./state.js";
import { available as e2e, deviceKeypair } from "./crypto.js";

export let device = null;
const secrets = new Map();

export async function initDrops() {
  if (!e2e) return;
  try {
    device = await deviceKeypair();
  } catch {
    device = null;
  }
}

export const enabled = () => !!(state.drops && e2e);
export const fits = (size) =>
  enabled() && size > 0 && size <= state.drops.maxBytes;

export const linkFor = (id, secret) =>
  `${location.origin}${location.pathname}#d=${id}.${secret}`;

export function parseLink(hash) {
  const m = (hash || "").match(
    /^#d=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.([A-Za-z0-9_-]{43})$/i,
  );
  return m ? { id: m[1].toLowerCase(), secret: m[2] } : null;
}

export const remember = (id, secret) => secrets.set(id, secret);
export const secretFor = (id) => secrets.get(id);
export const forgetSecret = (id) => secrets.delete(id);

export const ttlText = (sec) =>
  sec >= 90
    ? `${Math.round(sec / 60)} min`
    : `${Math.max(0, Math.round(sec))} s`;
