# cc-qoder-bridge

Use your Qoder subscription in Claude Code.

## Install

Download and extract the archive for your platform from
[Releases](https://github.com/noknov/cc-qoder-bridge/releases/latest).

| Platform | Download |
| --- | --- |
| macOS · Apple Silicon | [arm64](https://github.com/noknov/cc-qoder-bridge/releases/latest/download/cc-qoder-bridge_darwin_arm64.tar.gz) |
| macOS · Intel | [amd64](https://github.com/noknov/cc-qoder-bridge/releases/latest/download/cc-qoder-bridge_darwin_amd64.tar.gz) |
| Linux · ARM64 | [arm64](https://github.com/noknov/cc-qoder-bridge/releases/latest/download/cc-qoder-bridge_linux_arm64.tar.gz) |
| Linux · x86-64 | [amd64](https://github.com/noknov/cc-qoder-bridge/releases/latest/download/cc-qoder-bridge_linux_amd64.tar.gz) |

Requires Claude Code and a Qoder international account (`qoder.com`).

## Usage

```sh
./cc-qoder-bridge login
./cc-qoder-bridge models
./cc-qoder-bridge serve
```

Keep the service running. In another terminal, choose a model ID from `models`:

```sh
eval "$(./cc-qoder-bridge claude-env --model MODEL_ID)"
claude
```

Requests use your Qoder credits. Existing Claude Code provider settings may
conflict; check `/status` after connecting.

## Options

- `--state PATH`: use a separate state directory; run one service per directory.
- `--port PORT`: change the port for both `serve` and `claude-env`.
- `login --no-browser`: open the authorization URL manually.

## Limitations

Qoder CN, documents, forced tool choices, custom stop sequences, temperature/top-p
controls, server-side tools, exact token counting, and native Anthropic thinking
signatures are unsupported. Qoder protocol changes may affect compatibility.

## License

MIT. See `LICENSE` and `THIRD_PARTY_NOTICES`.
