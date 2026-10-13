package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

// newTestStorage puts the upload root INSIDE a parent temp dir, so tests can
// plant a file next to it and prove nothing can reach it.
func newTestStorage(t *testing.T) (l *Local, root, parent string) {
	t.Helper()
	parent = t.TempDir()
	root = filepath.Join(parent, "uploads")
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	return l, root, parent
}

// filesUnder lists every regular file below dir.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// endlessReader never returns EOF: a client that lies about Content-Length.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// failingReader delivers some bytes, then breaks like a dropped connection.
type failingReader struct{ sent bool }

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		return copy(p, "partial data"), nil
	}
	return 0, errors.New("connection reset")
}

func TestSaveAndOpen(t *testing.T) {
	l, root, _ := newTestStorage(t)
	ctx := context.Background()
	user, doc := uuid.New(), uuid.New()
	content := []byte("%PDF-1.7 pretend this is a pdf")

	rel, size, err := l.Save(ctx, user, doc, "pdf", bytes.NewReader(content), 1024)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if want := user.String() + "/" + doc.String() + ".pdf"; rel != want {
		t.Errorf("rel path = %q, want %q", rel, want)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}

	onDisk, err := os.ReadFile(filepath.Join(root, user.String(), doc.String()+".pdf"))
	if err != nil || !bytes.Equal(onDisk, content) {
		t.Fatalf("file on disk wrong: %v", err)
	}

	f, err := l.Open(ctx, rel)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	if f.Size() != int64(len(content)) {
		t.Errorf("Size() = %d, want %d", f.Size(), len(content))
	}
	// ReadAt is what the DOCX and PDF parsers will use.
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, 1); err != nil || string(buf) != "PDF-" {
		t.Errorf("ReadAt = %q, %v", buf, err)
	}
	if all, _ := io.ReadAll(f); !bytes.Equal(all, content) {
		t.Errorf("Read returned %q", all)
	}

	// Exactly one file, and no temp file left behind.
	if files := filesUnder(t, root); len(files) != 1 {
		t.Errorf("files on disk = %v, want exactly one", files)
	}
}

func TestSave_SizeLimit(t *testing.T) {
	ctx := context.Background()

	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		l, _, _ := newTestStorage(t)
		_, size, err := l.Save(ctx, uuid.New(), uuid.New(), "txt", strings.NewReader("0123456789"), 10)
		if err != nil || size != 10 {
			t.Errorf("got (%d, %v), want (10, nil)", size, err)
		}
	})

	t.Run("one byte over is rejected and leaves nothing behind", func(t *testing.T) {
		l, root, _ := newTestStorage(t)
		_, _, err := l.Save(ctx, uuid.New(), uuid.New(), "txt", strings.NewReader("0123456789A"), 10)
		if !errors.Is(err, models.ErrFileTooLarge) {
			t.Errorf("err = %v, want ErrFileTooLarge", err)
		}
		if files := filesUnder(t, root); len(files) != 0 {
			t.Errorf("leftover files: %v", files)
		}
	})

	t.Run("an endless stream stops at the limit", func(t *testing.T) {
		l, root, _ := newTestStorage(t)
		done := make(chan error, 1)
		go func() {
			_, _, err := l.Save(ctx, uuid.New(), uuid.New(), "txt", endlessReader{}, 1024)
			done <- err
		}()

		select {
		case err := <-done:
			if !errors.Is(err, models.ErrFileTooLarge) {
				t.Errorf("err = %v, want ErrFileTooLarge", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Save did not stop reading an endless stream")
		}
		if files := filesUnder(t, root); len(files) != 0 {
			t.Errorf("leftover files: %v", files)
		}
	})
}

func TestSave_CleansUpAfterReadError(t *testing.T) {
	l, root, _ := newTestStorage(t)

	_, _, err := l.Save(context.Background(), uuid.New(), uuid.New(), "txt", &failingReader{}, 1024)
	if err == nil {
		t.Fatal("expected an error from the broken reader")
	}
	if files := filesUnder(t, root); len(files) != 0 {
		t.Errorf("partial upload left behind: %v", files)
	}
}

func TestSave_RejectsBadInput(t *testing.T) {
	l, root, _ := newTestStorage(t)
	ctx := context.Background()

	badExts := []string{"", "../x", "pdf/../../x", "PDF", "pdf.exe", "a b", "pdf\x00", strings.Repeat("a", 11), "päf"}
	for _, ext := range badExts {
		t.Run(fmt.Sprintf("ext %q", ext), func(t *testing.T) {
			if _, _, err := l.Save(ctx, uuid.New(), uuid.New(), ext, strings.NewReader("x"), 10); err == nil {
				t.Error("expected an error")
			}
		})
	}

	if _, _, err := l.Save(ctx, uuid.Nil, uuid.New(), "pdf", strings.NewReader("x"), 10); err == nil {
		t.Error("nil user id should be rejected")
	}
	if _, _, err := l.Save(ctx, uuid.New(), uuid.New(), "pdf", strings.NewReader("x"), 0); err == nil {
		t.Error("zero maxBytes should be rejected")
	}

	if files := filesUnder(t, root); len(files) != 0 {
		t.Errorf("rejected saves left files: %v", files)
	}
}

func TestResolve_BlocksEscapes(t *testing.T) {
	l, _, parent := newTestStorage(t)
	ctx := context.Background()

	// A file OUTSIDE the upload root that must stay untouchable.
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}

	bad := []string{"", ".", "..", "../secret.txt", "a/../../secret.txt", "a/../../../secret.txt", "/etc/passwd"}
	if runtime.GOOS == "windows" {
		bad = append(bad, `..\secret.txt`, `C:\Windows\win.ini`, `C:/Windows/win.ini`)
	}

	for _, rel := range bad {
		t.Run(fmt.Sprintf("%q", rel), func(t *testing.T) {
			f, err := l.Open(ctx, rel)
			if err == nil {
				f.Close()
				t.Error("Open accepted an escaping path")
			} else if !errors.Is(err, models.ErrInvalidPath) {
				t.Errorf("Open err = %v, want ErrInvalidPath", err)
			}
			if err := l.Delete(ctx, rel); !errors.Is(err, models.ErrInvalidPath) {
				t.Errorf("Delete err = %v, want ErrInvalidPath", err)
			}
		})
	}

	if b, err := os.ReadFile(secret); err != nil || string(b) != "do not touch" {
		t.Fatalf("a file outside the upload root was affected: %v", err)
	}
}

