package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"time"

	"alert-system/internal/agentprotocol"
)

const outputLimit = 8192

// boundedOutput drains the pipe even after the capture limit is reached.
type boundedOutput struct {
	data  []byte
	total int64
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.total += int64(len(p))
	b.data = append(b.data, p[:min(len(p), outputLimit-len(b.data))]...)
	return len(p), nil
}

func (c *Client) execute(parent context.Context, h Handler, d agentprotocol.Delivery) {
	ctx, cancel := context.WithTimeout(parent, h.Timeout)
	defer cancel()
	input, err := json.Marshal(d.Envelope)
	if err != nil {
		c.log.Error("encode handler input", "message_id", d.MessageID)
		return
	}
	cmd := exec.CommandContext(ctx, h.Exec[0], h.Exec[1:]...)
	if err := prepareGroup(cmd); err != nil {
		c.log.Error("prepare handler", "message_id", d.MessageID, "error", err)
		return
	}
	// Only local config selects argv. Child processes receive neither client token nor ambient credentials.
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "MESSAGE_ID=" + d.MessageID, "EVENT_TYPE=" + d.Envelope.EventType}
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	start := time.Now()
	err = cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		_ = killGroup(cmd)
	}
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	c.log.Info("handler finished", "message_id", d.MessageID, "exit_code", code, "success", err == nil, "timeout", errors.Is(ctx.Err(), context.DeadlineExceeded), "duration_ms", time.Since(start).Milliseconds(), "stdout_bytes", stdout.total, "stderr_bytes", stderr.total)
	if c.cfg.ResultDir != "" {
		if err := c.saveOutput(d, stdout, stderr, code); err != nil {
			c.log.Error("save private handler output", "message_id", d.MessageID)
		}
	}
}

func (c *Client) saveOutput(d agentprotocol.Delivery, stdout, stderr boundedOutput, code int) error {
	// Receipts are validated before launch. Use a unique 0600 file for each run.
	f, err := os.CreateTemp(c.cfg.ResultDir, d.MessageID+"-*.json")
	if err != nil {
		return err
	}
	defer f.Close()
	clean := func(b []byte) string { return string(bytes.ReplaceAll(b, []byte(c.token), []byte("[REDACTED]"))) }
	return json.NewEncoder(f).Encode(map[string]any{"message_id": d.MessageID, "attempt": d.Attempt, "exit_code": code, "stdout": clean(stdout.data), "stderr": clean(stderr.data), "stdout_truncated": stdout.total > outputLimit, "stderr_truncated": stderr.total > outputLimit})
}
