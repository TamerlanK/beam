export const ICONS = {
  x: "M6 6l12 12M18 6L6 18",
  check: "M5 12.5l4.5 4.5L19 7",
  info: "M12 8h.01M12 11v5",
  copy: "M9 9h10v10H9zM5 15V5h10",
  save: "M12 4v11M7 10l5 5 5-5M4 19h16",
  up: "M12 19V5M6 11l6-6 6 6",
  down: "M12 5v14M6 13l6 6 6-6",
  phone: "M7 2.5h10a2.5 2.5 0 0 1 2.5 2.5v14a2.5 2.5 0 0 1-2.5 2.5H7A2.5 2.5 0 0 1 4.5 19V5A2.5 2.5 0 0 1 7 2.5zM10.5 18.5h3",
  tablet: "M7 3h10a2.5 2.5 0 0 1 2.5 2.5v13A2.5 2.5 0 0 1 17 21H7a2.5 2.5 0 0 1-2.5-2.5v-13A2.5 2.5 0 0 1 7 3zM10 18h4",
  laptop: "M5.5 5h13A1.5 1.5 0 0 1 20 6.5v9.5H4V6.5A1.5 1.5 0 0 1 5.5 5zM2.5 19h19",
  file: "M6 3h8l4 4v14H6zM14 3v4h4",
  image: "M5 5h14a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2zM21 16l-5-5-8 8M8.5 10.5h.01",
  video: "M5 5h14a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2zM10 9l5 3-5 3z",
  audio: "M9 18V6l10-2v12M9 18a2.5 2.5 0 1 1-5 0 2.5 2.5 0 0 1 5 0zM19 16a2.5 2.5 0 1 1-5 0 2.5 2.5 0 0 1 5 0z",
  archive: "M4 4h16v5H4zM5 9v11h14V9M10 13h4",
  doc: "M6 3h8l4 4v14H6zM14 3v4h4M9 12h6M9 16h6",
  sheet: "M5 4h14a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2zM3 10h18M3 15h18M9 4v16",
  code: "M8 8l-4 4 4 4M16 8l4 4-4 4M13.5 5l-3 14",
  clock: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7.5V12l3 2",
};

const KINDS = {
  image: "png jpg jpeg gif webp avif heic heif svg bmp tiff tif psd raw",
  video: "mp4 mov mkv webm avi m4v wmv flv",
  audio: "mp3 wav flac aac m4a ogg opus aiff",
  archive: "zip rar 7z tar gz bz2 xz tgz dmg iso pkg apk",
  doc: "pdf doc docx odt rtf pages key ppt pptx odp epub",
  sheet: "xls xlsx csv numbers ods tsv",
  code: "js mjs ts jsx tsx py go rs java c cpp cc h hpp cs rb php swift kt html css scss json yaml yml toml sh bash sql",
};
const EXT = new Map();
for (const [kind, list] of Object.entries(KINDS)) for (const e of list.split(" ")) EXT.set(e, kind);

export function fileKind(name, mime = "") {
  const m = mime.split("/")[0];
  if (m === "image" || m === "video" || m === "audio") return m;
  const ext = (name.split(".").pop() || "").toLowerCase();
  return EXT.get(ext) || (m === "text" ? "doc" : "file");
}
