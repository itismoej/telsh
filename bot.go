package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// keyMap maps human-readable key names to the byte sequences a terminal sends.
var keyMap = map[string][]byte{
	"esc":       {0x1B},
	"escape":    {0x1B},
	"tab":       {0x09},
	"enter":     {0x0D},
	"return":    {0x0D},
	"backspace": {0x7F},
	"bs":        {0x7F},
	"delete":    {0x1B, '[', '3', '~'},
	"del":       {0x1B, '[', '3', '~'},
	"up":        {0x1B, '[', 'A'},
	"down":      {0x1B, '[', 'B'},
	"right":     {0x1B, '[', 'C'},
	"left":      {0x1B, '[', 'D'},
	"home":      {0x1B, '[', 'H'},
	"end":       {0x1B, '[', 'F'},
	"pgup":      {0x1B, '[', '5', '~'},
	"pgdown":    {0x1B, '[', '6', '~'},
}

const (
	// maxMessageLen is Telegram's hard limit for a single message (characters).
	maxMessageLen = 4096
	// preWrapLen is the overhead of <pre>...</pre> tags.
	preWrapLen = 11
	// maxOutputLen is the usable output per message chunk.
	maxOutputLen = maxMessageLen - preWrapLen
)

// Bot wraps the Telegram bot API client and routes updates to handlers.
type Bot struct {
	api         *tgbotapi.BotAPI
	sm          *SessionManager
	cfg         *Config
	interactive sync.Map // user ID (int64) → bool
	screens     sync.Map // user ID (int64) → *TUIScreen
}

// NewBot initialises the Telegram bot client.
func NewBot(cfg *Config, sm *SessionManager) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}
	log.Printf("authorised as @%s (ID: %d)", api.Self.UserName, api.Self.ID)

	// Remove any active webhook — if one is set, long-polling silently receives nothing.
	if _, err := api.MakeRequest("deleteWebhook", tgbotapi.Params{}); err != nil {
		log.Printf("warning: could not delete webhook: %v", err)
	} else {
		log.Println("webhook cleared, using long-polling")
	}

	return &Bot{api: api, sm: sm, cfg: cfg}, nil
}

// Start begins long-polling for updates. It returns when ctx is cancelled.
func (b *Bot) Start(ctx context.Context) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			if update.Message != nil {
				go b.safeHandle(update.Message)
			}
		}
	}
}

// safeHandle wraps handleMessage with panic recovery so a crash in one
// goroutine doesn't silently swallow the error.
func (b *Bot) safeHandle(msg *tgbotapi.Message) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC handling message: %v\n%s", r, debug.Stack())
		}
	}()
	b.handleMessage(msg)
}

// handleMessage routes an incoming message to the appropriate handler.
func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	if msg.From == nil {
		log.Println("ignoring message with nil sender")
		return
	}

	log.Printf("message from user %d (%s): %q", msg.From.ID, msg.From.UserName, msg.Text)

	if !b.isAuthorized(msg.From.ID) {
		log.Printf("unauthorized user %d", msg.From.ID)
		b.reply(msg, "⛔ Unauthorized. Your user ID is "+fmt.Sprint(msg.From.ID))
		return
	}

	// File upload: user sends a document to the bot.
	if msg.Document != nil {
		b.handleUpload(msg)
		return
	}

	if msg.IsCommand() {
		b.handleCommand(msg)
		return
	}

	if strings.TrimSpace(msg.Text) == "" {
		return
	}

	b.handleText(msg)
}

