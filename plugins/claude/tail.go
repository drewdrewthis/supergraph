package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// maxTranscriptLine caps a single JSONL record. A well-formed transcript line is a few
// KiB; a line larger than this (a corrupt/never-terminated write) is skipped, not
// buffered — bounding tail memory regardless of what lands in projectsDir.
const maxTranscriptLine = 1 << 20

// tailKey names one transcript's byte-offset cursor in core's cursors table, so a
// scan resumes where the last left off instead of re-reading the file
// (AC-CLAUDE-CURSOR).
func tailKey(path string) string { return "tail:" + path }

// runTail is the pull channel (EDR §"Decision"): a poll-scan of projectsDir on a
// ticker — the correctness floor that backfills hook-less/pre-install sessions and
// enriches with fields the hook lacks. It is deliberately fsnotify-free (a ticker,
// not inotify) so the plugin pulls in no new dependency; the byte-offset cursor keeps
// each re-scan cheap.
func (p *Plugin) runTail(ctx context.Context) {
	p.scanOnce(ctx)
	t := time.NewTicker(p.scanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.scanOnce(ctx)
		}
	}
}

// scanOnce folds every new transcript line then runs the pid-liveness sweep.
func (p *Plugin) scanOnce(ctx context.Context) {
	files, _ := filepath.Glob(filepath.Join(p.projectsDir, "*", "*.jsonl"))
	for _, f := range files {
		p.scanFile(ctx, f)
	}
	p.sweep(ctx)
	if p.retentionDays > 0 {
		_ = p.store.prune(ctx, p.now().Add(-time.Duration(p.retentionDays)*24*time.Hour))
	}
}

// scanFile reads the bytes appended since this transcript's cursor, folding each
// COMPLETE (newline-terminated) JSON line and advancing the cursor. A trailing
// partial line (a mid-write flush) is left unconsumed so it is folded once, whole, on
// a later scan (fold-state.sh:47 tolerance).
func (p *Plugin) scanFile(ctx context.Context, path string) {
	off, _ := strconv.ParseInt(p.store.cursor(ctx, tailKey(path)), 10, 64)
	fh, err := os.Open(path) //nolint:gosec // path comes from our own projectsDir glob
	if err != nil {
		return
	}
	defer func() { _ = fh.Close() }()
	if fi, err := fh.Stat(); err != nil || fi.Size() <= off {
		return
	}
	if _, err := fh.Seek(off, io.SeekStart); err != nil {
		return
	}
	rd := bufio.NewReader(fh)
	newOff := off
	for {
		line, oversized, consumed, complete := readLine(rd, maxTranscriptLine)
		if !complete {
			break // EOF with no trailing newline: a partial line, do not consume it
		}
		newOff += consumed
		if oversized {
			// Cursor still advances past it, so it is never re-read; we just skip the fold.
			log.Printf("claude: skipping oversized transcript line (> %d bytes) in %s", maxTranscriptLine, path)
			continue
		}
		p.foldLine(ctx, line)
	}
	if newOff != off {
		_ = p.store.setCursor(ctx, tailKey(path), strconv.FormatInt(newOff, 10))
	}
}

// readLine reads one '\n'-terminated line, buffering at most cap bytes. It returns the
// line (nil when oversized), whether it exceeded cap, how many bytes were consumed from
// rd (so the cursor advances even past a skipped line), and whether a full line was
// read. On EOF before any '\n' it reports complete=false and consumed=0, leaving a
// mid-write partial line unconsumed to be folded whole on a later scan.
func readLine(rd *bufio.Reader, cap int) (line []byte, oversized bool, consumed int64, complete bool) {
	var buf []byte
	for {
		b, err := rd.ReadByte()
		if err != nil {
			return nil, false, 0, false
		}
		consumed++
		if !oversized {
			if len(buf) >= cap {
				oversized, buf = true, nil // stop buffering; keep consuming to the newline
			} else {
				buf = append(buf, b)
			}
		}
		if b == '\n' {
			return buf, oversized, consumed, true
		}
	}
}

// foldLine projects one transcript record to enrichment + PreToolUse folds and
// applies them. A malformed line is skipped (its bytes still count, so the cursor
// still advances past it).
func (p *Plugin) foldLine(ctx context.Context, line []byte) {
	var rec transcriptRecord
	if json.Unmarshal(line, &rec) != nil || rec.SessionID == "" {
		return
	}
	enr, folds := projectRecord(rec)
	if err := p.store.applyEnrichment(ctx, rec.SessionID, p.hostID, enr, p.now()); err == nil {
		p.emitSessionUpdated(ctx, rec.SessionID)
	}
	for _, fi := range folds {
		if fi.ts == "" {
			fi.ts = p.now().Format(rfc)
		}
		if applied, err := p.store.applyFold(ctx, fi, p.hostID, p.now()); err == nil && applied {
			p.emitForFold(ctx, fi)
		}
	}
}

// sweep marks sessions whose process is gone as stale (AC-CLAUDE-STALE) and emits the
// ended/updated envelopes for each.
func (p *Plugin) sweep(ctx context.Context) {
	if !p.pidLiveness {
		return
	}
	dead, err := p.store.staleScan(ctx, p.now(), p.alive)
	if err != nil {
		return
	}
	for _, sid := range dead {
		p.emit(ctx, "claude.session.ended", sessionKey(sid, p.hostID), map[string]any{"sid": sid})
		p.emitSessionUpdated(ctx, sid)
	}
}

// realAlive reports whether pid names a live process. signal 0 probes existence:
// nil (running) or EPERM (running, not ours) is alive; ESRCH is dead.
func realAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
