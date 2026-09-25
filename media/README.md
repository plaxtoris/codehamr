# Demo media

`codehamr.gif` shows the terminal UI repairing a small Python API with
`qwen/qwen3.8-27b`. The recording uses the real application, model, and tools.
Long recordings are accelerated. Playback pauses on the final result.

## Render the saved frames

From the repository root:

```bash
make demo
```

The source frames live in `codehamr.cast.gz`: a compressed
[asciicast recording](https://docs.asciinema.org/manual/asciicast/v2/) containing
terminal text, colors, cursor movements, and timing. This keeps the source small
and preserves the animation without storing hundreds of screenshots.

Rendering needs Python 3 on Linux or macOS. Windows users can use WSL.
`render.py` downloads a pinned version of [agg](https://github.com/asciinema/agg)
and JetBrains Mono into the user cache on first use. It checks their SHA256
hashes. Later renders use the cache and need no network, Go build, or API key.
The font size, frame rate, and playback settings are in `render.py`.

## Record the current UI again

```bash
make demo-record
```

This also needs Go, Python 3, and an `openrouter` profile with a working key in
`.codehamr/config.yaml`. Recording uses `qwen/qwen3.8-27b` through that provider
and incurs normal API usage. It does not change your project profiles.

The recorder builds the current application and copies `record/project/` into
a temporary directory. The example starts with failing tests. Qwen receives
the repair task and runs the real tools. The recorder checks that the turn
finished and an untouched copy of the original tests passes before saving the
terminal frames. A failed run leaves the previous recording and GIF available.

The temporary project, credentials, and debug log are removed afterwards.
Only terminal output is saved. The recording is checked for the API key before
it is published. Review a new GIF before committing it.

The example project is intentionally broken. Its failing tests provide the
starting point for the demo; they are not part of codehamr's test suite.
