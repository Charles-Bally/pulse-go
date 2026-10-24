package pulse

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
)

const (
	kemID      = 0x0020
	kdfID      = 0x0001
	aes128GCM  = 0x0001
	aes256GCM  = 0x0002
	nonceBytes = 12
)

func i2osp(value, length int) []byte {
	out := make([]byte, length)
	for index := length - 1; index >= 0; index-- {
		out[index] = byte(value)
		value >>= 8
	}
	return out
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func hkdfExtract(salt, ikm []byte) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

func hkdfExpand(prk, info []byte, length int) []byte {
	var out, previous []byte
	for counter := 1; len(out) < length; counter++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(previous)
		mac.Write(info)
		mac.Write([]byte{byte(counter)})
		previous = mac.Sum(nil)
		out = append(out, previous...)
	}
	return out[:length]
}

type suite []byte

func (s suite) prefix(label string) []byte { return concat([]byte("HPKE-v1"), s, []byte(label)) }

func (s suite) extract(salt []byte, label string, ikm []byte) []byte {
	return hkdfExtract(salt, concat(s.prefix(label), ikm))
}

func (s suite) expand(prk []byte, label string, info []byte, length int) []byte {
	return hkdfExpand(prk, concat(i2osp(length, 2), s.prefix(label), info), length)
}

var kemSuite = suite(concat([]byte("KEM"), i2osp(kemID, 2)))

func aeadKeyBytes(aead int) int {
	if aead == aes128GCM {
		return 16
	}
	return 32
}

func sharedSecret(dh, enc, recipient []byte) []byte {
	prk := kemSuite.extract(nil, "eae_prk", dh)
	return kemSuite.expand(prk, "shared_secret", concat(enc, recipient), sha256.Size)
}

func keySchedule(shared, info []byte, aead int) (cipher.AEAD, []byte, error) {
	s := suite(concat([]byte("HPKE"), i2osp(kemID, 2), i2osp(kdfID, 2), i2osp(aead, 2)))
	context := concat([]byte{0}, s.extract(nil, "psk_id_hash", nil), s.extract(nil, "info_hash", info))
	secret := s.extract(shared, "secret", nil)
	block, err := aes.NewCipher(s.expand(secret, "key", context, aeadKeyBytes(aead)))
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	return gcm, s.expand(secret, "base_nonce", context, nonceBytes), err
}

// RFC 9180, mode de base, un seul message : le numéro de séquence 0 garde le nonce de base tel quel.
func hpkeSeal(recipient, info, aad, plaintext []byte, aead int, ephemeral *ecdh.PrivateKey) (enc, ct []byte, err error) {
	if ephemeral == nil {
		if ephemeral, err = ecdh.X25519().GenerateKey(rand.Reader); err != nil {
			return nil, nil, err
		}
	}
	public, err := ecdh.X25519().NewPublicKey(recipient)
	if err != nil {
		return nil, nil, err
	}
	dh, err := ephemeral.ECDH(public)
	if err != nil {
		return nil, nil, err
	}
	enc = ephemeral.PublicKey().Bytes()
	gcm, nonce, err := keySchedule(sharedSecret(dh, enc, recipient), info, aead)
	if err != nil {
		return nil, nil, err
	}
	return enc, gcm.Seal(nil, nonce, plaintext, aad), nil
}

func hpkeOpen(recipientPrivate, info, aad, enc, ct []byte, aead int) ([]byte, error) {
	private, err := ecdh.X25519().NewPrivateKey(recipientPrivate)
	if err != nil {
		return nil, err
	}
	sender, err := ecdh.X25519().NewPublicKey(enc)
	if err != nil {
		return nil, err
	}
	dh, err := private.ECDH(sender)
	if err != nil {
		return nil, err
	}
	gcm, nonce, err := keySchedule(sharedSecret(dh, enc, private.PublicKey().Bytes()), info, aead)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ct, aad)
}
