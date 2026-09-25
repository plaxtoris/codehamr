#!/usr/bin/env python3
"""Render saved terminal frames, or record a fresh Qwen run before rendering."""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import tempfile
import time
import urllib.request

MEDIA = Path(__file__).resolve().parent
ROOT = MEDIA.parent
FRAMES = MEDIA / "codehamr.cast.gz"
OUTPUT = MEDIA / "codehamr.gif"
CACHE = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "codehamr-media"
AGG_VERSION = "1.9.0"
AGG_ASSETS = {
    ("Linux", "x86_64"): ("x86_64-unknown-linux-musl", "ddcbf6ca044c8ac3a434dcb9ee89fb9e3be87209982b7c2adb55f782e8f0f390"),
    ("Linux", "aarch64"): ("aarch64-unknown-linux-gnu", "2b4be407b97e00e1c313a41d154ced8fa3d02c560c8f47a0db4950a2576444c9"),
    ("Darwin", "x86_64"): ("x86_64-apple-darwin", "1462150b611d231d2950d10a676303eaeb1019ff330735882aaae09b52e2e1c1"),
    ("Darwin", "arm64"): ("aarch64-apple-darwin", "742b2b6230529b72f310acb835e9479496000f2eabc97b0993cabe1d7fe70171"),
}
FONTS = {
    "Regular": "a0bf60ef0f83c5ed4d7a75d45838548b1f6873372dfac88f71804491898d138f",
    "Bold": "5590990c82e097397517f275f430af4546e1c45cff408bde4255dad142479dcb",
}
PROMPT = "Fix the API's 500 on empty input. Reproduce and test. Summarize in one short sentence without dashes."


def download(url, path, digest):
    """Cache pinned tools outside the repository and verify every use."""
    if path.exists() and hashlib.sha256(path.read_bytes()).hexdigest() == digest:
        return path
    print(f"Downloading {path.name}", flush=True)
    with urllib.request.urlopen(url, timeout=60) as response:
        data = response.read()
    if hashlib.sha256(data).hexdigest() != digest:
        raise RuntimeError(f"Checksum mismatch for {path.name}")
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(".download")
    temporary.write_bytes(data)
    temporary.replace(path)
    return path


def renderer():
    target = (platform.system(), platform.machine())
    if target not in AGG_ASSETS:
        raise RuntimeError("Use Linux, macOS, or WSL to render the demo.")
    asset, digest = AGG_ASSETS[target]
    agg = download(
        f"https://github.com/asciinema/agg/releases/download/v{AGG_VERSION}/agg-{asset}",
        CACHE / f"agg-v{AGG_VERSION}-{asset}", digest,
    )
    agg.chmod(0o755)
    for weight, digest in FONTS.items():
        name = f"JetBrainsMono-{weight}.ttf"
        download(
            f"https://raw.githubusercontent.com/JetBrains/JetBrainsMono/v2.304/fonts/ttf/{name}",
            CACHE / "fonts" / name, digest,
        )
    return agg


