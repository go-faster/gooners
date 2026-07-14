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

func runUpload(ctx context.Context, client *ssh.Client, job *TransferJob) {
	job.finish(ctx, upload(ctx, client, job))
}

func upload(ctx context.Context, client *ssh.Client, job *TransferJob) error {
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
	job.setTotal(stat.Size())

	sftpClient, err := sftp.NewClient(client, sftp.UseConcurrentWrites(true))
	if err != nil {
		return err
	}
	defer func() {
		_ = sftpClient.Close()
	}()
	if !job.setCloser(sftpClient) {
		return context.Cause(ctx)
	}

	dst, err := sftpClient.Create(job.RemotePath)
	if err != nil {
		return err
	}

	if _, err := io.Copy(dst, &progressReader{r: src, ctx: ctx, job: job}); err != nil {
		_ = dst.Close()
		_ = sftpClient.Remove(job.RemotePath)
		return err
	}
	if err := dst.Close(); err != nil {
		_ = sftpClient.Remove(job.RemotePath)
		return err
	}

	return nil
}

func runDownload(ctx context.Context, client *ssh.Client, job *TransferJob) {
	job.finish(ctx, download(ctx, client, job))
}

func download(ctx context.Context, client *ssh.Client, job *TransferJob) error {
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer func() {
		_ = sftpClient.Close()
	}()
	if !job.setCloser(sftpClient) {
		return context.Cause(ctx)
	}

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
	job.setTotal(stat.Size())

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

	if _, err := io.Copy(dst, &progressReader{r: src, ctx: ctx, job: job}); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, job.LocalPath)
}
