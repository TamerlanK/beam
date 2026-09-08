package client

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"sort"

	"github.com/TamerlanK/beam/internal/protocol"
	"github.com/google/uuid"
)

// Mirrors web/static/js/crypto.js so a terminal and a browser agree on
// every byte: ephemeral P-256 ECDH, the raw shared secret used directly as
// the AES-256-GCM key (what WebCrypto's deriveKey does), IV = 96-bit
// big-endian chunk index, AAD = the 16-byte transfer id.

type keypair struct {
	priv *ecdh.PrivateKey
	pub  string // raw uncompressed point, base64
}

func newKeypair() (keypair, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return keypair{}, err
	}
	return keypair{priv: priv, pub: base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())}, nil
}

// commitment is what an offer carries instead of the key: base64 of the
// SHA-256 of the raw point. The key itself follows in transfer-key, after
// the receiver has answered with its own.
func commitment(pub string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// shared derives the transfer key and the 4-character verification code
// both sides display; a relay that swapped keys yields mismatched codes.
func shared(kp keypair, theirPub string) (cipher.AEAD, string, error) {
	raw, err := base64.StdEncoding.DecodeString(theirPub)
	if err != nil {
		return nil, "", err
	}
	pub, err := ecdh.P256().NewPublicKey(raw)
	if err != nil {
		return nil, "", err
	}
	secret, err := kp.priv.ECDH(pub)
	if err != nil {
		return nil, "", err
	}
	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", err
	}
	keys := []string{kp.pub, theirPub}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(keys[0] + "|" + keys[1]))
	sas := make([]byte, 4)
	for i, b := range sum[:4] {
		sas[i] = protocol.RoomCodeAlphabet[b%32]
	}
	return aead, string(sas), nil
}

func nonce(seq uint32) []byte {
	iv := make([]byte, 12)
	binary.BigEndian.PutUint32(iv[8:], seq)
	return iv
}

func sealChunk(aead cipher.AEAD, seq uint32, id uuid.UUID, dst, plain []byte) []byte {
	return aead.Seal(dst, nonce(seq), plain, id[:])
}

func openChunk(aead cipher.AEAD, seq uint32, id uuid.UUID, dst, sealed []byte) ([]byte, error) {
	return aead.Open(dst, nonce(seq), sealed, id[:])
}
