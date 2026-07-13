package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/go-faster/gooners/internal/effect"
)

// SpoolingBuffer is a buffer that spools to disk when it exceeds a certain threshold.
type SpoolingBuffer struct {
	// mu protects the buffer and its state.
	mu sync.Mutex
	// fs is where the spool file is written. All paths below are relative to it.
	fs effect.FS
	// sessionID is the ID of the session this buffer belongs to.
	sessionID string
	// spoolID is the unique ID of the spool file.
	spoolID string
	// threshold is the size in bytes at which the buffer will spool to disk.
	threshold int64
	// buf is the in-memory buffer.
	buf bytes.Buffer
	// file is the file to which the buffer has been spilled, if any.
	file effect.File
	// filePath is the spool file's path within fs, if any.
	filePath string
	// size is the current size of the buffer, in bytes.
	size int64
	// spilled indicates whether the buffer has been spilled to disk.
	spilled bool
	// err is the error that occurred during writing, if any.
	err error
}

// NewSpoolingBuffer creates a new SpoolingBuffer that spills into fsys once it
// exceeds threshold bytes.
func NewSpoolingBuffer(fsys effect.FS, sessionID string, threshold int64) *SpoolingBuffer {
	return &SpoolingBuffer{
		fs:        fsys,
		sessionID: sessionID,
		spoolID:   generateSpoolID(),
		threshold: threshold,
	}
}

func generateSpoolID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Write implements the [io.Writer] interface.
func (b *SpoolingBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.err != nil {
		return 0, b.err
	}

	n = len(p)
	b.size += int64(n)

	if b.spilled {
		if b.file == nil {
			b.err = fmt.Errorf("spooling buffer closed")
			return 0, b.err
		}
		var nw int
		nw, err = b.file.Write(p)
		if err != nil {
			b.err = err
			return nw, err
		}
		return n, nil
	}

	if b.size > b.threshold {
		if err := b.fs.MkdirAll(b.sessionID, 0o700); err != nil {
			b.err = fmt.Errorf("creating session spool directory: %w", err)
			return 0, b.err
		}

		path := filepath.Join(b.sessionID, b.spoolID+".out")
		f, err := b.fs.Create(path)
		if err != nil {
			b.err = fmt.Errorf("creating spool file: %w", err)
			return 0, b.err
		}
		b.file = f
		b.filePath = path
		b.spilled = true

		if b.buf.Len() > 0 {
			if _, err := b.file.Write(b.buf.Bytes()); err != nil {
				_ = b.file.Close()
				b.file = nil
				b.err = fmt.Errorf("writing buffer to spool file: %w", err)
				return 0, b.err
			}
		}

		var nw int
		nw, err = b.file.Write(p)
		if err != nil {
			_ = b.file.Close()
			b.file = nil
			b.err = fmt.Errorf("writing to spool file: %w", err)
			return nw, err
		}
		return n, nil
	}

	_, err = b.buf.Write(p)
	if err != nil {
		b.err = err
		return 0, err
	}
	return n, nil
}

// Close closes the buffer and the underlying file if it has been spilled to disk.
func (b *SpoolingBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.file != nil {
		err := b.file.Close()
		b.file = nil
		return err
	}
	return nil
}

// String returns the contents of the buffer as a string. If the buffer has been spilled to disk, it returns an empty string.
func (b *SpoolingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Spilled returns true if the buffer has been spilled to disk.
func (b *SpoolingBuffer) Spilled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spilled
}

// SpoolID returns the unique ID of the spool file.
func (b *SpoolingBuffer) SpoolID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spoolID
}

// FilePath returns the spool file's path within the buffer's [effect.FS], if
// any. It is not a host path: only the pool's spool FS can resolve it.
func (b *SpoolingBuffer) FilePath() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.filePath
}

// Size returns the current size of the buffer, in bytes.
func (b *SpoolingBuffer) Size() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}

// MapSpoolID returns the spool ID of the buffer if it has been spilled to disk, or an empty string otherwise.
func mapSpoolID(b *SpoolingBuffer) string {
	if b.Spilled() {
		return b.SpoolID()
	}
	return ""
}
