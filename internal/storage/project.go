package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"magi/internal/core/protocol"
)

type Metadata struct {
	ProjectID     string `json:"project_id"`
	FormatVersion int    `json:"format_version"`
}

// Project owns workspace identity and the durable store, not engine workflow state.
type Project struct {
	root     string
	metadata Metadata
	disk     *Store
}

// InitProject atomically initializes missing project state or opens a complete existing project.
func InitProject(ctx context.Context, root string) (*Project, error) {
	return openProject(ctx, root, true)
}

// OpenProject requires an initialized project and never repairs missing state.
func OpenProject(ctx context.Context, root string) (*Project, error) {
	return openProject(ctx, root, false)
}

func openProject(ctx context.Context, root string, initialize bool) (*Project, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("project root is not a directory")
	}
	final := filepath.Join(abs, ".magi")
	dir := final
	info, err = os.Lstat(final)
	create := os.IsNotExist(err)
	if create && !initialize {
		return nil, protocol.LifecycleConflict("run magi init first")
	}
	if err != nil && !create {
		return nil, err
	}
	if !create && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return nil, protocol.IntegrityFailure("state path is not a physical directory")
	}
	published := false
	if create {
		dir, err = os.MkdirTemp(abs, ".magi-preinit-")
		if err != nil {
			return nil, err
		}
		defer func() {
			if !published {
				_ = os.RemoveAll(dir)
			}
		}()
		stage := filepath.Join(abs, ".magi-init-"+strings.TrimPrefix(filepath.Base(dir), ".magi-preinit-"))
		if err = writeInitializationMarker(dir, stage); err != nil {
			return nil, err
		}
		if err = os.Rename(dir, stage); err != nil {
			return nil, err
		}
		dir = stage
	}
	metadata, err := openMetadata(filepath.Join(dir, "project.json"), create)
	if err != nil {
		return nil, err
	}
	if create {
		for _, name := range []string{"safe-points", "history"} {
			if err = os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
				return nil, err
			}
		}
	}
	store := &Project{root: abs, metadata: metadata}
	if create {
		store.disk, err = initStore(ctx, dir)
	} else {
		store.disk, err = openStore(ctx, dir)
	}
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Project, error) { _ = store.disk.Close(); return nil, err }
	if create {
		if err = store.disk.Close(); err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if _, e := os.Lstat(final); !os.IsNotExist(e) {
			return nil, protocol.LifecycleConflict("state appeared during init")
		}
		if err = os.Rename(dir, final); err != nil {
			return nil, err
		}
		published = true
		if err = syncDir(abs); err != nil {
			return nil, err
		}
		return OpenProject(ctx, abs)
	}
	for _, name := range []string{"safe-points", "history"} {
		info, e := os.Lstat(filepath.Join(dir, name))
		if e != nil || !info.IsDir() {
			return fail(protocol.IntegrityFailure("required private directory missing: " + name))
		}
	}
	if err = quarantineAbandonedInitializations(abs, dir); err != nil {
		return fail(err)
	}
	return store, nil
}

func (s *Project) Root() string       { return s.root }
func (s *Project) Metadata() Metadata { return s.metadata }
func (s *Project) Disk() *Store       { return s.disk }
func (s *Project) Close() error       { return s.disk.Close() }

func openMetadata(path string, create bool) (Metadata, error) {
	if !create {
		info, err := os.Lstat(path)
		if err != nil {
			return Metadata{}, err
		}
		if err := validatePrivateEntry(path, info); err != nil {
			return Metadata{}, err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return Metadata{}, err
		}
		var m Metadata
		if protocol.UnmarshalJSON(body, &m) != nil || m.FormatVersion != protocol.StorageFormatVersion || len(m.ProjectID) != 32 {
			return m, protocol.IntegrityFailure("unsupported project metadata")
		}
		raw, err := hex.DecodeString(m.ProjectID)
		if err != nil || hex.EncodeToString(raw) != m.ProjectID {
			return m, protocol.IntegrityFailure("invalid project identity")
		}
		return m, nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Metadata{}, err
	}
	m := Metadata{ProjectID: hex.EncodeToString(id[:]), FormatVersion: protocol.StorageFormatVersion}
	body, _ := json.Marshal(m)
	// The containing staging directory is published atomically by openProject.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return m, err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err != nil || closeErr != nil {
		return m, errors.Join(err, closeErr)
	}
	return m, syncDir(filepath.Dir(path))
}

// Move only staging directories with a matching marker into the
// excluded private state. Keep their bytes for diagnosis instead of deleting
// project-root content whose ownership cannot be established.
func quarantineAbandonedInitializations(root, stateDir string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".magi-init-") || !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		owned, markerErr := IsPrivateInitializationDir(path)
		if markerErr != nil {
			return markerErr
		}
		if !owned {
			continue
		}
		quarantine := filepath.Join(stateDir, "abandoned-initializations")
		if info, err := os.Lstat(quarantine); err == nil {
			if !info.IsDir() {
				return protocol.IntegrityFailure("initialization quarantine is not a physical directory")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err = os.MkdirAll(quarantine, 0700); err != nil {
			return err
		}
		destination := filepath.Join(quarantine, entry.Name())
		if _, statErr := os.Lstat(destination); !os.IsNotExist(statErr) {
			if statErr == nil {
				return protocol.IntegrityFailure("duplicate abandoned initialization")
			}
			return statErr
		}
		if err = os.Rename(path, destination); err != nil {
			return err
		}
	}
	return nil
}

// ValidateStateDirectory checks that the held project still owns its state path.
// Individual storage operations validate the files they access.
func (s *Project) ValidateStateDirectory() error {
	return s.disk.validate(databaseName)
}

// Lock holds exclusive access to private project state. The caller controls
// its lifetime across dialogue and execution. Process exit releases it without
// treating an interrupted session as a cancelled workflow.
func Lock(root string) (func(), error) {
	// Hold the physical state directory before creating anything inside it.
	// O_NOFOLLOW on the lock alone would still follow a .magi directory link.
	directory, err := os.OpenFile(filepath.Join(root, ".magi"), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	fd, err := syscall.Openat(int(directory.Fd()), "instance.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "instance.lock")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, protocol.IntegrityFailure("instance lock is not a regular file")
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, protocol.LifecycleConflict("another Magi instance is active in this directory")
		}
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