// handleCommand dispatches bot slash commands.
func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	cmd := msg.Command()
	args := strings.TrimSpace(msg.CommandArguments())

	switch cmd {
	case "start":
		b.reply(msg, startText)

	case "help":
		b.reply(msg, helpText)

	case "newsession":
		b.sendTyping(msg.Chat.ID)
		_, err := b.sm.Reset(msg.From.ID)
		if err != nil {
			b.reply(msg, fmt.Sprintf("❌ Failed to create new session: %v", err))
			return
		}
		b.reply(msg, "✅ New shell session started.")

	case "signal":
		if args == "" {
			b.reply(msg, "Usage: /signal &lt;INT|EOF|TSTP|KILL&gt;")
			return
		}
		if b.isTUIActive(msg.From.ID) {
			if err := b.sendTUISignal(msg.From.ID, args); err != nil {
				b.reply(msg, fmt.Sprintf("❌ %v", err))
				return
			}
			if strings.EqualFold(args, "kill") || strings.EqualFold(args, "sigkill") {
				b.reply(msg, "✅ TUI mode stopped.")
				return
			}
			if err := b.refreshTUI(msg.From.ID); err != nil {
				b.reply(msg, fmt.Sprintf("❌ %v", err))
				return
			}
			return
		}
		sess, err := b.sm.Get(msg.From.ID)
		if err != nil {
			b.reply(msg, fmt.Sprintf("❌ No session: %v", err))
			return
		}
		if err := sess.SendSignal(args); err != nil {
			b.reply(msg, fmt.Sprintf("❌ %v", err))
			return
		}
		b.reply(msg, fmt.Sprintf("✅ Sent signal %s.", strings.ToUpper(args)))

	case "download":
		if args == "" {
			b.reply(msg, "Usage: /download &lt;/path/to/file&gt;")
			return
		}
		b.handleDownload(msg, args)

	case "interactive":
		if b.isTUIActive(msg.From.ID) {
			b.reply(msg, "❌ TUI mode is active. Stop it with <code>/tui stop</code> first.")
			return
		}
		on := !b.isInteractive(msg.From.ID)
		b.interactive.Store(msg.From.ID, on)
		if on {
			b.reply(msg, "✅ Interactive mode <b>ON</b>.\nText is sent raw (+ Enter). Use /key for special keys.\nRun /interactive again to switch back.")
		} else {
			b.reply(msg, "✅ Interactive mode <b>OFF</b> — back to normal.")
		}

	case "tui":
		switch {
		case args == "":
			if !b.isTUIActive(msg.From.ID) {
				b.reply(msg, "Usage: /tui &lt;command&gt; or <code>/tui stop</code>")
				return
			}
			if err := b.refreshTUI(msg.From.ID); err != nil {
				b.reply(msg, fmt.Sprintf("❌ %v", err))
			}
		case strings.EqualFold(args, "stop"):
			if err := b.stopTUI(msg.From.ID); err != nil {
				b.reply(msg, fmt.Sprintf("❌ %v", err))
				return
			}
			b.reply(msg, "✅ TUI mode stopped.")
		default:
			if err := b.startTUI(msg, args); err != nil {
				b.reply(msg, fmt.Sprintf("❌ %v", err))
				return
			}
		}

	case "screen":
		if !b.isTUIActive(msg.From.ID) {
			b.reply(msg, "❌ No active TUI. Start one with <code>/tui &lt;command&gt;</code>.")
			return
		}
		if err := b.refreshTUI(msg.From.ID); err != nil {
			b.reply(msg, fmt.Sprintf("❌ %v", err))
		}

	case "key":
		if args == "" {
			var names []string
			for k := range keyMap {
				names = append(names, k)
			}
			usage := "Usage: /key &lt;name&gt;\nSupported: <code>" + strings.Join(names, ", ") + "</code>"
			if b.isTUIActive(msg.From.ID) {
				usage += "\nTUI mode also supports <code>ctrl+x</code> (for example <code>/key ctrl+c</code>)."
			}
			b.reply(msg, usage)
			return
		}
		if b.isTUIActive(msg.From.ID) {
			if err := b.sendTUIKey(msg.From.ID, strings.ToLower(args)); err != nil {
				b.reply(msg, fmt.Sprintf("❌ %v", err))
				return
			}
			if err := b.refreshTUI(msg.From.ID); err != nil {
				b.reply(msg, fmt.Sprintf("❌ %v", err))
			}
			return
		}

		seq, ok := keyMap[strings.ToLower(args)]
		if !ok {
			b.reply(msg, fmt.Sprintf("Unknown key %q — try /key for the list.", html.EscapeString(args)))
			return
		}
		sess, err := b.sm.Get(msg.From.ID)
		if err != nil {
			b.reply(msg, fmt.Sprintf("❌ No session: %v", err))
			return
		}
		if err := sess.SendKey(seq); err != nil {
			b.reply(msg, fmt.Sprintf("❌ %v", err))
			return
		}
		b.reply(msg, fmt.Sprintf("✅ Sent %s", strings.ToUpper(args)))

	default:
		// In interactive mode, unknown /commands (like /etc/fstab) are sent
		// as raw text instead of being rejected.
		if b.isTUIActive(msg.From.ID) {
			if err := b.sendTUIText(msg.From.ID, msg.Text, true); err != nil {
				b.reply(msg, fmt.Sprintf("❌ %v", err))
				return
			}
			if err := b.refreshTUI(msg.From.ID); err != nil {
				b.reply(msg, fmt.Sprintf("❌ %v", err))
			}
			return
		}
		if b.isInteractive(msg.From.ID) {
			b.handleInteractiveText(msg)
			return
		}
		b.reply(msg, fmt.Sprintf("Unknown command /%s — try /help.", html.EscapeString(cmd)))
	}
}

