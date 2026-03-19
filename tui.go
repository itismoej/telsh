package main

import (
	"fmt"
	"html"
	"os/exec"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	tuiWidth  = 80
	tuiHeight = 24
)

var tmuxKeyMap = map[string]string{
	"esc":       "Escape",
	"escape":    "Escape",
	"tab":       "Tab",
	"enter":     "Enter",
	"return":    "Enter",
	"backspace": "BSpace",
	"bs":        "BSpace",
	"delete":    "DC",
	"del":       "DC",
	"up":        "Up",
	"down":      "Down",
	"right":     "Right",
	"left":      "Left",
	"home":      "Home",
	"end":       "End",
	"pgup":      "PgUp",
	"pgdown":    "PgDn",
}

type TUIScreen struct {
	Session   string
	ChatID    int64
	MessageID int
}

func (b *Bot) isTUIActive(userID int64) bool {
	_, ok := b.screens.Load(userID)
	return ok
}

func (b *Bot) getTUIScreen(userID int64) (*TUIScreen, bool) {
	v, ok := b.screens.Load(userID)
	if !ok {
		return nil, false
	}
	screen, ok := v.(*TUIScreen)
	return screen, ok
}

func (b *Bot) startTUI(msg *tgbotapi.Message, command string) error {
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("missing command")
	}

	session := tuiSessionName(msg.From.ID)
	_ = b.hostExec("tmux", "kill-session", "-t", session).Run()
	b.interactive.Store(msg.From.ID, false)

	cmd := b.hostExec(
		"tmux",
		"new-session",
		"-d",
		"-s", session,
		"-x", fmt.Sprint(tuiWidth),
		"-y", fmt.Sprint(tuiHeight),
		"/bin/sh", "-lc", command,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return formatTmuxError("start TUI", err, out)
	}

	screen := &TUIScreen{Session: session, ChatID: msg.Chat.ID}
	b.screens.Store(msg.From.ID, screen)
	return b.postTUI(msg.From.ID)
}

func (b *Bot) stopTUI(userID int64) error {
	screen, ok := b.getTUIScreen(userID)
	if !ok {
		return fmt.Errorf("no active TUI")
	}
	_ = b.hostExec("tmux", "kill-session", "-t", screen.Session).Run()
	b.screens.Delete(userID)
	return nil
}

func (b *Bot) refreshTUI(userID int64) error {
	screen, ok := b.getTUIScreen(userID)
	if !ok {
		return fmt.Errorf("no active TUI")
	}

	rendered, err := b.captureTUI(screen.Session)
	if err != nil {
		b.screens.Delete(userID)
		return err
	}

	text := formatTUIScreen(rendered)
	edit := tgbotapi.NewEditMessageText(screen.ChatID, screen.MessageID, text)
	edit.ParseMode = tgbotapi.ModeHTML
	if _, err := b.api.Send(edit); err != nil {
		// Telegram rejects edits when the contents have not changed.
		if strings.Contains(strings.ToLower(err.Error()), "message is not modified") {
			return nil
		}
		return err
	}
	return nil
}

func (b *Bot) postTUI(userID int64) error {
	screen, ok := b.getTUIScreen(userID)
	if !ok {
		return fmt.Errorf("no active TUI")
	}

	rendered, err := b.captureTUI(screen.Session)
	if err != nil {
		b.screens.Delete(userID)
		return err
	}

	msg := tgbotapi.NewMessage(screen.ChatID, formatTUIScreen(rendered))
	msg.ParseMode = tgbotapi.ModeHTML
	sent, err := b.api.Send(msg)
	if err != nil {
		return err
	}

	screen.MessageID = sent.MessageID
	b.screens.Store(userID, screen)
	return nil
}

