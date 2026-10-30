package tools

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"magi/internal/storage"
)

// workspace reads the current project files. It creates no workspace snapshots.
type workspace struct{ root string }

var errSpecialFile = errors.New("project path is not a regular file or symbolic link")

func newWorkspace(root string) (*workspace, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("project root is not a directory")
	}
	return &workspace{root: absolute}, nil
}

func privateComponent(component string) bool {
	return component == ".magi" || component == ".git"
}

// open opens a current public file or directory without following links.
// For a symbolic link, it returns only the link text and no file.
func (b *workspace) open(ctx context.Context, path string) (*os.File, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if path == "" || filepath.IsAbs(path) {
		return nil, nil, fs.ErrPermission
	}
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".." || privateComponent(component) {
			return nil, nil, fs.ErrPermission
		}
	}
	path = filepath.ToSlash(filepath.Clean(path))
	components := strings.Split(path, "/")
	// Hold each directory while opening the next component with O_NOFOLLOW.
	// A link cannot redirect a regular-file read into private or external state.
	directory, err := os.Open(b.root)
	if err != nil {
		return nil, nil, err
	}
	defer func() { directory.Close() }()
	for _, component := range components[:len(components)-1] {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		fd, err := syscall.Openat(int(directory.Fd()), component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, nil, err
		}
		directory.Close()
		directory = os.NewFile(uintptr(fd), component)
		owned, err := storage.IsPrivateInitializationDirAt(directory, component)
		if err != nil {
			return nil, nil, err
		}
		if owned {
			return nil, nil, fs.ErrPermission
		}
	}
	name := components[len(components)-1]
	file, info, err := openEntryAt(directory.Fd(), name)
	if errors.Is(err, syscall.ELOOP) {
		target, readErr := readLinkAt(directory.Fd(), name)
		return nil, target, readErr
	}
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		owned, err := storage.IsPrivateInitializationDirAt(file, name)
		if err != nil || owned {
			file.Close()
			if err == nil {
				err = fs.ErrPermission
			}
			return nil, nil, err
		}
	}
	return file, nil, nil
}

// Read only the link text through its held parent, even if that parent is renamed.
func readLinkAt(directoryFD uintptr, name string) ([]byte, error) {
	for size := 128; ; size *= 2 {
		buffer := make([]byte, size)
		n, err := unix.Readlinkat(int(directoryFD), name, buffer)
		if err != nil {
			return nil, err
		}
		if n < len(buffer) {
			return buffer[:n], nil
		}
	}
}

// openEntryAt opens one public path component relative to its held parent.
// Callers check private names and initialization directories before traversing them.
func openEntryAt(directoryFD uintptr, name string) (*os.File, fs.FileInfo, error) {
	fd, err := syscall.Openat(int(directoryFD), name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, errSpecialFile
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !info.IsDir() && (!ok || stat.Nlink != 1) {
		file.Close()
		return nil, nil, fs.ErrPermission
	}
	return file, info, nil
}

// openPublicDirectory checks a child through its held parent. Private staging
// directories return nil without an error; changed entries remain distinguishable.
func openPublicDirectory(directoryFD uintptr, name string) (*os.File, error) {
	child, info, err := openEntryAt(directoryFD, name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		child.Close()
		return nil, syscall.ENOTDIR
	}
	private, err := storage.IsPrivateInitializationDirAt(child, name)
	if err != nil || private {
		child.Close()
		return nil, err
	}
	return child, nil
}

// Only descendant-entry failures are skippable. Errors on the requested root
// and cancellation still fail the operation.
func skippableEntryError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, errSpecialFile) ||
		errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.ELOOP)
}

// walk visits files in the selected scope and stops as soon as visit returns false.
// The callback may borrow the held parent directory to open a child; nil denotes
// an explicit file/link target. The directory must not be closed or retained.
// descend may prune directories before they are opened; nil visits every directory.
// Directory entries are read in bounded batches, with no full-tree manifest.
// Special nodes encountered during traversal are skipped without opening them.
// Missing, replaced or unreadable descendants are skipped and reported as incomplete.
func (b *workspace) walk(ctx context.Context, path string, descend func(string) bool, visit func(string, bool, *os.File) (bool, error)) (incomplete bool, err error) {
	file, link, err := b.open(ctx, path)
	if err != nil {
		return false, err
	}
	if file == nil {
		_, err = visit(filepath.ToSlash(filepath.Clean(path)), link != nil, nil)
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		_, err = visit(filepath.ToSlash(filepath.Clean(path)), false, nil)
		return false, err
	}
	skip := func(err error) bool {
		if skippableEntryError(err) {
			incomplete = true
			return true
		}
		return false
	}
	var walk func(*os.File, string) (bool, error)
	walk = func(dir *os.File, prefix string) (bool, error) {
		for {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			entries, readErr := dir.ReadDir(64)
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				if privateComponent(entry.Name()) || entry.Name() == tempDirectory {
					continue
				}
				if !utf8.ValidString(entry.Name()) {
					incomplete = true
					continue
				}
				name := filepath.ToSlash(filepath.Join(prefix, entry.Name()))
				if entry.IsDir() {
					if descend != nil && !descend(name) {
						continue
					}
					child, err := openPublicDirectory(dir.Fd(), entry.Name())
					if skip(err) {
						continue
					}
					if err != nil {
						return false, err
					}
					if child == nil {
						continue
					}
					keep, err := walk(child, name)
					child.Close()
					if skip(err) {
						continue
					}
					if err != nil || !keep {
						return keep, err
					}
				} else {
					if !entry.Type().IsRegular() && entry.Type()&fs.ModeSymlink == 0 {
						continue
					}
					keep, err := visit(name, entry.Type()&fs.ModeSymlink != 0, dir)
					if skip(err) {
						continue
					}
					if err != nil || !keep {
						return keep, err
					}
				}
			}
			if readErr == io.EOF {
				return true, nil
			}
			if readErr != nil {
				return false, readErr
			}
		}
	}
	_, err = walk(file, filepath.Clean(path))
	return incomplete, err
}