// handleText treats the message as a shell command and executes it.
func (b *Bot) handleText(msg *tgbotapi.Message) {
	if b.isTUIActive(msg.From.ID) {
		b.handleTUIText(msg)
		return
	}
	if b.isInteractive(msg.From.ID) {
		b.handleInteractiveText(msg)
		return
	}

	input := msg.Text

	b.sendTyping(msg.Chat.ID)

	sess, err := b.sm.Get(msg.From.ID)
	if err != nil {
		b.reply(msg, fmt.Sprintf("❌ Could not start session: %v", err))
		return
	}

	output, busy, err := sess.Execute(input)
	if busy {
		b.reply(msg, "⚠️ A command is already running. Use /signal INT to interrupt it.")
		return
	}
	if err != nil {
		// Timeout — return partial output with a warning.
		b.sendOutput(msg.Chat.ID, output)
		b.send(msg.Chat.ID, "⚠️ Command timed out after 30 s. Use /signal INT to interrupt or /newsession to reset.")
		return
	}

	if output == "" {
		b.reply(msg, "✅ (no output)")
		return
	}

	b.sendOutput(msg.Chat.ID, output)
}

func (b *Bot) handleTUIText(msg *tgbotapi.Message) {
	if err := b.sendTUIText(msg.From.ID, msg.Text, true); err != nil {
		b.reply(msg, fmt.Sprintf("❌ %v", err))
		return
	}
	if err := b.refreshTUI(msg.From.ID); err != nil {
		b.reply(msg, fmt.Sprintf("❌ %v", err))
	}
}

// handleInteractiveText sends the message text raw to the PTY (with Enter)
// and returns whatever output comes back within a short window.
func (b *Bot) handleInteractiveText(msg *tgbotapi.Message) {
	sess, err := b.sm.Get(msg.From.ID)
	if err != nil {
		b.reply(msg, fmt.Sprintf("❌ Could not start session: %v", err))
		return
	}

	output, busy, err := sess.SendRaw(msg.Text)
	if busy {
		b.reply(msg, "⚠️ Busy — wait for the previous input to finish.")
		return
	}
	if err != nil {
		b.reply(msg, fmt.Sprintf("❌ %v", err))
		return
	}

	output = strings.TrimSpace(output)
	if output == "" {
		return // no output is normal in interactive mode (e.g. vim keystrokes)
	}
	b.sendOutput(msg.Chat.ID, output)
}

// handleDownload reads a file from the host and sends it to Telegram.
func (b *Bot) handleDownload(msg *tgbotapi.Message, path string) {
	b.sendTyping(msg.Chat.ID)

	// Check the file type on the host filesystem.
	typeOut, err := b.hostExec("stat", "-c", "%F", path).Output()
	if err != nil {
		b.reply(msg, fmt.Sprintf("❌ Cannot stat %s: file not found or not accessible.", html.EscapeString(path)))
		return
	}
	if strings.TrimSpace(string(typeOut)) == "directory" {
		b.reply(msg, "❌ Path is a directory, not a file.")
		return
	}

	// Read the file content from the host.
	data, err := b.hostExec("cat", path).Output()
	if err != nil {
		b.reply(msg, fmt.Sprintf("❌ Cannot read %s: %v", html.EscapeString(path), err))
		return
	}

	fileBytes := tgbotapi.FileBytes{Name: filepath.Base(path), Bytes: data}
	doc := tgbotapi.NewDocument(msg.Chat.ID, fileBytes)
	doc.Caption = path
	if _, err := b.api.Send(doc); err != nil {
		b.reply(msg, fmt.Sprintf("❌ Upload to Telegram failed: %v", err))
	}
}

