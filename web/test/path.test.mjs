import { test } from "node:test";
import assert from "node:assert/strict";

globalThis.matchMedia = () => ({});
const { cleanPath, folderOf, labelOf } = await import("../static/js/util.js");

test("cleanPath: relative folder segments only, traversal and junk dropped", () => {
  assert.equal(cleanPath("photos"), "photos");
  assert.equal(cleanPath("photos/2024"), "photos/2024");
  assert.equal(cleanPath("photos\\2024"), "photos/2024");
  for (const bad of [
    "",
    "../x",
    "a/../b",
    "/abs",
    "a//b",
    "a/./b",
    "a/",
    "a/\x01/b",
    "a".repeat(1025),
    `a/${"b".repeat(256)}`,
  ])
    assert.equal(cleanPath(bad), "", JSON.stringify(bad));
  assert.equal(cleanPath(42), "");
  assert.equal(cleanPath(undefined), "");
});

test("folderOf: the shared top folder of a pick, or nothing", () => {
  assert.equal(
    folderOf([{ path: "photos" }, { path: "photos/2024" }]),
    "photos",
  );
  assert.equal(folderOf([{ path: "photos" }, { path: "docs" }]), "");
  assert.equal(folderOf([{ path: "photos" }, { name: "loose.txt" }]), "");
  assert.equal(folderOf([]), "");
});

test("labelOf: folder and name joined for display", () => {
  assert.equal(
    labelOf({ path: "photos/2024", name: "a.jpg" }),
    "photos/2024/a.jpg",
  );
  assert.equal(labelOf({ path: "", name: "a.jpg" }), "a.jpg");
  assert.equal(labelOf({ name: "a.jpg" }), "a.jpg");
});
