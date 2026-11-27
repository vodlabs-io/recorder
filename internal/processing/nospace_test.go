package processing

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"testing"
)

func TestFFmpegNoSpace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exits through sh")
	}
	exit := func(code int) error {
		return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	}
	// AVERROR(ENOSPC) is -28, which Unix reports as 228.
	if err := exit(228); !ffmpegNoSpace(fmt.Errorf("wrapped: %w", err)) {
		t.Fatalf("exit 228 (%v) not read as a full disk", err)
	}
	if err := exit(1); ffmpegNoSpace(err) {
		t.Fatal("exit 1 read as a full disk")
	}
	if ffmpegNoSpace(errors.New("not an exit")) {
		t.Fatal("a plain error read as a full disk")
	}
}
