package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"magi/internal/storage"
)

const mutationDirectoryFlags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
const mutationReadFlags = syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC

const maxTextFileBytes = 16 << 20

// One synchronous mutation owns its directory handles until publication and cleanup finish.
type fileMutation struct {
	ctx  context.Context
	root *os.File
}

func openFileMutation(ctx context.Context, root string) (*fileMutation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(root, mutationDirectoryFlags, 0)
	if err != nil {
		return nil, err
	}
	return &fileMutation{ctx: ctx, root: os.NewFile(uintptr(fd), root)}, nil
}

type fileSnapshot struct {
	body []byte
	info syscall.Stat_t
}

func sameFileIdentity(a, b syscall.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim && a.Mode == b.Mode && a.Nlink == b.Nlink
}

// parent holds every directory while opening the next without following links.
// write may create missing parents; edit only walks existing directories.
func (e *fileMutation) parent(path string, create bool) (*os.File, string, error) {
	if err := e.ctx.Err(); err != nil {
		return nil, "", err
	}
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	fd, err := syscall.Openat(int(e.root.Fd()), ".", mutationDirectoryFlags, 0)
	if err != nil {
		return nil, name, err
	}
	directory := os.NewFile(uintptr(fd), ".")
	defer func() {
		if directory != nil {
			directory.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		if err := e.ctx.Err(); err != nil {
			return nil, name, err
		}
		childFD, err := syscall.Openat(int(directory.Fd()), part, mutationDirectoryFlags, 0)
		if errors.Is(err, syscall.ENOENT) {
			if !create {
				return nil, name, nil
			}
			if err := e.ctx.Err(); err != nil {
				return nil, name, err
			}
			mkdirErr := syscall.Mkdirat(int(directory.Fd()), part, 0755)
			if mkdirErr != nil && !errors.Is(mkdirErr, syscall.EEXIST) {
				return nil, name, mkdirErr
			}
			childFD, err = syscall.Openat(int(directory.Fd()), part, mutationDirectoryFlags, 0)
		}
		if err != nil {
			return nil, name, err
		}
		child := os.NewFile(uintptr(childFD), part)
		private, err := storage.IsPrivateInitializationDirAt(child, part)
		if err != nil || private {
			child.Close()
			if err != nil {
				return nil, name, err
			}
			return nil, name, errors.New("target enters private project initialization")
		}
		directory.Close()
		directory = child
	}
	parent := directory
	directory = nil
	return parent, name, nil
}

func (e *fileMutation) readAt(directory *os.File, name string) (*fileSnapshot, error) {
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
	if directory == nil {
		return nil, nil
	}
	fd, err := syscall.Openat(int(directory.Fd()), name, mutationReadFlags, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var before, after syscall.Stat_t
	if err := syscall.Fstat(fd, &before); err != nil {
		return nil, err
	}
	if before.Mode&syscall.S_IFMT != syscall.S_IFREG || before.Nlink != 1 {
		return nil, errors.New("target must be a regular file with one link")
	}
	if before.Size > maxTextFileBytes {
		return nil, errors.New("target exceeds 16 MiB")
	}
	var body []byte
	buffer := make([]byte, 64<<10)
	for len(body) <= maxTextFileBytes {
		if err := e.ctx.Err(); err != nil {
			return nil, err
		}
		n, err := file.Read(buffer[:min(len(buffer), maxTextFileBytes+1-len(body))])
		body = append(body, buffer[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(body) > maxTextFileBytes {
		return nil, errors.New("target exceeds 16 MiB")
	}
	if err := syscall.Fstat(fd, &after); err != nil {
		return nil, err
	}
	if !sameFileIdentity(before, after) {
		return nil, errors.New("source changed while being read")
	}
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
	return &fileSnapshot{body, after}, nil
}

func (e *fileMutation) requireUnchanged(directory *os.File, name string, expected *fileSnapshot) error {
	actual, err := e.readAt(directory, name)
	if err != nil {
		return err
	}
	if actual == nil && expected == nil {
		return nil
	}
	if actual != nil && expected != nil && sameFileIdentity(actual.info, expected.info) && bytes.Equal(actual.body, expected.body) {
		return nil
	}
	return errors.New("target changed during preparation; reread before retrying")
}

// publish replaces one file atomically after checking its prepared source.
// Cancellation before publication leaves the source untouched; after it, success stands.
func (e *fileMutation) publish(directory *os.File, name string, data []byte, expected *fileSnapshot) (err error) {
	if err := e.ctx.Err(); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := ".magi-write-" + hex.EncodeToString(nonce[:])
	fd, err := syscall.Openat(int(directory.Fd()), temp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), temp)
	remove := true
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if remove {
			err = errors.Join(err, syscall.Unlinkat(int(directory.Fd()), temp))
		}
	}()
	for len(data) > 0 {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		n, err := file.Write(data[:min(len(data), 64<<10)])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	if err := e.ctx.Err(); err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if expected != nil {
		mode = os.FileMode(expected.info.Mode & 0777)
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	err = file.Close()
	file = nil
	if err != nil {
		return err
	}
	if err := e.requireUnchanged(directory, name, expected); err != nil {
		return err
	}
	if err := e.ctx.Err(); err != nil {
		return err
	}
	if expected == nil {
		// Atomic create-if-absent: a concurrent new file must not be overwritten.
		if err := unix.Renameat2(int(directory.Fd()), temp, int(directory.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
	} else {
		if err := syscall.Renameat(int(directory.Fd()), temp, int(directory.Fd()), name); err != nil {
			return err
		}
	}
	remove = false
	return nil
}
