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
| `/reset` | Clear conversation |
| `/exit` | Quit |

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