def record():
    # PTY imports stay here so saved frames can be rendered without a terminal.
    import codecs
    import fcntl
    import pty
    import select
    import struct
    import termios

    print("Recording a real Qwen run with the local OpenRouter credentials.", flush=True)
    with tempfile.TemporaryDirectory(prefix="codehamr-demo-") as temporary:
        work = Path(temporary)
        binary, setup = work / "codehamr", work / "prepare"
        subprocess.run(["go", "build", "-ldflags", "-X main.version=demo", "-o", str(binary), "./cmd/codehamr"], cwd=ROOT, check=True)
        subprocess.run(["go", "build", "-o", str(setup), "./media/record"], cwd=ROOT, check=True)
        project = work / "project"
        subprocess.run([str(setup), str(ROOT), str(project)], check=True)
        golden_tests = work / "test_api.py"
        golden_tests.write_bytes((project / "test_api.py").read_bytes())
        tests = ["python3", str(golden_tests), "-v"]
        test_environment = dict(os.environ, PYTHONPATH=str(project))
        before = subprocess.run(tests, cwd=project, env=test_environment, capture_output=True)
        if before.returncode == 0 or b"FAIL: test_empty_input" not in before.stderr:
            raise RuntimeError("The demo must start with the expected failing test.")

        # Reject any credential echo before publishing the captured terminal data.
        config_text = (project / ".codehamr/config.yaml").read_text()
        key_line = re.search(r"(?m)^\s+key: (.+)$", config_text).group(1)
        key = key_line.strip("\"'")
        environment = dict(os.environ, TERM="xterm-256color", COLORTERM="truecolor", CODEHAMR_NO_UPDATE_CHECK="1")
        environment.pop("CODEHAMR_URL", None)
        environment.pop("NO_COLOR", None)
        environment["CLICOLOR_FORCE"] = "1"
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 36, 120, 0, 0))
        child = subprocess.Popen([str(binary)], cwd=project, env=environment, stdin=slave, stdout=slave, stderr=slave)
        os.close(slave)
        events, terminal = [], ""
        decoder = codecs.getincrementaldecoder("utf-8")("replace")
        started = time.monotonic()
        log = project / ".codehamr/log.txt"

        def pump(seconds):
            nonlocal terminal
            until = time.monotonic() + seconds
            while time.monotonic() < until:
                if select.select([master], [], [], min(0.05, max(0, until - time.monotonic())))[0]:
                    data = os.read(master, 65536)
                    text = decoder.decode(data)
                    if text:
                        events.append([round(time.monotonic() - started, 4), "o", text])
                        terminal += text
                    if b"\x1b[6n" in data:
                        os.write(master, b"\x1b[1;1R")

        try:
            deadline = time.monotonic() + 10
            while "qwen/qwen3.8-27b" not in terminal and time.monotonic() < deadline:
                pump(0.1)
            if "qwen/qwen3.8-27b" not in terminal:
                raise RuntimeError("The Qwen splash did not appear.")
            pump(1.5)
            for char in PROMPT:
                os.write(master, char.encode())
                pump(0.035)
            os.write(master, b"\r")
            deadline = time.monotonic() + 180
            while time.monotonic() < deadline:
                pump(0.2)
                if child.poll() is not None:
                    raise RuntimeError("The demo process exited before completing the task.")
                if log.exists() and "] turn_end\n" in log.read_text():
                    break
            else:
                raise RuntimeError("Qwen did not finish within three minutes.")
            pump(0.6)
            after = subprocess.run(tests, cwd=project, env=test_environment, capture_output=True)
            if after.returncode != 0:
                detail = after.stderr.decode(errors="replace").replace(key, "[redacted]")
                raise RuntimeError("The original demo tests still fail:\n" + detail)
            clean = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", terminal)
            if not re.search(r"qwen3\.8[^\r\n]*✓", clean):
                raise RuntimeError("The UI did not report a completed turn.")
            if key in terminal or key in json.dumps(events):
                raise RuntimeError("A credential appeared in the recording; nothing was saved.")
            os.write(master, b"\x04")
            child.wait(timeout=5)
            if child.returncode != 0:
                raise RuntimeError("The terminal did not exit cleanly.")
        finally:
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
            os.close(master)

        header = {"version": 2, "width": 120, "height": 36, "title": "codehamr with qwen3.8", "env": {"TERM": "xterm-256color"}}
        content = "\n".join(json.dumps(row, ensure_ascii=False, separators=(",", ":")) for row in [header, *events]) + "\n"
        FRAMES.write_bytes(gzip.compress(content.encode(), compresslevel=9, mtime=0))
        print(f"Saved {len(events)} terminal updates in {FRAMES.stat().st_size:,} bytes. Tests failed before the repair and passed afterwards.", flush=True)


def render(agg):
    with tempfile.TemporaryDirectory(prefix="codehamr-render-") as temporary:
        cast = Path(temporary) / "demo.cast"
        raw = gzip.decompress(FRAMES.read_bytes())
        cast.write_bytes(raw)
        rows = [json.loads(line) for line in raw.splitlines()]
        duration = rows[-1][0]
        gif = Path(temporary) / "demo.gif"
        subprocess.run([
            str(agg), "--quiet", "--font-dir", str(CACHE / "fonts"), "--text-font-family", "JetBrains Mono",
            "--font-size", "14", "--line-height", "1.4", "--theme", "dracula",
            "--fps-cap", "10", "--speed", str(max(1, round(duration / 18, 2))),
            "--last-frame-duration", "4", "--idle-time-limit", "180", str(cast), str(gif),
        ], check=True)
        OUTPUT.write_bytes(gif.read_bytes())
    print(f"Rendered {OUTPUT.relative_to(ROOT)} ({OUTPUT.stat().st_size:,} bytes)")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--record", action="store_true", help="make a real Qwen recording using the configured OpenRouter key")
    args = parser.parse_args()
    agg = renderer()
    if args.record:
        record()
    elif not FRAMES.exists():
        parser.error("saved frames are missing; use --record first")
    render(agg)


if __name__ == "__main__":
    main()
