package webtoken

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

func TestResolveGenerates(t *testing.T) {
	dir := t.TempDir()
	tok, err := Resolve(vlconfig.WebConfig{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !tok.Created || tok.Source != Generated || len(tok.Value) < MinLength {
		t.Fatalf("unexpected token %+v", tok)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(Path(dir))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode().Perm())
		}
	}
	again, err := Resolve(vlconfig.WebConfig{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Value != tok.Value {
		t.Error("second resolve did not reuse the token")
	}
}

func TestResolveOrder(t *testing.T) {
	dir := t.TempDir()
	pinned := strings.Repeat("v", MinLength)
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("f", MinLength)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := Resolve(vlconfig.WebConfig{Token: pinned, TokenFile: secret}, dir)
	if err != nil || tok.Source != PinnedFile || tok.Value != strings.Repeat("f", MinLength) {
		t.Fatalf("token_file did not win: %+v %v", tok, err)
	}
	tok, err = Resolve(vlconfig.WebConfig{Token: pinned}, dir)
	if err != nil || tok.Source != PinnedValue || tok.Value != pinned {
		t.Fatalf("token did not win over the generated file: %+v %v", tok, err)
	}
	if _, err := os.Stat(Path(dir)); err == nil {
		t.Error("a pinned token still generated web-token")
	}
}

func TestResolveRejectsShortPin(t *testing.T) {
	if _, err := Resolve(vlconfig.WebConfig{Token: "short"}, t.TempDir()); err == nil {
		t.Fatal("short token accepted")
	}
}

func TestRotate(t *testing.T) {
	dir := t.TempDir()
	first, err := Resolve(vlconfig.WebConfig{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := Rotate(vlconfig.WebConfig{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Value == first.Value {
		t.Error("rotate kept the token")
	}
	if now, _ := Resolve(vlconfig.WebConfig{}, dir); now.Value != rotated.Value {
		t.Error("rotated token not persisted")
	}
	if _, err := Rotate(vlconfig.WebConfig{Token: strings.Repeat("v", MinLength)}, dir); err == nil {
		t.Error("rotated a pinned token")
	}
}
