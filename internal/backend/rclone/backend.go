package rclone

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/restic/restic/internal/backend/rest"
	"github.com/restic/restic/internal/backend/sftp"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"golang.org/x/net/http2"
)

// Backend is used to access data stored somewhere via rclone.
type Backend struct {
	*rest.Backend
	tr  *http2.Transport
	cmd *exec.Cmd
}

// run starts command with args and initializes the StdioConn.
func run(command string, args ...string) (*StdioConn, *exec.Cmd, func() error, error) {
	cmd := exec.Command(command, args...)
	cmd.Stderr = os.Stderr

	r, stdin, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}

	stdout, w, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}

	cmd.Stdin = r
	cmd.Stdout = w

	bg, err := startForeground(cmd)
	if err != nil {
		return nil, nil, nil, err
	}

	c := &StdioConn{
		stdin:  stdout,
		stdout: stdin,
		cmd:    cmd,
	}

	return c, cmd, bg, nil
}

// New initializes a Backend and starts the process.
func New(cfg Config) (*Backend, error) {
	arg0 := "rclone"
	args := []string{"serve", "restic", "--stdin", cfg.Remote}

	var err error

	if cfg.Command != "" {
		arg0, args, err = sftp.SplitShellArgs(cfg.Command)
		if err != nil {
			return nil, err
		}
	}

	conn, cmd, bg, err := run(arg0, args...)
	if err != nil {
		return nil, err
	}

	tr := &http2.Transport{
		AllowHTTP: true, // this is not really HTTP, just stdin/stdout
		DialTLS: func(network, address string, cfg *tls.Config) (net.Conn, error) {
			debug.Log("new connection requested, %v %v", network, address)
			return conn, nil
		},
	}

	// send HEAD request to the base URL, see if the repo is there
	client := &http.Client{
		Transport: tr,
		Timeout:   60 * time.Second,
	}

	res, err := client.Get("http://localhost/")
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		bg()
		_ = cmd.Process.Kill()
		return nil, errors.Errorf("invalid HTTP response from rclone: %v", res.Status)
	}

	bg()

	url, err := url.Parse("http://localhost/")
	if err != nil {
		return nil, err
	}
	restConfig := rest.Config{
		Connections: 20,
		URL:         url,
	}
	restBackend, err := rest.Open(restConfig, tr)

	if err != nil {
		return nil, err
	}

	be := &Backend{
		Backend: restBackend,
		tr:      tr,
		cmd:     cmd,
	}

	return be, nil
}

// Open starts an rclone process with the given config.
func Open(cfg Config) (*Backend, error) {
	return New(cfg)
}

// Close terminates the backend.
func (be *Backend) Close() error {
	be.tr.CloseIdleConnections()
	return be.cmd.Wait()
}
