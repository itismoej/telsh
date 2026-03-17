# telsh

SSH over Telegram. A Go-based Telegram bot that gives you a persistent root shell on your server via chat messages.

## Features

- **Persistent sessions** — `cd`, env vars, and shell state carry over between messages (PTY-based)
- **Host access** — runs commands on the host via `nsenter`, not inside the container
- **Interactive mode** — toggle raw input mode for `vim`, `nano`, and other TUI programs
- **Special keys** — send `ESC`, arrow keys, `Tab`, etc. via `/key`
- **File transfer** — upload files to and download files from the server
- **Signals** — send `Ctrl+C`, `Ctrl+Z`, `Ctrl+D`, or `SIGKILL` to running processes
- **Long output** — auto-splits into messages or sends as a `.txt` file
- **Authorized users only** — whitelist by Telegram user ID

## Setup

1. Create a bot via [@BotFather](https://t.me/BotFather) and copy the token.
2. Get your Telegram user ID from [@userinfobot](https://t.me/userinfobot).
3. Configure:

```bash
cp .env.example .env
# Edit .env with your bot token and user ID
```

4. Deploy:

```bash
docker compose up --build -d
```

## Usage

Send any text message to execute it as a shell command:

```
> ls -la /root
> cd /etc && cat hostname
> apt update
```

### Commands

| Command | Description |
|---|---|
| `/help` | Show available commands |
| `/newsession` | Start a fresh shell session |
| `/signal <name>` | Send signal: `INT`, `EOF`, `TSTP`, `KILL` |
| `/download <path>` | Download a file from the server |
| `/interactive` | Toggle interactive mode (for vim, etc.) |
| `/key <name>` | Send special key: `esc`, `enter`, `tab`, `up`, `down`, `left`, `right`, `backspace`, `delete` |

To upload a file, send it to the bot with the destination path as the caption (defaults to `/tmp/`).

### Editing files with vim

```
/interactive        — switch to interactive mode
vim config.yaml     — opens vim
/key esc            — sends ESC
:wq                 — saves and quits
/interactive        — switch back to normal mode
```

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `TELSH_BOT_TOKEN` | yes | | Telegram bot token |
| `TELSH_ALLOWED_USERS` | yes | | Comma-separated Telegram user IDs |
| `TELSH_SHELL` | no | `/bin/bash` | Shell command (use nsenter for host access) |
| `TELSH_SESSION_TIMEOUT` | no | `30` | Idle session timeout in minutes |

For full host filesystem access, set:

```
TELSH_SHELL=/usr/bin/nsenter -t 1 -m -u -i -n -p -- /bin/bash
```

This requires `privileged: true` and `pid: "host"` in `docker-compose.yml` (already configured).

## Security

- Only whitelisted Telegram user IDs can interact with the bot
- The bot uses outbound HTTPS long-polling only — no ports exposed
- The `.env` file containing the bot token is excluded from git
