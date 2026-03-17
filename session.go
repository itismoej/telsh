package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	sentinelPrefix = "__TELSH_DONE_"
	shellPrompt    = "TELSH> "
	executeTimeout = 30 * time.Second
)

// Session represents a persistent PTY shell session for one user.
type Session struct {
	ptmx     *os.File
	cmd      *exec.Cmd
	mu       sync.Mutex // guards Execute (prevents concurrent command execution)
	lastUse  time.Time
	lastMu   sync.RWMutex
	chunks   chan []byte   // readLoop sends PTY output chunks here
	done     chan struct{} // closed when readLoop exits
	waitOnce sync.Once    // ensures cmd.Wait is called exactly once (no race)
}

// NewSession starts a new shell in a PTY and returns the session.
func NewSession(shell string) (*Session, error) {
	parts := strings.Fields(shell)
	if len(parts) == 0 {
		return nil, fmt.Errorf("TELSH_SHELL is empty")
	}

	log.Printf("session: starting %v", parts)
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		fmt.Sprintf("PS1=%s", shellPrompt),
		"HISTFILE=/dev/null",
	)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}
	log.Printf("session: pty started, pid=%d", cmd.Process.Pid)

	s := &Session{
		ptmx:    ptmx,
		cmd:     cmd,
		lastUse: time.Now(),
		chunks:  make(chan []byte, 128),
		done:    make(chan struct{}),
	}

	// Background goroutine continuously reads PTY output into the chunks channel.
	go s.readLoop()

	// Drain whatever the shell prints at startup (prompt, motd, etc.).
	s.drainUntilQuiet(500 * time.Millisecond)
	log.Println("session: initial drain done")

	// Force-set our sentinel PS1 in case the host's ~/.bashrc overrode it.
	// stty noflsh: don't flush the input queue on signal characters (Ctrl+C etc.)
	// — without this, our pre-queued sentinel echo command gets discarded when
	// SIGINT is sent, and Execute waits forever for a sentinel that was flushed.
	_, _ = fmt.Fprintf(s.ptmx, "export PS1='%s'; stty noflsh\n", shellPrompt)
	s.drainUntilQuiet(300 * time.Millisecond)
	log.Println("session: ready")

	return s, nil
}

// readLoop continuously reads from the PTY master and sends chunks to the
// chunks channel. It exits when the PTY is closed or errors.
func (s *Session) readLoop() {
	defer close(s.done)
	// Reap the process as soon as the PTY breaks — prevents zombies if the
	// shell exits on its own (e.g. user types "exit").
	defer s.reapProcess()

	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case s.chunks <- chunk:
			default:
				// Channel full — drop oldest chunk to make room.
				<-s.chunks
				s.chunks <- chunk
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("session: readLoop exit: %v", err)
			}
			return
		}
	}
}

// reapProcess waits for the shell process exactly once, preventing zombies.
// Safe to call from both readLoop and Close concurrently.
func (s *Session) reapProcess() {
	s.waitOnce.Do(func() {
		if s.cmd.Process != nil {
			_ = s.cmd.Wait()
		}
	})
}

// isAlive returns false if the shell process has exited.
func (s *Session) isAlive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Execute writes input to the shell and collects the output until a sentinel
// marker appears or the timeout is reached.
func (s *Session) Execute(input string) (output string, busy bool, err error) {
	if !s.mu.TryLock() {
		return "", true, nil
	}
	defer s.mu.Unlock()

	s.setLastUse(time.Now())

	sentinel := fmt.Sprintf("%s%d__", sentinelPrefix, time.Now().UnixNano())

	_, err = fmt.Fprintf(s.ptmx, "%s\necho %s\n", input, sentinel)
	if err != nil {
		return "", false, fmt.Errorf("write to pty: %w", err)
	}
	log.Printf("execute: wrote command, waiting for sentinel")

	raw, err := s.readUntilSentinel(sentinel, executeTimeout)
	if err != nil {
		log.Printf("execute: error: %v (raw %d bytes)", err, len(raw))
		return cleanOutput(raw, input, sentinel), false, err
	}

	cleaned := cleanOutput(raw, input, sentinel)
	log.Printf("execute: done, raw=%d bytes, cleaned=%d bytes", len(raw), len(cleaned))
	return cleaned, false, nil
}

