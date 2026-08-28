// file: internal/adapters/blob/local.go
package blob

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Local stores blobs on disk under Root, sharded two levels deep by the key's first four
// characters. Content-addressed keys are hex, so this keeps any single directory to a few
// thousand entries — ext4 slows down noticeably past ~10k files per directory.
type Local struct{ Root string }

func NewLocal(root string) (*Local, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Local{Root: root}, nil
}

func (l *Local) path(key string) (string, error) {
	// Path traversal guard: a key from a request must never escape Root.
	clean := filepath.Clean("/" + strings.TrimPrefix(key, "/"))
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("blob: invalid key %q", key)
	}
	base := filepath.Base(clean)
	// Shard only content-addressed keys (long hex names). Short, human-readable keys
	// like "0.in" are left alone: sharding them produces silly nesting for no benefit,
	// and their directories never grow large enough for it to matter.
	shard := ""
	if len(base) >= 16 && isHex(base[:4]) {
		shard = base[:2] + "/" + base[2:4]
	}
	return filepath.Join(l.Root, filepath.Dir(clean), shard, base), nil
}

func (l *Local) Put(_ context.Context, key string, r io.Reader) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// Write to a temp file and rename: a reader never observes a half-written blob.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (l *Local) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// GetToFile streams straight to disk. Test inputs can be tens of megabytes; buffering them
// in the runner's heap is exactly the "memory leak" failure mode the brief warns about.
func (l *Local) GetToFile(ctx context.Context, key, dst string) error {
	src, err := l.Get(ctx, key)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, src)
	return err
}

func (l *Local) Exists(_ context.Context, key string) (bool, error) {
	p, err := l.path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func isHex(s string) bool {
	for _, c := range s {
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
