package player

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// startWrite creates an oggWrite whose temporary file is fed by the returned
// function, bypassing the Spotify download.
func startWrite(t *testing.T, srcSize int64) (*oggWrite, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "track.ogg")
	w := newOggWrite(path)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".track.ogg.*.part")
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.tmp, w.srcSize, w.headerKnown = tmp, srcSize, true
	w.cond.Broadcast()
	w.mu.Unlock()
	return w, path
}

func TestGrowingOggReadsWhileWriting(t *testing.T) {
	data := make([]byte, 300_000)
	rand.New(rand.NewSource(1)).Read(data)
	w, path := startWrite(t, int64(len(data)))

	r, err := w.newReader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	go func() {
		for off := 0; off < len(data); off += 7000 {
			end := min(off+7000, len(data))
			if _, err := (oggWriteCounter{w}).Write(data[off:end]); err != nil {
				t.Error(err)
			}
			time.Sleep(time.Millisecond)
		}
		_ = w.tmp.Close()
		w.mu.Lock()
		_ = os.Rename(w.tmp.Name(), path)
		w.mu.Unlock()
		w.finish(nil)
	}()

	// Sequential reads (as the decoder does) block until data arrives.
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, contents differ", len(got))
	}
	if r.Size() != int64(len(data)) {
		t.Fatalf("size %d after finish, want %d", r.Size(), len(data))
	}

	// A reader opened after the rename gets the final file.
	r2, err := w.newReader()
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, ok := r2.(*oggFile); !ok {
		t.Fatalf("reader after finish is %T, want *oggFile", r2)
	}
}

func TestGrowingOggWriteFailure(t *testing.T) {
	w, _ := startWrite(t, 100_000)
	r, err := w.newReader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	_, _ = (oggWriteCounter{w}).Write(make([]byte, 1000))
	boom := errors.New("network gone")
	go func() {
		time.Sleep(10 * time.Millisecond)
		w.finish(boom)
	}()

	buf := make([]byte, 5000)
	if _, err := r.ReadAt(buf, 0); !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	if _, err := w.newReader(); !errors.Is(err, boom) {
		t.Fatalf("newReader after failure: got %v, want %v", err, boom)
	}
}
