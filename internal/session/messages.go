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
	ID  string
	Err error
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
	SessionID string
	Command   string
	Cwd       string
	Sudo      bool
	resp      chan<- ExecResponse
	cancel    <-chan struct{} // Closed when handler times out
}

func (ExecRequest) isRequest() {}

type ExecResponse struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}