func TestOpen_MissingFile(t *testing.T) {
	l, _, _ := newTestStorage(t)

	rel := uuid.NewString() + "/" + uuid.NewString() + ".pdf"
	if _, err := l.Open(context.Background(), rel); !errors.Is(err, models.ErrFileNotFound) {
		t.Errorf("err = %v, want ErrFileNotFound", err)
	}
}

func TestDelete_IsIdempotent(t *testing.T) {
	l, root, _ := newTestStorage(t)
	ctx := context.Background()

	rel, _, err := l.Save(ctx, uuid.New(), uuid.New(), "txt", strings.NewReader("hello"), 100)
	if err != nil {
		t.Fatal(err)
	}

	if err := l.Delete(ctx, rel); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := l.Delete(ctx, rel); err != nil {
		t.Errorf("second delete should succeed, got %v", err)
	}
	if _, err := l.Open(ctx, rel); !errors.Is(err, models.ErrFileNotFound) {
		t.Errorf("Open after delete err = %v, want ErrFileNotFound", err)
	}
	if files := filesUnder(t, root); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
}

func TestDelete_RefusesDirectories(t *testing.T) {
	l, root, _ := newTestStorage(t)
	user := uuid.New()
	if err := os.MkdirAll(filepath.Join(root, user.String()), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := l.Delete(context.Background(), user.String()); !errors.Is(err, models.ErrInvalidPath) {
		t.Errorf("err = %v, want ErrInvalidPath", err)
	}
	if _, err := os.Stat(filepath.Join(root, user.String())); err != nil {
		t.Errorf("directory was removed: %v", err)
	}
}

func TestCleanTemp(t *testing.T) {
	l, root, _ := newTestStorage(t)

	dir := filepath.Join(root, uuid.NewString())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	oldTmp := filepath.Join(dir, ".upload-old.tmp")
	newTmp := filepath.Join(dir, ".upload-new.tmp")
	kept := filepath.Join(dir, "important.pdf")
	for _, p := range []string{oldTmp, newTmp, kept} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	past := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{oldTmp, kept} { // the real file is old too: must still survive
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	n, err := l.CleanTemp(time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("CleanTemp = (%d, %v), want (1, nil)", n, err)
	}
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Error("old temp file should have been removed")
	}
	for _, p := range []string{newTmp, kept} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should have been kept: %v", filepath.Base(p), err)
		}
	}
}

func TestNewLocal_CreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "b", "uploads")

	if _, err := NewLocal(root); err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("root was not created: %v", err)
	}
	if files := filesUnder(t, root); len(files) != 0 {
		t.Errorf("startup probe left files: %v", files)
	}
}