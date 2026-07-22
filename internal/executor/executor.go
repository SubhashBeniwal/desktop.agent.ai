// Package executor runs subprocesses and streams their output line-by-line to
// a callback so it can be forwarded to the control plane as it is produced.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
)

// maxLine is the largest single output line we will buffer (1 MiB). Longer
// lines are split.
const maxLine = 1 << 20

// LogFunc receives one line of output. stream is "stdout" or "stderr".
type LogFunc func(stream, data string)

// Run starts cmd, streams stdout and stderr through log, and waits for it to
// finish. It returns the process exit code and an error only for failures that
// are not a non-zero exit (e.g. the binary could not be started). A non-zero
// exit is reported via the returned code with a nil error.
//
// cmd should be created with exec.CommandContext so that context cancellation
// terminates the process.
func Run(ctx context.Context, cmd *exec.Cmd, log LogFunc) (int, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}

	if err := cmd.Start(); err != nil {
		return -1, err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go stream(stdout, "stdout", log, &wg)
	go stream(stderr, "stderr", log, &wg)
	wg.Wait()

	err = cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Non-zero exit is a normal outcome we report via the code.
		return exitErr.ExitCode(), nil
	}
	// Context cancellation surfaces here too; report it to the caller.
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	return -1, err
}

func stream(r io.Reader, name string, log LogFunc, wg *sync.WaitGroup) {
	defer wg.Done()
	if log == nil {
		_, _ = io.Copy(io.Discard, r)
		return
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		log(name, sc.Text())
	}
}

// Capture runs cmd to completion and returns its combined output. Use it for
// short commands whose full output is needed (e.g. `gh pr diff`).
func Capture(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if ctx.Err() != nil {
		return buf.Bytes(), ctx.Err()
	}
	return buf.Bytes(), err
}