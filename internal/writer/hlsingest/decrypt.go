package hlsingest

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
)

// decryptSegment decrypts a segment under whole-segment AES-128, reporting
// whether it did. seqID is the segment's media sequence number, the IV when
// the key names none. Anything else (no key body, SAMPLE-AES) is left as it
// arrived, for the Sink to judge.
func decryptSegment(key *Key, seqID uint64, body []byte) ([]byte, bool, error) {
	if key == nil || len(key.Body) != 16 {
		return body, false, nil
	}
	if key.Method != "AES-128" && key.Method != "AES128" {
		return body, false, nil
	}
	iv := key.IV
	if len(iv) == 0 {
		iv = make([]byte, 16)
		n := seqID
		for i := 0; i < 8; i++ {
			iv[15-i] = byte(n)
			n >>= 8
		}
	}
	dec, err := decryptAES128CBC(body, key.Body, iv)
	if err != nil {
		return body, false, err
	}
	return dec, true, nil
}

func decryptAES128CBC(ciphertext, key, iv []byte) ([]byte, error) {
	if len(key) != 16 || len(iv) != 16 {
		return nil, errors.New("invalid key/iv size for AES-128-CBC")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		pad := aes.BlockSize - (len(ciphertext) % aes.BlockSize)
		ciphertext = append(ciphertext, make([]byte, pad)...)
	}
	out := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ciphertext)
	// Strip PKCS#7 padding when it is well formed.
	if n := len(out); n > 0 {
		pad := int(out[n-1])
		if pad > 0 && pad <= aes.BlockSize && pad <= n {
			good := true
			for i := 0; i < pad; i++ {
				if out[n-1-i] != byte(pad) {
					good = false
					break
				}
			}
			if good {
				out = out[:n-pad]
			}
		}
	}
	return out, nil
}
