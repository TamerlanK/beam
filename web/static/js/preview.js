const MAX_SIDE = 320;
const MAX_BYTES = 40 * 1024;
const IMAGE_LIMIT = 40 << 20;
const PDF_LIMIT = 30 << 20;
const TIMEOUT = 3000;

export const PREVIEW_RE =
  /^data:image\/(?:jpeg|png|webp);base64,[A-Za-z0-9+/]+=*$/;

export function validPreview(p) {
  return typeof p === "string" && p.length <= MAX_BYTES && PREVIEW_RE.test(p);
}

export async function previewOf(file) {
  const pdf = file.type === "application/pdf" || /\.pdf$/i.test(file.name);
  const image = !pdf && file.type.startsWith("image/");
  if (!(pdf || image) || file.size > (image ? IMAGE_LIMIT : PDF_LIMIT))
    return "";
  try {
    const url = await Promise.race([
      image ? fromImage(file) : fromPdf(file),
      new Promise((r) => setTimeout(() => r(""), TIMEOUT)),
    ]);
    return validPreview(url) ? url : "";
  } catch {
    return "";
  }
}

function fit(w, h) {
  const s = Math.min(1, MAX_SIDE / Math.max(w, h, 1));
  return [Math.max(1, Math.round(w * s)), Math.max(1, Math.round(h * s))];
}

function encode(source, w, h) {
  const [cw, ch] = fit(w, h);
  const c = document.createElement("canvas");
  c.width = cw;
  c.height = ch;
  const ctx = c.getContext("2d");
  ctx.fillStyle = "#fff";
  ctx.fillRect(0, 0, cw, ch);
  ctx.drawImage(source, 0, 0, cw, ch);
  for (const q of [0.8, 0.6, 0.4, 0.25]) {
    const u = c.toDataURL("image/jpeg", q);
    if (u.length <= MAX_BYTES) return u;
  }
  return "";
}

async function fromImage(file) {
  let bmp;
  try {
    bmp = await createImageBitmap(file, { imageOrientation: "from-image" });
  } catch {
    bmp = await createImageBitmap(file);
  }
  try {
    return encode(bmp, bmp.width, bmp.height);
  } finally {
    bmp.close();
  }
}

async function fromPdf(file) {
  const pdfjs = await import("../vendor/pdf.min.mjs");
  pdfjs.GlobalWorkerOptions.workerSrc = new URL(
    "../vendor/pdf.worker.min.mjs",
    import.meta.url,
  ).href;
  const doc = await pdfjs.getDocument({
    data: await file.arrayBuffer(),
    isEvalSupported: false,
  }).promise;
  try {
    const page = await doc.getPage(1);
    const base = page.getViewport({ scale: 1 });
    const [w] = fit(base.width, base.height);
    const vp = page.getViewport({ scale: w / base.width });
    const c = document.createElement("canvas");
    c.width = Math.ceil(vp.width);
    c.height = Math.ceil(vp.height);
    await page.render({ canvasContext: c.getContext("2d"), viewport: vp })
      .promise;
    return encode(c, c.width, c.height);
  } finally {
    doc.destroy();
  }
}
