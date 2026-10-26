package core

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"decred.org/dcrdex/dex/encrypt"
)

func TestMeshSwapStoreKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh", "swapstore.key")
	crypter := encrypt.NewCrypter([]byte("pw"))
	k1, err := meshSwapStoreKey(path, crypter)
	if err != nil || len(k1) != 32 {
		t.Fatalf("new key: %v", err)
	}
	onDisk, _ := os.ReadFile(path)
	if bytes.Contains(onDisk, k1) {
		t.Fatal("store key on disk in the clear")
	}
	k2, err := meshSwapStoreKey(path, crypter)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatalf("reload: %v", err)
	}
	if _, err := meshSwapStoreKey(path, encrypt.NewCrypter([]byte("other"))); err == nil {
		t.Fatal("wrong crypter decrypted the key")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp key file left behind: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mesh dir mode %v", fi.Mode().Perm())
	}
}
