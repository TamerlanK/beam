// Run with `make test` or `node --test "web/test/**/*.test.mjs"` (Node 22+). No dependencies: Node's global
// WebCrypto is the same API the browser modules call.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  keypair,
  shared,
  commit,
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

test("commitment: binds the offer to one public key", async () => {
  const a = await keypair();
  const c = await commit(a.pub);
  assert.match(c, /^[A-Za-z0-9+/]{43}=$/);
  assert.equal(await commit(a.pub), c);
  assert.notEqual(await commit((await keypair()).pub), c);
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

test("interop: a chunk sealed by the Go client opens here with the same verification code", async () => {
  // Pinned in internal/client/vector_test.go: the sender's key is the scalar
  // 1..32, ours is 33..64; a Go sender sealed chunk 3 of that transfer.
  const pubA =
    "BFFcPW6545a5BNP+yn9U/c0MwemXvzddylFa0KbDtANfRTa+OlDzGPv5pUdZAqIhUCvvDVfgjFOyzApW8X2fk1Q=";
  const pubB =
    "BB8UAUa/sbJR+E9N2+DUzc/Xev2YSpUg41eUAh+DErue7JlaCLH6dwTfPcwLUKlmUmP7dxH5X5+KRJxQluR8iSs=";
  const bytes = (b64) => Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
  const b64url = (b) =>
    btoa(String.fromCharCode(...b))
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
  const raw = bytes(pubB);
  const priv = await crypto.subtle.importKey(
    "jwk",
    {
      kty: "EC",
      crv: "P-256",
      d: b64url(Uint8Array.from({ length: 32 }, (_, i) => 33 + i)),
      x: b64url(raw.subarray(1, 33)),
      y: b64url(raw.subarray(33)),
    },
    { name: "ECDH", namedCurve: "P-256" },
    false,
    ["deriveKey"],
  );
  assert.equal(
    await commit(pubA),
    "QmmIlDHjExlm/K9qRXFBlD7Sw1tbkXrmLLM5VG9SNVE=",
  );
  const { key, sas } = await shared(priv, pubB, pubA);
  assert.equal(sas, "NN74");
  const id = Uint8Array.from(
    "0f1e2d3c4b5a49788796a5b4c3d2e1f0".match(/../g),
    (h) => parseInt(h, 16),
  );
  const box = bytes("JaDYl764NTv1GNQ6WVOIlfISPRi4d9UliodJOYKmR3KIy5Y=");
  assert.equal(dec(await open(key, 3, id, box)), "beam interop vector");
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
