package hlsingest

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func encryptAES128(t *testing.T, plain, key, iv []byte) []byte {
	t.Helper()
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

// TestRotatedKeyAppliesUntilTheNext pins that an EXT-X-KEY partway down a
// playlist decrypts every segment after it, not only the first.
func TestRotatedKeyAppliesUntilTheNext(t *testing.T) {
	k1, k2 := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	iv1, iv2 := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16)
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXT-X-KEY:METHOD=AES-128,URI=\"1.key\",IV=0x01010101010101010101010101010101\n#EXTINF:2.000,\na.ts\n")
		fmt.Fprint(w, "#EXT-X-KEY:METHOD=AES-128,URI=\"2.key\",IV=0x02020202020202020202020202020202\n#EXTINF:2.000,\nb.ts\n#EXTINF:2.000,\nc.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/live/1.key", func(w http.ResponseWriter, r *http.Request) { w.Write(k1) })
	mux.HandleFunc("/live/2.key", func(w http.ResponseWriter, r *http.Request) { w.Write(k2) })
	for name, k := range map[string][2][]byte{"a": {k1, iv1}, "b": {k2, iv2}, "c": {k2, iv2}} {
		body := encryptAES128(t, []byte("segment "+name), k[0], k[1])
		mux.HandleFunc("/live/"+name+".ts", func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sink := &capture{}
	if err := ingestOrigin(t, srv, sink, 0); err != nil {
		t.Fatal(err)
	}
	assertBodies(t, sink.bodies(), "segment a", "segment b", "segment c")
}

// TestAES128SegmentsReachTheSinkDecrypted pins that whole-segment AES-128 is
// undone before the Sink, with the key gone from what it is handed, whether
// the key names an IV or the media sequence number stands in for one; and that
// a segment whose key cannot be fetched still arrives, with its Key.
func TestAES128SegmentsReachTheSinkDecrypted(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := bytes.Repeat([]byte{7}, 16)
	seqIV := make([]byte, 16)
	seqIV[15] = 11 // media sequence 11

	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:10\n")
		fmt.Fprint(w, "#EXT-X-KEY:METHOD=AES-128,URI=\"k.key\",IV=0x07070707070707070707070707070707\n#EXTINF:2.000,\na.ts\n")
		fmt.Fprint(w, "#EXT-X-KEY:METHOD=AES-128,URI=\"k.key\"\n#EXTINF:2.000,\nb.ts\n")
		fmt.Fprint(w, "#EXT-X-KEY:METHOD=AES-128,URI=\"gone.key\"\n#EXTINF:2.000,\nc.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/live/k.key", func(w http.ResponseWriter, r *http.Request) { w.Write(key) })
	mux.HandleFunc("/live/gone.key", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 403) })
	mux.HandleFunc("/live/a.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Write(encryptAES128(t, []byte("segment a"), key, iv))
	})
	mux.HandleFunc("/live/b.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Write(encryptAES128(t, []byte("segment b"), key, seqIV))
	})
	mux.HandleFunc("/live/c.ts", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("cipher c")) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sink := &capture{}
	if err := ingestOrigin(t, srv, sink, 0); err != nil {
		t.Fatal(err)
	}
	assertBodies(t, sink.bodies(), "segment a", "segment b", "cipher c")
	if sink.meta[0].Key != nil || sink.meta[1].Key != nil {
		t.Errorf("decrypted segments kept their Key: %+v %+v", sink.meta[0].Key, sink.meta[1].Key)
	}
	if k := sink.meta[2].Key; k == nil || k.Method != "AES-128" || k.Body != nil {
		t.Errorf("undecryptable segment Key = %+v", k)
	}
}