// handleUpload downloads a file from Telegram and saves it on the host.
// Set the file's caption to the destination path (e.g. "/root/myfile.txt").
// If no caption is given, the file is saved to /tmp/<original_filename>.
func (b *Bot) handleUpload(msg *tgbotapi.Message) {
	doc := msg.Document

	dest := strings.TrimSpace(msg.Caption)
	if dest == "" {
		dest = filepath.Join("/tmp", doc.FileName)
	}

	b.sendTyping(msg.Chat.ID)

	url, err := b.api.GetFileDirectURL(doc.FileID)
	if err != nil {
		b.reply(msg, fmt.Sprintf("❌ Could not get file URL: %v", err))
		return
	}

	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		b.reply(msg, fmt.Sprintf("❌ Download from Telegram failed: %v", err))
		return
	}
	defer resp.Body.Close()

	// Ensure parent directory exists on the host.
	_ = b.hostExec("mkdir", "-p", filepath.Dir(dest)).Run()

	// Write the file to the host filesystem via tee.
	cmd := b.hostExec("tee", dest)
	cmd.Stdin = resp.Body
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		b.reply(msg, fmt.Sprintf("❌ Write to %s failed: %v", html.EscapeString(dest), err))
		return
	}

	b.reply(msg, fmt.Sprintf("✅ Saved to <code>%s</code>.", html.EscapeString(dest)))
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// hostExec builds an exec.Cmd that runs on the host filesystem.
// If the shell is configured with nsenter, the command is wrapped with the
// same nsenter prefix so it operates in the host's mount namespace.
// Otherwise, the command runs directly in the container.
func (b *Bot) hostExec(name string, args ...string) *exec.Cmd {
	if len(b.cfg.ShellPrefix) > 0 {
		fullArgs := make([]string, 0, len(b.cfg.ShellPrefix)+1+len(args))
		fullArgs = append(fullArgs, b.cfg.ShellPrefix[1:]...) // skip binary name
		fullArgs = append(fullArgs, name)
		fullArgs = append(fullArgs, args...)
		return exec.Command(b.cfg.ShellPrefix[0], fullArgs...)
	}
	return exec.Command(name, args...)
}

func (b *Bot) isAuthorized(userID int64) bool {
	return b.cfg.AllowedUsers[userID]
}

func (b *Bot) isInteractive(userID int64) bool {
	v, ok := b.interactive.Load(userID)
	return ok && v.(bool)
}

// sendOutput formats text as monospace code blocks. If the output would need
// more than 3 messages, it sends a .txt file instead.
func (b *Bot) sendOutput(chatID int64, text string) {
	runes := []rune(text)

	// Count how many messages we'd need.
	msgCount := (len(runes) + maxOutputLen - 1) / maxOutputLen
	if msgCount < 1 {
		msgCount = 1
	}

	if msgCount > 3 {
		doc := tgbotapi.NewDocument(chatID, tgbotapi.FileBytes{
			Name:  "output.txt",
			Bytes: []byte(text),
		})
		if _, err := b.api.Send(doc); err != nil {
			log.Printf("send file to %d: %v", chatID, err)
		}
		return
	}

	for len(runes) > 0 {
		chunk := runes
		if len(chunk) > maxOutputLen {
			chunk = runes[:maxOutputLen]
		}
		runes = runes[len(chunk):]

		escaped := html.EscapeString(string(chunk))
		b.send(chatID, "<pre>"+escaped+"</pre>")
	}
}

func (b *Bot) reply(msg *tgbotapi.Message, text string) {
	b.send(msg.Chat.ID, text)
}

func (b *Bot) send(chatID int64, text string) {
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = tgbotapi.ModeHTML
	if _, err := b.api.Send(m); err != nil {
		log.Printf("send to %d: %v", chatID, err)
	}
}

func (b *Bot) sendTyping(chatID int64) {
	action := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	_, _ = b.api.Request(action)
}

// ── Static strings ────────────────────────────────────────────────────────────

const startText = `<b>telsh</b> — SSH over Telegram

Send any text message and it will be executed as a shell command on the server. Your session is persistent — <code>cd</code>, environment variables, and shell state carry over between messages.

Type /help for a list of commands.`

const helpText = `<b>Commands</b>

/newsession — Start a fresh shell session
/signal &lt;name&gt; — Send signal: <code>INT</code>, <code>EOF</code>, <code>TSTP</code>, <code>KILL</code>
/download &lt;path&gt; — Download a file from the server
/interactive — Toggle interactive mode (for vim, etc.)
/tui &lt;command&gt; — Run a full-screen app in tmux and mirror its screen here
/tui stop — Stop the active TUI session
/screen — Refresh the active TUI screen
/key &lt;name&gt; — Send a special key: <code>esc</code>, <code>enter</code>, <code>tab</code>, <code>up</code>, <code>down</code>, <code>left</code>, <code>right</code>, <code>backspace</code>, <code>delete</code>, <code>home</code>, <code>end</code>
Send a file — Upload (set caption = destination path, default /tmp/)
/help — Show this message

<b>Normal mode</b> (default)
Each message runs as a shell command and returns output.

<b>Interactive mode</b> (/interactive)
Each message is sent raw + Enter. Use for vim, nano, etc.
<code>vim file.txt</code> → opens vim
<code>/key esc</code> → sends ESC
<code>:wq</code> → sends :wq + Enter (saves &amp; quits)

<b>TUI mode</b> (<code>/tui &lt;command&gt;</code>)
Runs the app inside tmux with an 80x24 screen and edits one Telegram message with the current screen.
Use plain text for typed input and <code>/key</code> for arrows, ESC, etc.
Example: <code>/tui lynx https://example.com</code>`
