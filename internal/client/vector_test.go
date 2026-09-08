package client

import (
	"crypto/ecdh"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
)

// The same vector is checked against WebCrypto in web/test/crypto.test.mjs,
// so a terminal and a browser are proven to agree on every byte: the
// commitment, the verification code, and a sealed chunk.
const (
	vectorID    = "0f1e2d3c-4b5a-4978-8796-a5b4c3d2e1f0"
	vectorSeq   = 3
	vectorPlain = "beam interop vector"

	vectorPubA   = "BFFcPW6545a5BNP+yn9U/c0MwemXvzddylFa0KbDtANfRTa+OlDzGPv5pUdZAqIhUCvvDVfgjFOyzApW8X2fk1Q="
	vectorPubB   = "BB8UAUa/sbJR+E9N2+DUzc/Xev2YSpUg41eUAh+DErue7JlaCLH6dwTfPcwLUKlmUmP7dxH5X5+KRJxQluR8iSs="
	vectorCommit = "QmmIlDHjExlm/K9qRXFBlD7Sw1tbkXrmLLM5VG9SNVE="
	vectorSAS    = "NN74"
	vectorBox    = "JaDYl764NTv1GNQ6WVOIlfISPRi4d9UliodJOYKmR3KIy5Y="
)

func vectorKey(t *testing.T, first byte) keypair {
	t.Helper()
	scalar := make([]byte, 32)
	for i := range scalar {
		scalar[i] = first + byte(i)
	}
	priv, err := ecdh.P256().NewPrivateKey(scalar)
	if err != nil {
		t.Fatal(err)
	}
	return keypair{priv: priv, pub: base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())}
}

func TestBrowserVector(t *testing.T) {
	a, b := vectorKey(t, 1), vectorKey(t, 33)
	commit, err := commitment(a.pub)
	if err != nil {
		t.Fatal(err)
	}
	keyA, sasA, err := shared(a, b.pub)
	if err != nil {
		t.Fatal(err)
	}
	keyB, sasB, err := shared(b, a.pub)
	if err != nil {
		t.Fatal(err)
	}
	if sasA != sasB {
		t.Fatalf("codes differ: %s vs %s", sasA, sasB)
	}
	id := uuid.MustParse(vectorID)
	box := sealChunk(keyA, vectorSeq, id, nil, []byte(vectorPlain))
	plain, err := openChunk(keyB, vectorSeq, id, nil, box)
	if err != nil || string(plain) != vectorPlain {
		t.Fatalf("open: %v %q", err, plain)
	}
	got := map[string]string{
		"pubA": a.pub, "pubB": b.pub, "commit": commit, "sas": sasA,
		"box": base64.StdEncoding.EncodeToString(box),
	}
	want := map[string]string{
		"pubA": vectorPubA, "pubB": vectorPubB, "commit": vectorCommit, "sas": vectorSAS, "box": vectorBox,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %s, want %s", k, got[k], w)
		}
	}
}
