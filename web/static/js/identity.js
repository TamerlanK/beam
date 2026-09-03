import { uuid } from "./util.js";

const KEY = "beam:identity";

export const EMOJIS = [
  "🦊", "🐼", "🦉", "🐙", "🦄", "🐢", "🐝", "🦋", "🐬", "🦩", "🐨", "🦥",
  "🐸", "🦜", "🐺", "🦔", "🦁", "🐯", "🐻", "🐰", "🦊", "🐧", "🦈", "🐳",
  "🦅", "🦆", "🦢", "🦡", "🦫", "🦌", "🐉", "🦎", "🦙", "🦦", "🦝", "🦭",
  "🌙", "⚡", "🔥", "🌊", "🌵", "🍄", "🎧", "🚀", "🛸", "🎲", "🧭", "🔮",
];

let cached = null;

export function identity() {
  if (cached) return cached;
  let stored = {};
  try { stored = JSON.parse(localStorage.getItem(KEY) || "{}"); } catch {}
  cached = {
    id: typeof stored.id === "string" && stored.id ? stored.id : uuid(),
    name: typeof stored.name === "string" ? stored.name : "",
    emoji: typeof stored.emoji === "string" ? stored.emoji : "",
  };
  if (!stored.id) persist();
  return cached;
}

export function saveIdentity(patch) {
  Object.assign(identity(), patch);
  persist();
}

function persist() {
  try { localStorage.setItem(KEY, JSON.stringify(cached)); } catch {}
}
