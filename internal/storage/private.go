package storage

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const initMarker = ".magi-private-init-v3"
const initMarkerPrefix = "magi private initialization v3 "

// writeInitializationMarker identifies a stage by its eventual published name.
func writeInitializationMarker(stage, publishedStage string) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	body := []byte(initMarkerPrefix + filepath.Base(publishedStage) + " " + hex.EncodeToString(nonce[:]) + "\n")
	return os.WriteFile(filepath.Join(stage, initMarker), body, 0600)
}

func validInitMarker(stage string, body []byte) bool {
	prefix := initMarkerPrefix + filepath.Base(stage) + " "
	if !bytes.HasPrefix(body, []byte(prefix)) || len(body) != len(prefix)+32+1 || body[len(body)-1] != '\n' {
		return false
	}
	nonce := body[len(prefix) : len(body)-1]
	decoded := make([]byte, 16)
	n, err := hex.Decode(decoded, nonce)
	return err == nil && n == len(decoded) && hex.EncodeToString(decoded) == string(nonce)
}

// IsPrivateInitializationDir recognizes a published private stage even after
// a whole-project copy changes filesystem inode numbers.
func IsPrivateInitializationDir(path string) (bool, error) {
	if !strings.HasPrefix(filepath.Base(path), ".magi-init-") {
		return false, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	directory := os.NewFile(uintptr(fd), path)
	defer directory.Close()
	return IsPrivateInitializationDirAt(directory, filepath.Base(path))
}

// IsPrivateInitializationDirAt checks the marker through an already opened
// directory, so tools never re-resolve a checked parent by its pathname.
func IsPrivateInitializationDirAt(directory *os.File, name string) (bool, error) {
	if !strings.HasPrefix(name, ".magi-init-") {
		return false, nil
	}
	fd, err := syscall.Openat(int(directory.Fd()), initMarker, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if os.IsNotExist(err) || err == syscall.ELOOP {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), initMarker)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	expectedSize := len(initMarkerPrefix) + len(name) + 1 + 32 + 1
	if !info.Mode().IsRegular() || validatePrivateEntry(initMarker, info) != nil || info.Size() != int64(expectedSize) {
		return false, nil
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(expectedSize+1)))
	if err != nil {
		return false, err
	}
	return validInitMarker(name, body), nil
}

// validatePrivateEntry rejects links and special files in private storage.
func validatePrivateEntry(path string, info os.FileInfo) error {
	if info.IsDir() {
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("project state path must be a physical regular file or directory: %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return fmt.Errorf("project state file must have a single link: %s", path)
	}
	return nil
}