func (b *Bot) sendTUIText(userID int64, text string, sendEnter bool) error {
	screen, ok := b.getTUIScreen(userID)
	if !ok {
		return fmt.Errorf("no active TUI")
	}

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			cmd := b.hostExec("tmux", "send-keys", "-t", screen.Session, "-l", line)
			if out, err := cmd.CombinedOutput(); err != nil {
				return formatTmuxError("send text", err, out)
			}
		}
		if sendEnter || i < len(lines)-1 {
			if err := b.sendTmuxKey(screen.Session, "Enter"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Bot) sendTUIKey(userID int64, key string) error {
	screen, ok := b.getTUIScreen(userID)
	if !ok {
		return fmt.Errorf("no active TUI")
	}

	tmuxKey, ok := tmuxKeyName(key)
	if !ok {
		return fmt.Errorf("unknown key %q", key)
	}
	return b.sendTmuxKey(screen.Session, tmuxKey)
}

func (b *Bot) sendTUISignal(userID int64, name string) error {
	screen, ok := b.getTUIScreen(userID)
	if !ok {
		return fmt.Errorf("no active TUI")
	}

	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "":
		return fmt.Errorf("missing signal name")
	case "INT", "SIGINT", "C":
		return b.sendTmuxKey(screen.Session, "C-c")
	case "EOF", "D":
		return b.sendTmuxKey(screen.Session, "C-d")
	case "TSTP", "SIGTSTP", "Z":
		return b.sendTmuxKey(screen.Session, "C-z")
	case "KILL", "SIGKILL":
		return b.stopTUI(userID)
	default:
		return fmt.Errorf("unknown signal %q — supported: INT, EOF, TSTP, KILL", name)
	}
}

func (b *Bot) sendTmuxKey(session, key string) error {
	cmd := b.hostExec("tmux", "send-keys", "-t", session, key)
	if out, err := cmd.CombinedOutput(); err != nil {
		return formatTmuxError("send key", err, out)
	}
	return nil
}

func (b *Bot) captureTUI(session string) (string, error) {
	argsList := [][]string{
		{"capture-pane", "-p", "-a", "-t", session},
		{"capture-pane", "-p", "-t", session},
	}

	var lastErr error
	for i, args := range argsList {
		cmd := b.hostExec("tmux", args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			lastErr = formatTmuxError("capture screen", err, out)
			continue
		}

		rendered := strings.ReplaceAll(string(out), "\r", "")
		if i == len(argsList)-1 || strings.TrimSpace(rendered) != "" {
			return rendered, nil
		}
	}

	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("capture screen failed")
}

func tmuxKeyName(key string) (string, bool) {
	if strings.HasPrefix(key, "ctrl+") && len(key) == len("ctrl+")+1 {
		return "C-" + strings.ToLower(key[len("ctrl+"):]), true
	}
	if strings.HasPrefix(key, "ctrl-") && len(key) == len("ctrl-")+1 {
		return "C-" + strings.ToLower(key[len("ctrl-"):]), true
	}
	v, ok := tmuxKeyMap[key]
	return v, ok
}

func tuiSessionName(userID int64) string {
	return fmt.Sprintf("telsh-tui-%d", userID)
}

func formatTUIScreen(screen string) string {
	screen = strings.TrimRight(screen, "\n")
	if screen == "" {
		screen = "(blank screen)"
	}

	lines := strings.Split(screen, "\n")
	if len(lines) > tuiHeight {
		lines = lines[len(lines)-tuiHeight:]
	}

	var kept []string
	used := preWrapLen
	for _, line := range lines {
		escaped := html.EscapeString(line)
		if used+len(escaped)+1 > maxMessageLen {
			break
		}
		kept = append(kept, line)
		used += len(escaped) + 1
	}

	if len(kept) == 0 {
		kept = []string{"(screen too wide for Telegram)"}
	}

	return "<pre>" + html.EscapeString(strings.Join(kept, "\n")) + "</pre>"
}

func formatTmuxError(action string, err error, output []byte) error {
	text := strings.TrimSpace(string(output))
	if text == "" {
		text = err.Error()
	}
	if ee, ok := err.(*exec.Error); ok && ee.Err != nil {
		text = ee.Err.Error()
	}
	return fmt.Errorf("%s failed: %s", action, text)
}
