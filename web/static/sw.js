const SHELL = "beam-shell";
const SHARE = "beam-share";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (url.origin !== location.origin) return;
  if (e.request.method === "POST" && url.pathname.endsWith("/share")) {
    e.respondWith(stash(e.request));
    return;
  }
  if (e.request.method !== "GET" || url.pathname.includes("/ws")) return;
  e.respondWith(
    fetch(e.request)
      .then((res) => {
        if (res.ok)
          caches
            .open(SHELL)
            .then((c) => c.put(e.request, res.clone()))
            .catch(() => {});
        return res;
      })
      .catch(() =>
        caches.match(e.request).then((hit) => hit || Response.error()),
      ),
  );
});

async function stash(req) {
  const form = await req.formData();
  const cache = await caches.open(SHARE);
  let i = 0;
  for (const f of form.getAll("files")) {
    if (!f || !f.size) continue;
    await cache.put(
      `${self.registration.scope}shared/${i++}`,
      new Response(f, {
        headers: {
          "content-type": f.type || "application/octet-stream",
          "x-name": encodeURIComponent(f.name || "file"),
        },
      }),
    );
  }
  const text = [form.get("text"), form.get("url")]
    .filter((v) => typeof v === "string" && v.trim())
    .join("\n")
    .trim();
  if (text)
    await cache.put(
      `${self.registration.scope}shared/text`,
      new Response(text),
    );
  return Response.redirect(`${self.registration.scope}?share`, 303);
}
