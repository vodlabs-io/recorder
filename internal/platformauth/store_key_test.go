package platformauth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// noKeyring is a host with no usable keychain.
type noKeyring struct{}

func (noKeyring) Get(string, string) (string, error) { return "", errors.New("no keychain") }
func (noKeyring) Set(string, string, string) error   { return errors.New("no keychain") }

func headlessStore(dir string, machineIDPaths []string, host string) *Store {
	return &Store{
		filePath:       filepath.Join(dir, cookieFileName),
		keyring:        noKeyring{},
		machineIDPaths: machineIDPaths,
		hostname:       func() (string, error) { return host, nil },
	}
}

var testSession = Session{Platform: "tiktok", Cookies: []Cookie{{Name: "sessionid", Value: "v"}}}

// A container gets a new hostname on every recreate; its logins must survive.
func TestStore_NoMachineIDSurvivesHostnameChange(t *testing.T) {
	dir := t.TempDir()
	if err := headlessStore(dir, nil, "aaa").Set(testSession); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("no %s written: %v", keyFileName, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s mode = %v, want 0600", keyFileName, info.Mode().Perm())
	}
	if _, ok := headlessStore(dir, nil, "bbb").Get("tiktok"); !ok {
		t.Fatal("login lost after the hostname changed")
	}
}

// A host with a machine id keeps the key it has always used.
func TestStore_MachineIDKeyUnchanged(t *testing.T) {
	dir := t.TempDir()
	idPath := filepath.Join(dir, "machine-id")
	if err := os.WriteFile(idPath, []byte("abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := headlessStore(dir, []string{idPath}, "host")
	key, err := s.encryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	if string(key) != string(derivedKey("abc123")) {
		t.Error("machine-id host no longer derives its key from the machine id")
	}
	if _, err := os.Stat(filepath.Join(dir, keyFileName)); err == nil {
		t.Errorf("machine-id host wrote %s", keyFileName)
	}
}

// A jar written under the old hostname-derived key is read and re-saved under
// the file key.
func TestStore_MigratesHostnameKey(t *testing.T) {
	dir := t.TempDir()
	old := &Store{filePath: filepath.Join(dir, cookieFileName), keyOverride: derivedKey("oldhost")}
	if err := old.Set(testSession); err != nil {
		t.Fatal(err)
	}

	if _, ok := headlessStore(dir, nil, "oldhost").Get("tiktok"); !ok {
		t.Fatal("legacy jar not readable")
	}
	if _, err := os.Stat(filepath.Join(dir, keyFileName)); err != nil {
		t.Fatalf("migration did not create %s: %v", keyFileName, err)
	}
	// Re-saved: a new hostname now reads it through the file key alone.
	if _, ok := headlessStore(dir, nil, "newhost").Get("tiktok"); !ok {
		t.Fatal("jar was not re-saved under the file key")
	}
}

func TestStore_CorruptKeyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, keyFileName), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := headlessStore(dir, nil, "h").Set(testSession); err == nil {
		t.Fatal("Set succeeded with a corrupt key file")
	}
}
