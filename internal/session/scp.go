package session

import (
	"context"
	"io"
	"os"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func (p *Pool) SFTP(ctx context.Context, id string) (*sftp.Client, error) {
	client, err := p.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return sftp.NewClient(client)
}

type poolProgressReader struct {
	r   io.Reader
	ctx context.Context
	job *UploadJob
}

func (pr *poolProgressReader) Read(p []byte) (int, error) {
	if err := pr.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := pr.r.Read(p)
	pr.job.mu.Lock()
	pr.job.BytesUploaded += int64(n)
	pr.job.mu.Unlock()
	return n, err
}

func runUpload(ctx context.Context, client *ssh.Client, job *UploadJob) {
	defer func() {
		job.mu.Lock()
		job.Done = true
		job.mu.Unlock()
		close(job.done)
	}()

	if err := func() error {
		src, err := os.Open(job.LocalPath)
		if err != nil {
			return err
		}
		defer func() {
			_ = src.Close()
		}()

		stat, err := src.Stat()
		if err != nil {
			return err
		}

		job.mu.Lock()
		job.TotalBytes = stat.Size()
		job.mu.Unlock()

		sftpClient, err := sftp.NewClient(client, sftp.UseConcurrentWrites(true))
		if err != nil {
			return err
		}
		defer func() {
			_ = sftpClient.Close()
		}()

		dst, err := sftpClient.Create(job.RemotePath)
		if err != nil {
			return err
		}

		pr := &poolProgressReader{r: src, ctx: ctx, job: job}
		if _, err := io.Copy(dst, pr); err != nil {
			_ = dst.Close()
			_ = sftpClient.Remove(job.RemotePath)
			return err
		}
		if err := dst.Close(); err != nil {
			_ = sftpClient.Remove(job.RemotePath)
			return err
		}

		return nil
	}(); err != nil {
		job.mu.Lock()
		job.Err = err
		job.mu.Unlock()
		return
	}
}

type poolDownloadProgressReader struct {
	r   io.Reader
	ctx context.Context
	job *DownloadJob
}

func (pr *poolDownloadProgressReader) Read(p []byte) (int, error) {
	if err := pr.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := pr.r.Read(p)
	pr.job.mu.Lock()
	pr.job.BytesDownloaded += int64(n)
	pr.job.mu.Unlock()
	return n, err
}

func runDownload(ctx context.Context, client *ssh.Client, job *DownloadJob) {
	defer func() {
		job.mu.Lock()
		job.Done = true
		job.mu.Unlock()
		close(job.done)
	}()

	if err := func() error {
		sftpClient, err := sftp.NewClient(client)
		if err != nil {
			return err
		}
		defer func() {
			_ = sftpClient.Close()
		}()

		src, err := sftpClient.Open(job.RemotePath)
		if err != nil {
			return err
		}
		defer func() {
			_ = src.Close()
		}()

		stat, err := src.Stat()
		if err != nil {
			return err
		}

		job.mu.Lock()
		job.TotalBytes = stat.Size()
		job.mu.Unlock()

		tmpPath := job.LocalPath + ".tmp"
		//nolint:gosec // LocalPath is validated to be within the allowed directory by withinDir
		dst, err := os.Create(tmpPath)
		if err != nil {
			return err
		}
		defer func() {
			_ = dst.Close()
			_ = os.Remove(tmpPath) // cleans up partial file if not renamed
		}()

		pr := &poolDownloadProgressReader{r: src, ctx: ctx, job: job}
		if _, err := io.Copy(dst, pr); err != nil {
			return err
		}

		if err := dst.Close(); err != nil {
			return err
		}

		if err := os.Rename(tmpPath, job.LocalPath); err != nil {
			return err
		}

		return nil
	}(); err != nil {
		job.mu.Lock()
		job.Err = err
		job.mu.Unlock()
		return
	}
}
