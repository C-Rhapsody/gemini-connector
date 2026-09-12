package main

import (
	"io"
	"os/exec"
)

// AgyCmdRunner is a function type for running an *exec.Cmd.
// It allows instance-local test injection without mutating global variables.
type AgyCmdRunner func(cmd *exec.Cmd) error

// DefaultCmdRunner runs the command via cmd.Run().
func DefaultCmdRunner(cmd *exec.Cmd) error {
	return cmd.Run()
}

// BoundedWriter is an io.Writer that limits total written bytes.
// If the limit is exceeded, Write returns an error and sets Exceeded = true.
type BoundedWriter struct {
	W        io.Writer
	MaxBytes int64
	Written  int64
	Exceeded bool
}

// NewBoundedWriter creates a BoundedWriter wrapping w with maxBytes limit.
// A limit <= 0 means no limit.
func NewBoundedWriter(w io.Writer, maxBytes int64) *BoundedWriter {
	return &BoundedWriter{
		W:        w,
		MaxBytes: maxBytes,
	}
}

func (bw *BoundedWriter) Write(p []byte) (n int, err error) {
	if bw.MaxBytes > 0 && bw.Written+int64(len(p)) > bw.MaxBytes {
		bw.Exceeded = true
		allowed := bw.MaxBytes - bw.Written
		if allowed > 0 {
			n, _ = bw.W.Write(p[:allowed])
			bw.Written += int64(n)
		}
		return n, &AgyError{Type: "output_too_large", Detail: "stream/output exceeded byte limit"}
	}
	n, err = bw.W.Write(p)
	bw.Written += int64(n)
	return n, err
}

// RunnerContext provides runner resolution.
func resolveRunner(optsRunner AgyCmdRunner, executorRunner AgyCmdRunner) AgyCmdRunner {
	if optsRunner != nil {
		return optsRunner
	}
	if executorRunner != nil {
		return executorRunner
	}
	if agyCmdRunner != nil {
		return agyCmdRunner
	}
	return DefaultCmdRunner
}
