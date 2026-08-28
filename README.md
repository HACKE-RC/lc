# lc

`lc` lists local coding-agent sessions for the Git repository you are in. It reads local session stores only; no transcript leaves your machine.

<img width="2560" height="1502" alt="image" src="https://github.com/user-attachments/assets/c1b32ef6-bd31-4c9b-94e4-1e2c45159d8d" />


## Install

Install the published package with `uv`:

```sh
uv tool install list-coding-agents
```

`pipx` and `pip` work too:

```sh
pipx install list-coding-agents
python3 -m pip install --user list-coding-agents
```

To install a development checkout instead:

```sh
sh scripts/install.sh
```

The checkout installer prefers `uv tool install`, then `pipx`, then `pip --user`. Make sure the resulting scripts directory is on your `PATH`, then run `lc`.

The current PyPI distribution is `list-coding-agents` version `0.1.0`. Its import package and command are both still `lc`:

```sh
lc --version
```

## Quick start

```sh
# Sessions for the current repository
lc

# Browse another directory
lc -I ~/projects/lc

# Restrict to agents; an existing directory can be positional too
lc codex claude
lc ~/projects/lc codex

# Every repository or machine-readable output
lc --all
lc --json
```

By default, `lc` shows at most 40 matching sessions. Use `--limit 0` for all of them.

## Filters

```sh
lc --agent codex --agent claude
lc -X gemini
lc --only-folder api
lc --except-folder 'worktrees/*'
lc --ids
lc --no-size
```

`--only-folder` and `--except-folder` accept a subdirectory, absolute path, glob, or bare directory name. `lc --help` has the full option list.

## Interactive browser

`lc -I` opens a terminal browser with an agent/directory pane, a title pane, and a Markdown-rendered preview. Codex is blue; Claude Code uses the orange accent.

| Key or action | Result |
| --- | --- |
| `j` / `k`, arrows | Move through sessions |
| `gg` / `G` | First / last session |
| `Ctrl-D` / `Ctrl-U`, Page Up / Down | Move by a page |
| `/` | Filter agent, directory, or title |
| Drag `│` | Resize panes |
| `[` / `]` | Resize the title pane without a mouse |
| `h`, `u`, `H` | Hide a directory, undo, or restore all |
| `i` | Toggle IDs |
| `Enter` | Resume when the agent supports it |
| `p` | Print the selected transcript path |
| `q` or `Esc` | Quit |

The selected session's age and transcript size appear at the bottom right.

## Names and performance

Codex and Claude Code native names take precedence. Other stores fall back to the first useful user prompt. Generic startup titles and injected context are ignored.

Codex rollouts can add up. `lc` reads compact metadata first and only opens a full transcript when it needs a title. It keeps a disposable local cache at:

```text
$XDG_CACHE_HOME/lc/codex-sessions.json
```

If `XDG_CACHE_HOME` is unset, the path is `~/.cache/lc/codex-sessions.json`. The cache is validated with transcript timestamp and size. Delete it whenever you want; `lc` will rebuild it.

## Supported stores

Claude Code, Codex, Droid, OpenCode, Cursor, GitHub Copilot, Grok, Kimi, and Gemini. Missing stores are ignored.

## Development

```sh
uv run --no-project python -m unittest discover -s tests -q
```

The tests cover title fallbacks and the Codex cache contract.
