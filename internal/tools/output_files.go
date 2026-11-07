package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const tempDirectory = ".magi-temp"
const outputDirectory = tempDirectory + "/"

// Call IDs form flat filenames, never paths supplied by a command.
func isOutputPath(path string) bool {
	if !strings.HasPrefix(path, outputDirectory) {
		return false
	}
	name := strings.TrimPrefix(path, outputDirectory)
	id, channel, ok := strings.Cut(name, ".")
	if !ok || (channel != "stdout" && channel != "stderr") || len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Hold the temporary directory while opening files; never follow links.
func outputDir(root string, create bool) (*os.File, error) {
	parent, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer parent.Close()

	fd, err := syscall.Openat(int(parent.Fd()), tempDirectory, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) && create {
		// Workers may remove scratch files before reporting. Restore capture for
		// later calls without clearing any output already created by a sibling.
		if err := syscall.Mkdirat(int(parent.Fd()), tempDirectory, 0700); err != nil && !errors.Is(err, syscall.EEXIST) {
			return nil, err
		}
		fd, err = syscall.Openat(int(parent.Fd()), tempDirectory, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err == nil {
			dir := os.NewFile(uintptr(fd), tempDirectory)
			ignoreFD, err := syscall.Openat(fd, ".gitignore", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
			if err == nil {
				ignore := os.NewFile(uintptr(ignoreFD), ".gitignore")
				_, writeErr := ignore.WriteString("*\n")
				err = errors.Join(writeErr, ignore.Close())
			}
			if err != nil && !errors.Is(err, syscall.EEXIST) {
				dir.Close()
				return nil, err
			}
			return dir, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), tempDirectory), nil
}

// PrepareRound runs before Planner and any Workers start. Output survives until
// the next round, including a stopped process's diagnostic files.
func (r *runtime) PrepareRound(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(r.workspace.root, tempDirectory)
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	// Keep disposable logs out of Git submissions without changing project rules.
	return os.WriteFile(filepath.Join(path, ".gitignore"), []byte("*\n"), 0600)
}

func createOutput(root, path string) (*os.File, error) {
	if !isOutputPath(path) {
		return nil, os.ErrPermission
	}
	dir, err := outputDir(root, true)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := syscall.Openat(int(dir.Fd()), strings.TrimPrefix(path, outputDirectory), syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func removeOutput(root, path string) {
	dir, err := outputDir(root, false)
	if err != nil {
		return
	}
	defer dir.Close()
	_ = syscall.Unlinkat(int(dir.Fd()), strings.TrimPrefix(path, outputDirectory))
}

// outputFile drains a stream into a bounded prefix on disk. Storage failures
// never stop draining a command or change its exit code.
type outputFile struct {
	limit        int
	total, saved int64
	root, path   string
	file         *os.File
	storageErr   error
}

func newOutputFile(request toolExecution, channel string) *outputFile {
	s := &outputFile{limit: max(0, request.MaxOutputBytes), root: request.ProjectRoot, path: outputDirectory + request.callID + "." + channel}
	if isOutputPath(s.path) {
		s.file, s.storageErr = createOutput(s.root, s.path)
	}
	return s
}

func (s *outputFile) Write(p []byte) (int, error) {
	if s.file != nil && s.storageErr == nil {
		s.writeFile(p)
	}
	s.total += int64(len(p))
	return len(p), nil
}

// Bash retains an independent real tail; Git reads its preview from the file.
type streamOutput struct {
	*outputFile
	tail   []byte
	inline int
}

func newStreamOutput(request toolExecution, channel string) *streamOutput {
	inline := request.inlineBytes
	if inline <= 0 {
		inline = request.MaxOutputBytes
	}
	return &streamOutput{outputFile: newOutputFile(request, channel), inline: max(0, min(inline, request.MaxOutputBytes))}
}

func (s *streamOutput) Write(p []byte) (int, error) {
	s.outputFile.Write(p)
	if len(p) >= s.inline {
		s.tail = append(s.tail[:0], p[len(p)-s.inline:]...)
	} else {
		drop := max(0, len(s.tail)+len(p)-s.inline)
		s.tail = append(s.tail[:copy(s.tail, s.tail[drop:])], p...)
	}
	return len(p), nil
}

func (s *streamOutput) finish() (path string, omitted int64, diagnostic string) {
	path, omitted, diagnostic = s.outputFile.finish()
	if path == "" {
		omitted = s.total - int64(len(s.tail))
	}
	return
}

func (s *outputFile) writeFile(p []byte) {
	n := min(int64(len(p)), int64(s.limit)-s.saved)
	if n <= 0 {
		return
	}
	count, err := s.file.Write(p[:n])
	s.saved += int64(count)
	if err != nil {
		s.storageErr = err
	}
}

func (s *outputFile) finish() (path string, omitted int64, diagnostic string) {
	if s.file != nil {
		if s.storageErr == nil {
			s.storageErr = s.file.Close()
			if s.storageErr == nil {
				info, err := os.Lstat(filepath.Join(s.root, s.path))
				s.storageErr = err
				if err == nil && !info.Mode().IsRegular() {
					s.storageErr = errors.New("output path no longer names a regular file")
				}
				if s.storageErr == nil {
					return s.path, s.total - s.saved, ""
				}
			}
			removeOutput(s.root, s.path)
		} else {
			_ = s.file.Close()
			removeOutput(s.root, s.path)
		}
	}
	if s.storageErr != nil {
		diagnostic = "output file unavailable: " + s.storageErr.Error()
	}
	return "", s.total, diagnostic
}
