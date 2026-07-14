package session

import (
	"context"
	"errors"
	"io"

	"golang.org/x/sync/errgroup"
)

const (
	// uploadChunkSize is the payload of a single SFTP write request, so one chunk is one
	// request and one acknowledgement. It matches the client's max packet, set alongside it.
	uploadChunkSize = 32 << 10
	// uploadConcurrency is how many write requests may be in flight at once. Overlapping
	// them is what keeps throughput up on a high-latency link.
	uploadConcurrency = 64
)

// copyToRemote streams src into dst, counting only the bytes the server has acknowledged.
//
// io.Copy would hand off to (*sftp.File).ReadFrom, which reads ahead of the wire by the
// whole in-flight window. Progress then tracks what was read from disk rather than what
// landed on the remote — on a file smaller than the window it reports 100% before a
// single byte is acknowledged. Issuing the writes here keeps the same pipelining but ties
// every increment to a request the server answered.
func copyToRemote(ctx context.Context, dst io.WriterAt, src io.Reader, job *TransferJob) error {
	type chunk struct {
		buf []byte
		off int64
	}

	g, ctx := errgroup.WithContext(ctx)

	work := make(chan chunk)
	// free hands buffers back to the reader. It holds every buffer in play, so returning
	// one never blocks.
	free := make(chan []byte, uploadConcurrency)
	for range uploadConcurrency {
		free <- make([]byte, uploadChunkSize)
	}

	g.Go(func() error {
		defer close(work)

		var off int64
		for {
			var buf []byte
			select {
			case buf = <-free:
			case <-ctx.Done():
				return context.Cause(ctx)
			}

			n, err := io.ReadFull(src, buf)
			if n > 0 {
				select {
				case work <- chunk{buf: buf[:n], off: off}:
					off += int64(n)
				case <-ctx.Done():
					return context.Cause(ctx)
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return nil
				}
				return err
			}
		}
	})

	for range uploadConcurrency {
		g.Go(func() error {
			for c := range work {
				if _, err := dst.WriteAt(c.buf, c.off); err != nil {
					return err
				}
				job.add(int64(len(c.buf)))
				free <- c.buf[:cap(c.buf)]
			}
			return nil
		})
	}

	return g.Wait()
}

// progressWriter counts bytes on their way to the destination and stops the transfer when
// the job's context is canceled.
//
// For a download the destination is the local file, so a byte counted here is a byte the
// remote already sent — the count cannot run ahead of the transfer.
type progressWriter struct {
	w   io.Writer
	ctx context.Context
	job *TransferJob
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	if pw.ctx.Err() != nil {
		return 0, context.Cause(pw.ctx)
	}
	n, err := pw.w.Write(p)
	pw.job.add(int64(n))
	return n, err
}
