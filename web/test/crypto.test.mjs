// Run with `make test` or `node --test "web/test/**/*.test.mjs"` (Node 22+). No dependencies: Node's global
// WebCrypto is the same API the browser modules call.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  keypair,
  shared,
  seal,
  open,
  randomKey,
  importSecret,
} from "../static/js/crypto.js";

const enc = (s) => new TextEncoder().encode(s);
const dec = (b) => new TextDecoder().decode(b);

test("per-transfer ECDH: same key and code on both sides, sealed chunks bound to counter and aad", async () => {
  const a = await keypair();
  const b = await keypair();
  const sa = await shared(a.priv, a.pub, b.pub);
  const sb = await shared(b.priv, b.pub, a.pub);
  assert.equal(sa.sas, sb.sas);
  assert.match(sa.sas, /^[23456789ABCDEFGHJKLMNPQRSTUVWXYZ]{4}$/);

  const aad = enc("transfer-id");
  const box = await seal(sa.key, 7, aad, enc("hello"));
  assert.equal(dec(await open(sb.key, 7, aad, box)), "hello");
  await assert.rejects(open(sb.key, 8, aad, box), "wrong counter must fail");
  await assert.rejects(open(sb.key, 7, enc("x"), box), "wrong aad must fail");

  const stranger = await keypair();
  const ss = await shared(stranger.priv, stranger.pub, a.pub);
  await assert.rejects(open(ss.key, 7, aad, box), "third party must fail");
});

test("drop links: the URL-fragment secret round-trips to a working key", async () => {
  const { key, secret } = await randomKey();
  assert.match(secret, /^[A-Za-z0-9_-]{43}$/);
  const box = await seal(key, 0, enc("aad"), enc("payload"));
  assert.equal(
    dec(await open(await importSecret(secret), 0, enc("aad"), box)),
    "payload",
  );
});

test("frame prefix: uuid string <-> 16 bytes round-trips", async () => {
  globalThis.matchMedia = () => ({}); // util.js touches it at import time
  const { uuid, uuidBytes, bytesToUUID } = await import("../static/js/util.js");
  const id = uuid();
  assert.match(
    id,
    /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
  );
  assert.equal(uuidBytes(id).length, 16);
  assert.equal(bytesToUUID(uuidBytes(id)), id);
});
