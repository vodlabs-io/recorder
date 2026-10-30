package preview

import (
	"bytes"
	"image/jpeg"
	"testing"
)

func jpegSize(t *testing.T, b []byte) (int, int) {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}
