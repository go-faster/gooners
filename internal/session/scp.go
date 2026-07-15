package session

import (
	"context"
	"io"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/go-faster/gooners/internal/effect"
)

func (p *Pool) SFTP(ctx context.Context, id string) (*sftp.Client, error) {
	client, err := p.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return sftp.NewClient(client)
}

// runUpload streams job.LocalPath to the remote. localFS is what decides
// whether job.LocalPath may be read at all; the path arrives from a tool call
// unvalidated, by design.
func runUpload(ctx context.Context, client *ssh.Client, localFS effect.FS, job *TransferJob) {
	job.finish(ctx, upload(ctx, client, localFS, job))
}

func upload(ctx context.Context, client *ssh.Client, localFS effect.FS, job *TransferJob) error {
	src, err := localFS.Open(job.LocalPath)
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

	sftpClient, err := sftp.NewClient(client, sftp.MaxPacket(uploadChunkSize))
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

	if err := copyToRemote(ctx, dst, src, job); err != nil {
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

// runDownload streams the remote file into job.LocalPath. As in [runUpload],
// localFS is the gate: the destination is whatever the agent asked for.
func runDownload(ctx context.Context, client *ssh.Client, localFS effect.FS, job *TransferJob) {
	job.finish(ctx, download(ctx, client, localFS, job))
}

func download(ctx context.Context, client *ssh.Client, localFS effect.FS, job *TransferJob) error {
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
	dst, err := localFS.Create(tmpPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = dst.Close()
		_ = localFS.Remove(tmpPath) // cleans up partial file if not renamed
	}()

	// Copy from the remote file directly, so io.Copy takes its WriteTo path and reads
	// concurrently. Wrapping src in a reader instead would hide WriteTo and serialize the
	// whole download.
	if _, err := io.Copy(&progressWriter{w: dst, ctx: ctx, job: job}, src); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}

	return localFS.Rename(tmpPath, job.LocalPath)
}
