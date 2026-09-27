package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// Step is a named unit of work.
type Step struct {
	Name string
	Fn   func(ctx context.Context) error
}

// StepList runs a sequence of steps, stopping at the first failure.
type StepList struct {
	Steps []Step
}

func (s *StepList) Add(name string, fn func(ctx context.Context) error) {
	s.Steps = append(s.Steps, Step{Name: name, Fn: fn})
}

// AddRun appends a step that runs a remote command over SSH.
func (s *StepList) AddRun(node Node, name string, cmd string) {
	s.Add(name, func(ctx context.Context) error {
		return node.Run(ctx, cmd)
	})
}

// AddUploadDir appends a step that syncs a directory via rsync.
func (s *StepList) AddUploadDir(node Node, name string, localDir, remoteDir string, extraFlags ...string) {
	s.Add(name, func(ctx context.Context) error {
		return node.UploadDir(ctx, localDir, remoteDir, extraFlags...)
	})
}

type contextKey string

const loggerKey contextKey = "logger"

// stepLogger buffers a step's output in memory so a successful step stays quiet
// and a failed one can dump everything it printed.
type stepLogger struct {
	buf *bytes.Buffer
}

func (l *stepLogger) Write(p []byte) (int, error) { return l.buf.Write(p) }

func writerFrom(ctx context.Context, fallback io.Writer) io.Writer {
	if ctx == nil {
		return fallback
	}
	w, ok := ctx.Value(loggerKey).(io.Writer)
	if !ok {
		return fallback
	}
	return w
}

func getStdout(ctx context.Context) io.Writer { return writerFrom(ctx, os.Stdout) }
func getStderr(ctx context.Context) io.Writer { return writerFrom(ctx, os.Stderr) }

// Run executes every step in order. Output is buffered per step and only
// printed when that step fails, so a clean run reads as a short checklist.
func (s *StepList) Run(ctx context.Context) error {
	for i, step := range s.Steps {
		fmt.Fprintf(os.Stderr, "  [%d/%d] %s ... ", i+1, len(s.Steps), step.Name)

		logger := &stepLogger{buf: &bytes.Buffer{}}
		start := time.Now()
		err := step.Fn(context.WithValue(ctx, loggerKey, logger))
		elapsed := time.Since(start).Round(time.Millisecond)

		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED (%s)\n", elapsed)
			fmt.Fprintf(os.Stderr, "\n--- output of %q ---\n", step.Name)
			os.Stderr.Write(logger.buf.Bytes())
			fmt.Fprintf(os.Stderr, "--- end output ---\n\n")
			return fmt.Errorf("step %q: %w", step.Name, err)
		}

		fmt.Fprintf(os.Stderr, "ok (%s)\n", elapsed)
	}
	return nil
}
