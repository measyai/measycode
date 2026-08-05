# MeasyCode

> AI coding agent that lives in your terminal.

[![Release](https://img.shields.io/github/v/release/measyai/measycode?style=flat-square&color=orange)](https://github.com/measyai/measycode/releases/latest)
[![License](https://img.shields.io/badge/license-MIT-blue?style=flat-square)](LICENSE)

`cd` into a project. Type `measy`. Describe what you want.  
It reads your code, writes changes, runs builds, fixes errors — and keeps going until it works.

## Install

**macOS / Linux**

```bash
curl -fsSL https://github.com/measyai/measycode/releases/latest/download/install.sh | bash
```

**Windows**

```powershell
irm https://github.com/measyai/measycode/releases/latest/download/install.ps1 | iex
```

<details>
<summary>Advanced options</summary>

```bash
# Custom install directory
PREFIX=/usr/local/bin ./install.sh

# Specific version
VERSION=1.0.3 ./install.sh
```

```powershell
# Custom directory
.\install.ps1 -Dir "C:\tools"

# Specific version
.\install.ps1 -Version "1.0.3"
```

</details>

## Usage

```bash
measy                          # start in current directory
measy -model measyai/cipher    # pick a model
measy -auto                    # skip all approval prompts
measy -login                   # sign in
measy -whoami                  # check account
```

Prefix any prompt with `ultrathink` to trigger maximum reasoning effort for that turn:
```text
> ultrathink fix the race condition in worker pool
```

### Commands

Once inside a session, type `/help` or use any of these:

| | |
|---|---|
| `/model [name]` | List or switch models |
| `/switch <path>` | Change workspace |
| `/git status\|diff\|log\|commit` | Built-in git |
| `/approval safe\|balanced\|developer` | Change approval mode |
| `/usage` | Token consumption |
| `/think` | Toggle chain-of-thought |
| `/mcp` | MCP server status |
| `/reset` | Clear conversation |
| `/exit` | Quit |

### Project Instructions

Drop a `MEASY.md` (or `AGENTS.md`, or `.measycode/instructions.md`) into your project root and measycode reads it into every session — build commands, style rules, off-limits areas:

```markdown
# My Project

## Build
- Always run `make test` before declaring a change done.
- Use `pnpm`, not `npm`.

## Style
- Prefer `pathlib.Path` over `os.path`.
```

First match wins: `MEASY.md` → `AGENTS.md` → `.measycode/instructions.md`. Files are capped at 20,000 characters.

### MCP Servers

Connect MCP servers to give the agent tools beyond the built-in five. Configure in `~/.measycode/config.json` (user-level, every workspace) or `<project>/.measycode/config.json` (project-level, wins per server name):

```json
{
  "mcp_servers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_..." }
    },
    "company_api": {
      "url": "https://mcp.internal.company.com/mcp",
      "headers": { "Authorization": "Bearer sk-..." },
      "timeout": 180
    }
  }
}
```

Tools appear as `mcp_<server>_<tool>` and ask for approval like any write (auto-approved only in Developer mode). Server subprocesses receive a filtered environment — secrets stay out unless explicitly passed via `env`. Check `/mcp` for connection status.

### Approval Modes

| Mode | Reads | Writes |
|---|---|---|
| Safe | Asks | Asks |
| **Balanced** (default) | Auto | Asks |
| Developer | Auto | Auto |

## Build from Source

```bash
git clone https://github.com/measyai/measycode.git
cd measycode
go build -o measy .
```

## License

[MIT](LICENSE)