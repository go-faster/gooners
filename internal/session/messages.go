package session

import "golang.org/x/crypto/ssh"

// Request is the base interface for all session manager messages.
type Request interface {
	isRequest()
}

type OpenRequest struct {
	Config Config
	resp   chan<- OpenResponse
}

func (OpenRequest) isRequest() {}

type OpenResponse struct {
	ID        string
	UserAgent string
	Banner    string
	Platform  string
	Err       error
}

type GetRequest struct {
	ID   string
	resp chan<- GetResponse
}

func (GetRequest) isRequest() {}

type GetResponse struct {
	Client *ssh.Client
	Err    error
}

type CloseRequest struct {
	ID   string
	resp chan<- error
}

func (CloseRequest) isRequest() {}

type ListRequest struct {
	resp chan<- []SessionInfo
}

func (ListRequest) isRequest() {}

type ExecRequest struct {
	SessionID    string
	Command      string
	Description  string
	Cwd          string
	Sudo         bool
	SudoPassword string
	resp         chan<- ExecResponse
	cancel       <-chan struct{} // Closed when handler times out
}

func (ExecRequest) isRequest() {}

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

type UploadRequest struct {
	SessionID  string
	LocalPath  string
	RemotePath string
	resp       chan<- UploadResponse
}

func (UploadRequest) isRequest() {}

type UploadResponse struct {
	UploadID string
	Err      error
}

type UploadStatusRequest struct {
	SessionID string
	UploadID  string
	resp      chan<- UploadStatusResponse
}

func (UploadStatusRequest) isRequest() {}

type UploadStatusResponse struct {
	UploadID      string
	BytesUploaded int64
	TotalBytes    int64
	Percent       float64
	Done          bool
	Err           error
}

type DownloadRequest struct {
	SessionID  string
	LocalPath  string
	RemotePath string
	resp       chan<- DownloadResponse
}

func (DownloadRequest) isRequest() {}

type DownloadResponse struct {
	DownloadID string
	Err        error
}

type DownloadStatusRequest struct {
	SessionID  string
	DownloadID string
	resp       chan<- DownloadStatusResponse
}

func (DownloadStatusRequest) isRequest() {}

type DownloadStatusResponse struct {
	DownloadID      string
	BytesDownloaded int64
	TotalBytes      int64
	Percent         float64
	Done            bool
	Err             error
}

type RegisterSpoolRequest struct {
	SessionID string
	SpoolID   string
	Path      string
	resp      chan<- error
}

func (RegisterSpoolRequest) isRequest() {}

type GetSpoolRequest struct {
	SessionID string
	SpoolID   string
	resp      chan<- GetSpoolResponse
}

func (GetSpoolRequest) isRequest() {}

type GetSpoolResponse struct {
	Path string
	Err  error
}

type DeleteSpoolRequest struct {
	SessionID string
	SpoolID   string
	resp      chan<- error
}

func (DeleteSpoolRequest) isRequest() {}

type MachineRequest struct {
	ID   string
	resp chan<- MachineResponse
}

func (MachineRequest) isRequest() {}

type MachineResponse struct {
	Machine string
	Err     error
}
