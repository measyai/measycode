<p align="center">
  <br />
  <code>
  ███╗   ███╗███████╗ █████╗ ███████╗██╗   ██╗ ██████╗ ██████╗ ██████╗ ███████╗
  ████╗ ████║██╔════╝██╔══██╗██╔════╝╚██╗ ██╔╝██╔════╝██╔═══██╗██╔══██╗██╔════╝
  ██╔████╔██║█████╗  ███████║███████╗ ╚████╔╝ ██║     ██║   ██║██║  ██║█████╗
  ██║╚██╔╝██║██╔══╝  ██╔══██║╚════██║  ╚██╔╝  ██║     ██║   ██║██║  ██║██╔══╝
  ██║ ╚═╝ ██║███████╗██║  ██║███████║   ██║   ╚██████╗╚██████╔╝██████╔╝███████╗
  ╚═╝     ╚═╝╚══════╝╚═╝  ╚═╝╚══════╝   ╚═╝    ╚═════╝ ╚═════╝ ╚═════╝ ╚══════╝
  </code>
  <br /><br />
  <strong>AI coding agent that lives in your terminal.</strong>
  <br />
  <sub>Reads your project, writes code, runs builds, fixes errors — then does it again until it works.</sub>
  <br /><br />
  <a href="https://github.com/measyai/measycode/releases/latest"><img src="https://img.shields.io/github/v/release/measyai/measycode?style=flat-square&color=orange" alt="Release" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue?style=flat-square" alt="MIT License" /></a>
</p>

---

## What is MeasyCode?

MeasyCode is a terminal-based AI coding agent powered by [MeasyAI](https://measyai.com). Point it at any project folder, describe what you want, and it will:

- **Read** your existing files to understand the codebase
- **Write** new code or edit existing files
- **Run** shell commands, builds, and tests
- **Fix** errors automatically and retry until it works

No IDE plugins, no browser tabs — just `cd` into your project and type `measy`.

---

## Installation

### macOS

```bash
curl -fsSL https://github.com/measyai/measycode/releases/latest/download/install.sh | bash
```

Works on both **Apple Silicon** (M1/M2/M3/M4) and **Intel** Macs. The installer detects your architecture automatically and places the binary in `~/.local/bin`.

### Linux

```bash
curl -fsSL https://github.com/measyai/measycode/releases/latest/download/install.sh | bash
```

Supports **x86_64** and **arm64**. Installs to `~/.local/bin` by default — no `sudo` needed.

### Windows

```powershell
irm https://github.com/measyai/measycode/releases/latest/download/install.ps1 | iex
```

Installs to `%LOCALAPPDATA%\Programs\Measy` and adds it to your user PATH automatically. No admin rights required.

### Install Options

| Option | macOS / Linux | Windows |
|---|---|---|
| Custom directory | `PREFIX=/usr/local/bin ./install.sh` | `.\\install.ps1 -Dir "C:\tools"` |
| Specific version | `VERSION=1.0.3 ./install.sh` | `.\\install.ps1 -Version "1.0.3"` |

### Verify

```bash
measy -whoami
```

---

## Quick Start

```bash
# 1. Navigate to your project
cd ~/projects/my-app

# 2. Launch the agent
measy

# 3. Sign in (first run only — opens your browser)

# 4. Start asking
> add a dark mode toggle to the settings page
```

MeasyCode will read the relevant files, write the changes, build the project, and keep going until it compiles and runs.

---

## Features

| Feature | Description |
|---|---|
| **File Operations** | Read, write, edit, and list files in your project |
| **Shell Commands** | Run builds, tests, linters — anything non-interactive |
| **Auto-Fix Loop** | Build fails? It reads the error, patches the code, retries |
| **Streaming Output** | See the model think and write in real time |
| **Chain of Thought** | Watch the reasoning behind every decision (`/think` to toggle) |
| **Approval Modes** | `safe` · `balanced` · `developer` — you control what runs without asking |
| **Multi-Model** | Switch between MeasyAI models mid-session with `/model` |
| **Git Integration** | `/git status`, `/git diff`, `/git commit "msg"` — built in |
| **Workspace Switching** | `/switch ~/other-project` without restarting |
| **Usage Tracking** | `/usage` shows your token consumption with a visual bar |
| **JSONL Protocol** | `measy -jsonl` for desktop app and tool integration |

---

## CLI Reference

```
measy [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-model` | `measyai/cipher` | Model to use |
| `-dir` | `.` | Working directory for the agent |
| `-approval` | | Approval mode: `safe` \| `balanced` \| `developer` |
| `-auto` | `false` | Developer mode — never ask for approval |
| `-think` | `true` | Show the model's chain of thought |
| `-jsonl` | `false` | JSON-lines protocol on stdio (for desktop app) |
| `-login` | | Sign in and exit |
| `-logout` | | Forget stored credential and exit |
| `-whoami` | | Print the signed-in account and exit |
| `-env` | `.env` | Env file to load |

### In-Session Commands

| Command | What it does |
|---|---|
| `/help` | Show all commands |
| `/model [name]` | List or switch models |
| `/switch <path>` | Change workspace |
| `/pwd` | Show current folder + git info |
| `/open` | Open current folder in your file manager |
| `/scan` | Scan project structure |
| `/git [subcmd]` | `status` · `diff` · `log` · `commit <msg>` |
| `/login` | Browser login |
| `/logout` | Remove session |
| `/whoami` | Account info |
| `/usage` | Token usage bar |
| `/approval [mode]` | Change approval mode |
| `/auto` | Toggle Developer mode |
| `/think` | Toggle chain-of-thought display |
| `/reset` | Clear conversation history |
| `/exit` | Quit |

---

## Approval Modes

MeasyCode asks before touching your files or running commands. Choose your comfort level:

| Mode | Reads | Writes & Commands |
|---|---|---|
| **Safe** | Asks | Asks |
| **Balanced** (default) | Auto | Asks |
| **Developer** | Auto | Auto |

Switch anytime with `/approval balanced` or launch with `measy -auto` for full Developer mode.

---

## Environment Variables

| Variable | Purpose |
|---|---|
| `MEASYAI_API_KEY` | API key (skips browser login) |
| `MEASYAI_BASE_URL` | Custom API endpoint |
| `NO_COLOR` | Disable colour output |
| `FORCE_COLOR` | Force colour even when piped |

You can also put these in a `.env` file in your project root.

---

## Building from Source

```bash
git clone https://github.com/measyai/measycode.git
cd measycode
go build -o measy .
```

Cross-compile for any platform:

```bash
# macOS Apple Silicon
GOOS=darwin GOARCH=arm64 go build -o measy-darwin-arm64 .

# macOS Intel
GOOS=darwin GOARCH=amd64 go build -o measy-darwin-amd64 .

# Linux
GOOS=linux GOARCH=amd64 go build -o measy-linux-amd64 .

# Windows
GOOS=windows GOARCH=amd64 go build -o measy-windows-amd64.exe .
```

---

## License

[MIT](LICENSE) — © 2026 MeasyAI