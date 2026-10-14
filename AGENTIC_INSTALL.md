# Agentic install

Instructions for an AI coding agent (Claude Code, Codex, Pi, ...) installing claude-rollet
for its user. A person can paste this into their agent:

> Install claude-rollet for me by following
> https://raw.githubusercontent.com/ruivalim/claude-rollet/main/AGENTIC_INSTALL.md

---

**Agent: read all of this before running anything.** The commands `rollet init` and
`rollet provider add` are interactive terminal forms you can't drive. You write the
config file directly instead, then use the non-interactive commands to check your work.

## 1. Check the platform

Run `uname -s` and `uname -m`. Supported: Linux and macOS, on x86_64 or arm64. On
anything else (Windows included), stop and tell the user it isn't supported.

## 2. Install the binary

Prefer the release binary:

```sh
curl -fsSL https://raw.githubusercontent.com/ruivalim/claude-rollet/main/install.sh | sh
```

It installs to `~/.local/bin/rollet` and verifies the SHA-256 checksum. If that directory
isn't on `PATH`, tell the user how to add it, and use the full path for now.

If there's no release yet, or the user would rather build from source, and Go 1.25+ is
installed:

```sh
go install github.com/ruivalim/claude-rollet/cmd/rollet@latest
```

Confirm with `rollet version`.

## 3. Ask the user what goes in the roulette

Ask, don't assume:

1. **Keep Claude in the roulette?** Almost always yes: without it there's nothing to
   compare against. It uses their existing Claude Code login, so no key is needed.
2. **Which other providers?** Built in: `mimo` (Xiaomi MiMo), `deepseek`, `chatgpt`
   (OpenAI API key), `codex` (ChatGPT subscription, no key), and `openai` (any
   OpenAI-compatible endpoint: they give you the base URL).
3. **A model for each.** Suggestions: `mimo-v2.5-pro`; `deepseek-v4-pro`; `gpt-5.5` for
   `chatgpt`; `gpt-6-sol` for `codex`. If they don't know, use the suggestion.
4. **How often should Claude come up?** That's `claude_odds`: `0.5` means half of the new
   sessions. Lower values show the other models more often.

**API keys:** ask the user to set them themselves rather than pasting them into the chat.
Two options:

- They run `rollet provider edit <name>` afterwards and paste the key into the form, or
- They edit `config.json` and fill in `api_key` themselves.

If they insist on handing you the key, write it into the config, never into a shell
command, and don't echo it back. Don't use `api_key_env`: the background service doesn't
see shell environment variables.

## 4. Write the config

The path is `~/.config/claude-rollet/config.json`, or `$XDG_CONFIG_HOME/claude-rollet/config.json`
if `XDG_CONFIG_HOME` is set. If the file already exists, show it to the user and ask
before changing anything. Create it with mode 0600:

```json
{
  "listen": "127.0.0.1:7777",
  "claude_odds": 0.5,
  "providers": [
    { "name": "claude", "type": "anthropic", "base_url": "https://api.anthropic.com" },
    { "name": "mimo", "type": "mimo", "api_key": "PASTE_KEY_HERE", "model": "mimo-v2.5-pro" },
    { "name": "gpt", "type": "codex", "model": "gpt-6-sol" }
  ]
}
```

Named adapters (`mimo`, `deepseek`, `chatgpt`, `codex`) don't need `base_url`. A generic
`openai` provider does.

## 5. Sign in to ChatGPT (only if there's a `codex` provider)

`rollet codex login` prints a code and a URL, then waits up to 15 minutes. Run it in the
background or in a way where you can relay its output. Show the user the URL and the code
right away, ask them to approve it in the browser, then wait for the command to finish.
`rollet codex status` confirms it worked.

## 6. Test every provider

```sh
rollet provider test --all
```

It checks plain text, streaming, and a tool call round trip through the real translation
layer. A Claude provider without a key prints that there's nothing to test from here.
That's expected, not a failure. For any other failure, show the user the error. The usual
causes are a wrong key, a wrong model id, or no credit left on the account.

## 7. Run it 24/7

```sh
rollet service install
```

This sets up a systemd user unit on Linux or a LaunchAgent on macOS. It checks that the
proxy answers before it returns. On Linux, mention `loginctl enable-linger $USER` if they
want it running while logged out.

## 8. Point Claude Code at it

Ask which they prefer:

- **Every session goes through the roulette** (the blind experiment as intended): append
  the output of `rollet env` to their shell rc (`~/.zshrc`, `~/.bashrc`, fish and nu via
  `rollet env --shell fish|nu`). Check that the line isn't already there before
  appending. Explain that if the service stops, Claude Code can't connect until it's back
  or the line is removed.
- **Opt in per session:** they run `rollet claude` instead of `claude`. Nothing to change.

## 9. Hand over

Tell the user, briefly:

- `rollet sessions` lists sessions with the provider hidden: Enter opens one, `g` guesses,
  `r` reveals.
- `rollet stats` shows how well they tell the models apart.
- `rollet delay first 40-70s --provider claude` slows Claude's first answer down, because
  speed is the biggest giveaway.
- Don't spoil the experiment: don't run `rollet sessions --reveal` or `rollet serve
  --reveal` for them, and don't read `sessions.json` back to them.