// SendRaw writes input directly to the PTY without appending a sentinel.
// Used in interactive mode (vim, etc.). Returns whatever output arrives
// within a short window. The trailing \n simulates pressing Enter.
func (s *Session) SendRaw(input string) (string, bool, error) {
	if !s.mu.TryLock() {
		return "", true, nil
	}
	defer s.mu.Unlock()
	s.setLastUse(time.Now())

	if _, err := fmt.Fprint(s.ptmx, input+"\n"); err != nil {
		return "", false, err
	}

	raw := s.readForDuration(500 * time.Millisecond)
	cleaned := stripANSI(raw)
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")
	return cleaned, false, nil
}

// SendKey writes a raw byte sequence to the PTY (special keys like ESC,
// arrow keys, etc.). Does NOT acquire the session mutex so it can be used
// to interrupt a running command or control interactive programs.
func (s *Session) SendKey(seq []byte) error {
	s.setLastUse(time.Now())
	_, err := s.ptmx.Write(seq)
	return err
}

// readForDuration collects PTY output for up to d, then returns it.
func (s *Session) readForDuration(d time.Duration) string {
	var buf bytes.Buffer
	timer := time.NewTimer(d)
	defer timer.Stop()

	for {
		select {
		case chunk := <-s.chunks:
			buf.Write(chunk)
		case <-timer.C:
			// Final drain of anything already queued.
			for {
				select {
				case chunk := <-s.chunks:
					buf.Write(chunk)
				default:
					return buf.String()
				}
			}
		}
	}
}

// SendSignal writes a control character to the PTY.
func (s *Session) SendSignal(name string) error {
	switch strings.ToUpper(name) {
	case "INT", "SIGINT", "C":
		_, err := s.ptmx.Write([]byte{0x03})
		return err
	case "EOF", "D":
		_, err := s.ptmx.Write([]byte{0x04})
		return err
	case "TSTP", "SIGTSTP", "Z":
		_, err := s.ptmx.Write([]byte{0x1A})
		return err
	case "KILL", "SIGKILL":
		if s.cmd.Process == nil {
			return fmt.Errorf("no process running")
		}
		pgid, err := syscall.Getpgid(s.cmd.Process.Pid)
		if err != nil {
			return s.cmd.Process.Signal(syscall.SIGKILL)
		}
		return syscall.Kill(-pgid, syscall.SIGKILL)
	default:
		return fmt.Errorf("unknown signal %q — supported: INT, EOF, TSTP, KILL", name)
	}
}

// Close kills the entire process tree and releases the PTY.
func (s *Session) Close() {
	if s.cmd.Process != nil {
		// Kill the whole process group so child processes (background jobs,
		// subshells) don't survive as orphans.
		if pgid, err := syscall.Getpgid(s.cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = s.cmd.Process.Signal(syscall.SIGKILL)
		}
		s.reapProcess()
	}
	_ = s.ptmx.Close()
	// Wait for readLoop to exit so we don't leak the goroutine.
	<-s.done
}

func (s *Session) setLastUse(t time.Time) {
	s.lastMu.Lock()
	s.lastUse = t
	s.lastMu.Unlock()
}

func (s *Session) getLastUse() time.Time {
	s.lastMu.RLock()
	defer s.lastMu.RUnlock()
	return s.lastUse
}

// readUntilSentinel collects PTY output until the sentinel appears or timeout.
func (s *Session) readUntilSentinel(sentinel string, timeout time.Duration) ([]byte, error) {
	var buf bytes.Buffer
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case chunk := <-s.chunks:
			buf.Write(chunk)
			if bytes.Contains(buf.Bytes(), []byte(sentinel)) {
				return buf.Bytes(), nil
			}
		case <-timer.C:
			return buf.Bytes(), fmt.Errorf("timeout after %s", timeout)
		}
	}
}

