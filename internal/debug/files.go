package debug

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"magi/internal/core/protocol"
)

var errCapture = errors.New("debug capture failed; check private directory and release file")

func privateDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errCapture
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errCapture
	}
	return nil
}

func rootDirectory() (string, error) {
	dir := os.Getenv("MAGI_DEBUG_API_DIR")
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("MAGI_DEBUG_API_DIR must be absolute")
	}
	if err := privateDirectory(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Every role Context owns one session directory. Only the latest reply request lives at root.
func sessionDirectory(ctx context.Context) (string, error) {
	root, err := rootDirectory()
	if err != nil || root == "" {
		return "", err
	}
	session := protocol.DiagnosticIdentityFromContext(ctx).SessionID
	if session == "" {
		session = "unscoped"
	}
	if session == "." || session == ".." || strings.ContainsAny(session, `/\`) {
		return "", errCapture
	}
	dir := filepath.Join(root, session)
	if err := privateDirectory(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func writeCapture(root, kind, name string, raw []byte) error {
	dir := filepath.Join(root, kind)
	if err := privateDirectory(dir); err != nil {
		return err
	}
	return writeProbeFile(dir, name, raw)
}

func writeProbeFile(dir, name string, raw []byte) error {
	f, err := os.CreateTemp(dir, ".api-debug-")
	if err != nil {
		return errCapture
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(append(raw, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return errCapture
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return errCapture
	}
	return nil
}
