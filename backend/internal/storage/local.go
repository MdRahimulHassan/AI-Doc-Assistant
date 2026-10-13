package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

const (
	tempPrefix = ".upload-"
	tempSuffix = ".tmp"
)

// Compile-time proof that Local satisfies the service's interface.
var _ service.FileStorage = (*Local)(nil)

// Local stores files on disk under a single root directory.
type Local struct {
	root string // absolute path
}

// NewLocal creates the root directory if needed and checks that it is
// writable, so a bad UPLOAD_DIR fails at startup instead of on the first upload.
func NewLocal(root string) (*Local, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve upload dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("create upload dir %s: %w", abs, err)
	}

	probe, err := os.CreateTemp(abs, ".write-test-*")
	if err != nil {
		return nil, fmt.Errorf("upload dir %s is not writable: %w", abs, err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())

	return &Local{root: abs}, nil
}

// Save writes the upload to a temp file, then renames it into place, so a
// reader can never observe a half-written file.
func (l *Local) Save(ctx context.Context, userID, docID uuid.UUID, ext string, r io.Reader, maxBytes int64) (string, int64, error) {
	if maxBytes <= 0 {
		return "", 0, errors.New("save file: maxBytes must be positive")
	}
	// Plain file I/O can't be cancelled midway, but don't start if already cancelled.
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}

	rel, err := relPath(userID, docID, ext)
	if err != nil {
		return "", 0, err
	}
	final, err := l.resolve(rel)
	if err != nil {
		return "", 0, err
	}

	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", 0, fmt.Errorf("create user dir: %w", err)
	}

	// The temp file lives in the SAME directory as the destination:
	// a rename is only atomic within one filesystem.
	tmp, err := os.CreateTemp(dir, tempPrefix+"*"+tempSuffix)
	if err != nil {
		return "", 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	committed := false
	defer func() {
		if !committed { // any early return: leave nothing behind
			_ = tmp.Close() // close first: Windows can't delete an open file
			_ = os.Remove(tmpName)
		}
	}()

	// Never trust Content-Length. Copy at most maxBytes+1: the single extra byte
	// exists only to tell "exactly at the limit" (fine) from "over it" (reject),
	// and it stops an endless stream from filling the disk.
	n, err := io.Copy(tmp, io.LimitReader(r, maxBytes+1))
	if err != nil {
		return "", 0, fmt.Errorf("write upload: %w", err)
	}
	if n > maxBytes {
		return "", 0, models.ErrFileTooLarge
	}

	// Flush to disk BEFORE the rename, so a crash can't leave a renamed but empty file.
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("sync upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("close upload: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", 0, fmt.Errorf("move upload into place: %w", err)
	}

	committed = true
	return rel, n, nil
}

func (l *Local) Open(_ context.Context, rel string) (service.StoredFile, error) {
	full, err := l.resolve(rel)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, models.ErrFileNotFound
		}
		return nil, fmt.Errorf("open file: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat file: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, models.ErrInvalidPath
	}
	return &localFile{File: f, size: info.Size()}, nil
}

func (l *Local) Delete(_ context.Context, rel string) error {
	full, err := l.resolve(rel)
	if err != nil {
		return err
	}

	info, err := os.Lstat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // already gone
		}
		return fmt.Errorf("stat file: %w", err)
	}
	// os.Remove also deletes empty directories, so refuse anything that isn't a file.
	if !info.Mode().IsRegular() {
		return models.ErrInvalidPath
	}

	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete file: %w", err)
	}
	// Deliberately NOT removing the now-empty user directory: a concurrent Save
	// may have just created it, and deleting it would make that upload fail.
	return nil
}

// CleanTemp removes temp files older than maxAge, left behind if the process
// crashed mid-upload. Call it once at startup, before serving requests.
func (l *Local) CleanTemp(maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	removed := 0

	err := filepath.WalkDir(l.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // skip unreadable entries and folders; keep walking
		}
		name := d.Name()
		if !strings.HasPrefix(name, tempPrefix) || !strings.HasSuffix(name, tempSuffix) {
			return nil // never touch anything that isn't one of our temp files
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil
		}
		if os.Remove(p) == nil {
			removed++
		}
		return nil
	})
	return removed, err
}

// ---------------------------------------------------------------------------

// relPath is the ONE place the naming scheme lives: <user_id>/<doc_id>.<ext>.
// UUID strings contain only hex digits and dashes, so they can't carry path
// separators. The result always uses "/", whatever the operating system.
func relPath(userID, docID uuid.UUID, ext string) (string, error) {
	if userID == uuid.Nil || docID == uuid.Nil {
		return "", errors.New("save file: user id and document id are required")
	}
	if !validExt(ext) {
		return "", fmt.Errorf("save file: invalid extension %q", ext)
	}
	return path.Join(userID.String(), docID.String()+"."+ext), nil
}

// validExt is a safety net, not the real validation. The handler maps the
// SNIFFED content type to an extension; this only guarantees that whatever
// arrives is short, lowercase, and free of dots and separators.
func validExt(ext string) bool {
	if len(ext) == 0 || len(ext) > 10 {
		return false
	}
	for _, c := range ext {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// resolve turns a stored relative path into an absolute one, and refuses
// anything that could leave the upload root. Two independent checks:
//  1. filepath.IsLocal: lexically relative, no "..", no drive or root, no
//     reserved Windows names.
//  2. After joining, the result must still be strictly inside root.
func (l *Local) resolve(rel string) (string, error) {
	local := filepath.FromSlash(rel)
	if rel == "" || strings.ContainsRune(rel, 0) || !filepath.IsLocal(local) {
		return "", models.ErrInvalidPath
	}

	full := filepath.Join(l.root, local)

	back, err := filepath.Rel(l.root, full)
	if err != nil || back == "." || back == ".." ||
		strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", models.ErrInvalidPath
	}
	return full, nil
}

// localFile adds Size() to *os.File. The embedded File provides
// Read, ReadAt and Close.
type localFile struct {
	*os.File
	size int64
}

func (f *localFile) Size() int64 { return f.size }