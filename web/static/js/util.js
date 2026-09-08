import { ICONS } from "./icons.js";

export const $ = (id) => document.getElementById(id);

export function el(tag, attrs = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === "class") n.className = v;
    else if (k === "text") n.textContent = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) if (c != null) n.append(c);
  return n;
}

export function uuid() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  return bytesToUUID(b);
}

export function cleanPath(p) {
  if (typeof p !== "string" || !p || p.length > 1024) return "";
  const segs = p.split(/[\\/]/);
  for (const s of segs)
    if (
      !s ||
      s === "." ||
      s === ".." ||
      s.length > 255 ||
      /[\x00-\x1f\x7f]/.test(s)
    )
      return "";
  return segs.join("/");
}

export function folderOf(list) {
  let top = "";
  for (const x of list) {
    const first = (x.path || "").split("/")[0];
    if (!first || (top && first !== top)) return "";
    top = first;
  }
  return top;
}

export const labelOf = (t) => (t.path ? `${t.path}/${t.name}` : t.name);

export function thumb(t) {
  return t.preview ? el("img", { src: t.preview, alt: "" }) : icon(t.kind);
}

export function icon(name, cls = "i") {
  const s = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  s.setAttribute("viewBox", "0 0 24 24");
  s.setAttribute("class", cls);
  s.setAttribute("aria-hidden", "true");
  const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
  p.setAttribute("d", ICONS[name] || ICONS.file);
  s.append(p);
  return s;
}

export function fmtSize(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1048576) return `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} KB`;
  if (n < 1073741824) return `${(n / 1048576).toFixed(1)} MB`;
  return `${(n / 1073741824).toFixed(2)} GB`;
}

export const fmtRate = (bps) => (bps > 0 ? `${fmtSize(bps)}/s` : "");

export function fmtLeft(sec) {
  if (!isFinite(sec) || sec < 0) return "";
  if (sec < 60) return `${Math.ceil(sec)}s left`;
  if (sec < 3600)
    return `${Math.floor(sec / 60)}m ${Math.ceil(sec % 60)}s left`;
  return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m left`;
}

export function fmtAgo(ts) {
  const s = Math.max(0, (Date.now() - ts) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  if (s < 172800) return "yesterday";
  return new Date(ts).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
  });
}

export function uuidBytes(str) {
  const hex = str.replace(/-/g, "");
  const b = new Uint8Array(16);
  for (let i = 0; i < 16; i++) b[i] = parseInt(hex.substr(i * 2, 2), 16);
  return b;
}

export function bytesToUUID(b) {
  const h = [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

export function asURL(text) {
  const t = text.trim();
  if (!/^https?:\/\/\S+$/i.test(t)) return null;
  try {
    return new URL(t).href;
  } catch {
    return null;
  }
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = el("textarea", { style: "position:fixed;opacity:0" });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
  }
}

export const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)");
export const coarsePointer = matchMedia("(hover: none)");
