package session

import (
	"context"

	"golang.org/x/crypto/ssh"
)

// Request is the base interface for all session manager messages.
type Request interface {
	isRequest()
}

// OpenRequest is a request to open a new SSH session.
type OpenRequest struct {
	Config Config
	resp   chan<- OpenResponse
}

func (OpenRequest) isRequest() {}

// OpenResponse is the response to an [OpenRequest].
type OpenResponse struct {
	ID        string
	UserAgent string
	Banner    string
	Platform  string
	Err       error
}

// GetRequest is a request to get an existing SSH session.
type GetRequest struct {
	ID   string
	resp chan<- GetResponse
}

func (GetRequest) isRequest() {}

// GetResponse is the response to a [GetRequest].
type GetResponse struct {
	Client *ssh.Client
	Err    error
}

// CloseRequest is a request to close an existing SSH session.
type CloseRequest struct {
	ID   string
	resp chan<- error
}

func (CloseRequest) isRequest() {}

// ListRequest is a request to list all existing SSH sessions.
type ListRequest struct {
	resp chan<- []SessionInfo
}

func (ListRequest) isRequest() {}

// ExecRequest is a request to execute a command on an existing SSH session.
type ExecRequest struct {
	SessionID          string
	Command            string
	Description        string
	DescriptionComment bool
	Cwd                string
	Stdin              string
	Sudo               bool
	SudoPassword       string
	resp               chan<- ExecResponse
	cancel             <-chan struct{} // Closed when handler times out
}

func (ExecRequest) isRequest() {}

// ExecResponse is the response to an [ExecRequest].
type ExecResponse struct {
	Stdout        string
	Stderr        string
	StdoutSize    int64
	StderrSize    int64
	StdoutSpoolID string
	StderrSpoolID string
	ExitCode      int
	Err           error
}

// UploadRequest is a request to upload a file to an existing SSH session.
type UploadRequest struct {
	SessionID  string
	LocalPath  string
	RemotePath string
	resp       chan<- UploadResponse
}

func (UploadRequest) isRequest() {}

// UploadResponse is the response to an [UploadRequest].
type UploadResponse struct {
	UploadID string
	Err      error
}

// UploadStatusRequest is a request to get the status of an ongoing upload.
type UploadStatusRequest struct {
	SessionID string
	UploadID  string
	resp      chan<- UploadStatusResponse
}

func (UploadStatusRequest) isRequest() {}

// UploadStatusResponse is the response to an [UploadStatusRequest].
type UploadStatusResponse struct {
	UploadID        string
	BytesUploaded   int64
	TotalBytes      int64
	Percent         float64
	InstantSpeedBPS float64
	AverageSpeedBPS float64
	ETASeconds      float64
	Done            bool
	Err             error
}

// UploadWaitRequest is a request to wait for an ongoing upload to complete.
type UploadWaitRequest struct {
	Ctx       context.Context
	SessionID string
	UploadID  string
	resp      chan<- UploadStatusResponse
}

func (UploadWaitRequest) isRequest() {}

// UploadCancelRequest is a request to cancel an ongoing upload.
type UploadCancelRequest struct {
	Ctx       context.Context
	SessionID string
	UploadID  string
	resp      chan<- UploadStatusResponse
}

func (UploadCancelRequest) isRequest() {}

// DownloadRequest is a request to download a file from an existing SSH session.
type DownloadRequest struct {
	SessionID  string
	LocalPath  string
	RemotePath string
	resp       chan<- DownloadResponse
}

func (DownloadRequest) isRequest() {}

// DownloadResponse is the response to a [DownloadRequest].
type DownloadResponse struct {
	DownloadID string
	Err        error
}

// DownloadStatusRequest is a request to get the status of an ongoing download.
type DownloadStatusRequest struct {
	SessionID  string
	DownloadID string
	resp       chan<- DownloadStatusResponse
}

func (DownloadStatusRequest) isRequest() {}

// DownloadStatusResponse is the response to a [DownloadStatusRequest].
type DownloadStatusResponse struct {
	DownloadID      string
	BytesDownloaded int64
	TotalBytes      int64
	Percent         float64
	InstantSpeedBPS float64
	AverageSpeedBPS float64
	ETASeconds      float64
	Done            bool
	Err             error
}

// DownloadWaitRequest is a request to wait for an ongoing download to complete.
type DownloadWaitRequest struct {
	Ctx        context.Context
	SessionID  string
	DownloadID string
	resp       chan<- DownloadStatusResponse
}

func (DownloadWaitRequest) isRequest() {}

// DownloadCancelRequest is a request to cancel an ongoing download.
type DownloadCancelRequest struct {
	Ctx        context.Context
	SessionID  string
	DownloadID string
	resp       chan<- DownloadStatusResponse
}

func (DownloadCancelRequest) isRequest() {}

// RegisterSpoolRequest is a request to register a spool file for an existing SSH session.
type RegisterSpoolRequest struct {
	SessionID string
	SpoolID   string
	Path      string
	resp      chan<- error
}

func (RegisterSpoolRequest) isRequest() {}

// GetSpoolRequest is a request to get the path of a spool file for an existing SSH session.
type GetSpoolRequest struct {
	SessionID string
	SpoolID   string
	resp      chan<- GetSpoolResponse
}

func (GetSpoolRequest) isRequest() {}

// GetSpoolResponse is the response to a [GetSpoolRequest].
type GetSpoolResponse struct {
	Path string
	Err  error
}

// DeleteSpoolRequest is a request to delete a spool file for an existing SSH session.
type DeleteSpoolRequest struct {
	SessionID string
	SpoolID   string
	resp      chan<- error
}

func (DeleteSpoolRequest) isRequest() {}

// MachineRequest is a request to get the machine name of an existing SSH session.
type MachineRequest struct {
	ID   string
	resp chan<- MachineResponse
}

func (MachineRequest) isRequest() {}

// MachineResponse is the response to a [MachineRequest].
type MachineResponse struct {
	Machine string
	Err     error
}