// drainUntilQuiet reads and discards output until nothing arrives for quietFor.
func (s *Session) drainUntilQuiet(quietFor time.Duration) {
	for {
		select {
		case <-s.chunks:
			// Got data — reset the quiet timer by looping (time.After restarts).
			continue
		case <-time.After(quietFor):
			return
		}
	}
}

// ── Output cleaning ──────────────────────────────────────────────────────────

var ansiRe = regexp.MustCompile(`\x1b(?:[@-Z\\-_]|\[[0-9;?]*[ -/]*[@-~]|\][^\x07]*\x07)`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

func cleanOutput(raw []byte, input, sentinel string) string {
	text := stripANSI(string(raw))
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	lines := strings.Split(text, "\n")
	inputTrimmed := strings.TrimSpace(input)

	var result []string
	foundInput := false

	for _, line := range lines {
		if strings.Contains(line, sentinel) {
			break
		}
		line = strings.ReplaceAll(line, shellPrompt, "")

		if !foundInput {
			if inputTrimmed != "" && strings.Contains(line, inputTrimmed) {
				foundInput = true
			}
			continue
		}

		if strings.Contains(line, sentinelPrefix) {
			continue
		}

		result = append(result, line)
	}

	if !foundInput {
		result = nil
		for _, line := range lines {
			if strings.Contains(line, sentinel) || strings.Contains(line, sentinelPrefix) {
				break
			}
			line = strings.ReplaceAll(line, shellPrompt, "")
			if strings.TrimSpace(line) != "" {
				result = append(result, line)
			}
		}
	}

	for len(result) > 0 && strings.TrimSpace(result[len(result)-1]) == "" {
		result = result[:len(result)-1]
	}

	return strings.Join(result, "\n")
}

// ── Session Manager ──────────────────────────────────────────────────────────

type SessionManager struct {
	mu       sync.Mutex
	sessions map[int64]*Session
	shell    string
	timeout  time.Duration
}

func NewSessionManager(shell string, timeout time.Duration) *SessionManager {
	sm := &SessionManager{
		sessions: make(map[int64]*Session),
		shell:    shell,
		timeout:  timeout,
	}
	go sm.reapLoop()
	return sm
}

func (sm *SessionManager) Get(userID int64) (*Session, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if s, ok := sm.sessions[userID]; ok {
		if s.isAlive() {
			return s, nil
		}
		// Shell died (user typed "exit", crash, etc.) — clean up and recreate.
		log.Printf("session: user %d session dead, auto-recreating", userID)
		s.Close()
		delete(sm.sessions, userID)
	}
	return sm.newSessionLocked(userID)
}

func (sm *SessionManager) Reset(userID int64) (*Session, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if s, ok := sm.sessions[userID]; ok {
		s.Close()
		delete(sm.sessions, userID)
	}
	return sm.newSessionLocked(userID)
}

func (sm *SessionManager) CloseAll() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, s := range sm.sessions {
		s.Close()
	}
	sm.sessions = make(map[int64]*Session)
}

func (sm *SessionManager) newSessionLocked(userID int64) (*Session, error) {
	s, err := NewSession(sm.shell)
	if err != nil {
		return nil, err
	}
	sm.sessions[userID] = s
	return s, nil
}

func (sm *SessionManager) reapLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		sm.mu.Lock()
		for id, s := range sm.sessions {
			if !s.isAlive() || time.Since(s.getLastUse()) > sm.timeout {
				log.Printf("session: reaping user %d (alive=%v, idle=%s)",
					id, s.isAlive(), time.Since(s.getLastUse()).Round(time.Second))
				s.Close()
				delete(sm.sessions, id)
			}
		}
		sm.mu.Unlock()
	}
}
