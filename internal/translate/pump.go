package translate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"
)

// ErrIdleTimeout mirrors the reference's STREAM_IDLE_TIMEOUT: no upstream
// data within the idle window. Handlers map it to a 429 with retry_after.
var ErrIdleTimeout = errors.New("STREAM_IDLE_TIMEOUT")

// PumpLines reads NDJSON from r, delivering each complete line to onLine.
//
// The idle timer resets on every received CHUNK (not per line, and not per
// total request duration) — the reference's watchdog semantics. Backpressure
// propagates naturally: onLine writes to the client synchronously, so a slow
// client blocks the pump loop, fills the chunk channel, and stalls the
// upstream read — TCP flow control does the rest (the Node backpressure fix,
// free in Go).
//
// A trailing line without a final newline is still delivered.
func PumpLines(ctx context.Context, r io.Reader, idle time.Duration, onLine func(line string)) error {
	type result struct {
		chunk []byte
		err   error
	}
	chunks := make(chan result, 8)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				chunks <- result{chunk: b}
			}
			if err != nil {
				close(chunks)
				return
			}
		}
	}()

	timer := time.NewTimer(idle)
	defer timer.Stop()

	var carry []byte
	for {
		select {
		case res, ok := <-chunks:
			if !ok {
				// Upstream done. Deliver the trailing partial line.
				if len(carry) > 0 {
					onLine(string(carry))
				}
				return nil
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)

			carry = append(carry, res.chunk...)
			for {
				idx := bytes.IndexByte(carry, '\n')
				if idx < 0 {
					break
				}
				onLine(string(carry[:idx]))
				carry = carry[idx+1:]
			}
			// Drop processed prefix to keep carry bounded on huge lines.
			if len(carry) == 0 {
				carry = carry[:0]
			}

		case <-timer.C:
			return ErrIdleTimeout

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// DrainAndClose discards the rest of an upstream body and closes it, so the
// keep-alive connection returns to the pool cleanly.
func DrainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
	_ = body.Close()
}
