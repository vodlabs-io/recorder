// Package webtoken resolves the Web UI's sign-in token. The daemon and
// `vod web` share it, so both always agree on which token is in force.
package webtoken

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// FileName is the generated token's file in the config folder.
const FileName = "web-token"

// MinLength is the shortest pinned token accepted. A generated one is 43
// characters (32 random bytes).
const MinLength = 32

// Source says where a token came from.
type Source int

const (
	// Generated is the random token in <config>/web-token.
	Generated Source = iota
	// PinnedFile is web.token_file, such as a Docker secret.
	PinnedFile
	// PinnedValue is web.token or VLREC_WEB_TOKEN.
	PinnedValue
)

// Token is the resolved sign-in token. Value is secret: never log it.
type Token struct {
	Value  string
	Source Source
	// Path is the file the token was read from; empty for PinnedValue.
	Path string
	// Created reports that this resolve generated the token.
	Created bool
}

// Pinned reports whether the token is set outside the config folder, where
// `vod web token --rotate` must not replace it.
func (t Token) Pinned() bool { return t.Source != Generated }

// Describe names where the token is set, for messages.
func (t Token) Describe() string {
	switch t.Source {
	case PinnedFile:
		return "web.token_file (" + t.Path + ")"
	case PinnedValue:
		return "web.token / VLREC_WEB_TOKEN"
	}
	return t.Path
}

// Resolve returns the token in force: web.token_file, then web.token, then
// the generated file in configDir, created on first use.
func Resolve(cfg vlconfig.WebConfig, configDir string) (Token, error) {
	if p := strings.TrimSpace(cfg.TokenFile); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return Token{}, fmt.Errorf("read web.token_file: %w", err)
		}
		v := strings.TrimSpace(string(b))
		if err := checkPinned(v, "web.token_file "+p); err != nil {
			return Token{}, err
		}
		return Token{Value: v, Source: PinnedFile, Path: p}, nil
	}
	if v := strings.TrimSpace(cfg.Token); v != "" {
		if err := checkPinned(v, "web.token (VLREC_WEB_TOKEN)"); err != nil {
			return Token{}, err
		}
		return Token{Value: v, Source: PinnedValue}, nil
	}
	return generated(Path(configDir))
}

// Path is the generated token's file in configDir.
func Path(configDir string) string { return filepath.Join(configDir, FileName) }

func checkPinned(v, where string) error {
	if len(v) < MinLength {
		return fmt.Errorf("%s is %d characters; the Web UI needs at least %d "+
			"(e.g. `openssl rand -base64 32`)", where, len(v), MinLength)
	}
	return nil
}

func generated(path string) (Token, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		v := strings.TrimSpace(string(b))
		if len(v) >= MinLength {
			return Token{Value: v, Source: Generated, Path: path}, nil
		}
		// Empty or truncated: replace it below.
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Token{}, fmt.Errorf("read %s: %w", path, err)
	}
	v, err := write(path)
	if err != nil {
		return Token{}, err
	}
	return Token{Value: v, Source: Generated, Path: path, Created: true}, nil
}

// Rotate replaces the generated token in configDir with a fresh one. It
// refuses a pinned token, which lives where the user set it.
func Rotate(cfg vlconfig.WebConfig, configDir string) (Token, error) {
	cur, err := Resolve(cfg, configDir)
	if err != nil {
		return Token{}, err
	}
	if cur.Pinned() {
		return Token{}, fmt.Errorf("the token is pinned by %s; change it there and restart the container", cur.Describe())
	}
	v, err := write(cur.Path)
	if err != nil {
		return Token{}, err
	}
	return Token{Value: v, Source: Generated, Path: cur.Path, Created: true}, nil
}

// New returns a fresh URL-safe random token.
func New() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// write puts a fresh token in path through a 0600 temp file, so a reader
// never sees a partial one.
func write(path string) (string, error) {
	v, err := New()
	if err != nil {
		return "", err
	}
	if err := WriteSecret(path, []byte(v+"\n")); err != nil {
		return "", err
	}
	return v, nil
}

// WriteSecret atomically replaces path with data, readable only by its owner.
func WriteSecret(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
