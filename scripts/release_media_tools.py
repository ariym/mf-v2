#!/usr/bin/env python3
"""Build native, self-contained FFmpeg/FFprobe hub archives from pinned source.

The source archive is cached in --build-dir. No host package manager is used.
Release files are created locally; uploading and hub publication are separate.
"""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import tarfile
import urllib.request

VERSION = "7.1.3"
SHA256 = "f0bf043299db9e3caacb435a712fc541fbb07df613c4b893e8b77e67baf3adbe"


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def package(prefix, source, output, repo, version, os_name, arch):
    output.mkdir(parents=True, exist_ok=True)
    for key in ("ffmpeg", "ffprobe"):
        binary = prefix / "bin" / key
        if not binary.is_file() or not os.access(binary, os.X_OK):
            raise ValueError(f"missing executable {binary}")
        name = f"{key}-v{version}-{os_name}-{arch}.tar.gz"
        archive_path = output / name
        with archive_path.open("wb") as stream:
            with gzip.GzipFile(filename="", mode="wb", fileobj=stream, mtime=0) as compressed:
                with tarfile.open(fileobj=compressed, mode="w") as archive:
                    for path, target in [(binary, f"bin/{key}"),
                                         (source / "COPYING.LGPLv2.1", "LICENSE"),
                                         (source / "COPYING.LGPLv3", "LICENSE.LGPLv3")]:
                        info = archive.gettarinfo(str(path), arcname=target)
                        info.uid = info.gid = info.mtime = 0
                        info.uname = info.gname = ""
                        with path.open("rb") as content:
                            archive.addfile(info, content)
        checksum = digest(archive_path)
        manifest = {
            "program_name": key, "version": version,
            "desc": f"Local {key} from FFmpeg {VERSION}; static native build.",
            "git_repo": f"https://github.com/{repo}",
            "artifacts": [{"os": os_name, "arch": arch,
                           "url": f"https://github.com/{repo}/releases/download/media-tools-v{version}/{name}",
                           "sha256": checksum, "entrypoint": f"bin/{key}",
                           "download_bytes": archive_path.stat().st_size}],
        }
        (output / f"{key}.json").write_text(json.dumps(manifest, indent=2) + "\n")
        (output / f"{name}.sha256").write_text(f"{checksum}  {name}\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True, help="GitHub OWNER/REPOSITORY hosting the release")
    parser.add_argument("--version", type=int, default=1, help="immutable hub compatibility version")
    parser.add_argument("--build-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repo) or args.version < 1:
        parser.error("use OWNER/REPOSITORY and a positive version")
    os_name = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system())
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64"}.get(platform.machine())
    if not os_name or not arch:
        parser.error("only native darwin/linux arm64/amd64 builds are supported")
    build = args.build_dir.resolve()
    build.mkdir(parents=True, exist_ok=True)
    archive = build / f"ffmpeg-{VERSION}.tar.xz"
    if not archive.exists():
        partial = archive.with_suffix(".download")
        urllib.request.urlretrieve(f"https://ffmpeg.org/releases/ffmpeg-{VERSION}.tar.xz", partial)
        if digest(partial) != SHA256:
            raise ValueError("FFmpeg source checksum mismatch")
        partial.replace(archive)
    if digest(archive) != SHA256:
        raise ValueError("FFmpeg source checksum mismatch")
    source = build / f"ffmpeg-{VERSION}"
    if not source.exists():
        with tarfile.open(archive) as stream:
            stream.extractall(build, filter="data")
    prefix = build / "runtime"
    configure = ["./configure", f"--prefix={prefix}", "--disable-autodetect",
                 "--disable-shared", "--enable-static", "--disable-network",
                 "--disable-doc", "--disable-debug", "--disable-ffplay"]
    subprocess.run(configure, cwd=source, check=True)
    subprocess.run(["make", "-j", str(min(os.cpu_count() or 2, 8))], cwd=source, check=True)
    subprocess.run(["make", "install"], cwd=source, check=True)
    # Check relocation before packaging; only OS libraries may remain dynamic.
    for key in ("ffmpeg", "ffprobe"):
        binary = prefix / "bin" / key
        libraries = subprocess.check_output(["otool", "-L", str(binary)] if os_name == "darwin"
                                            else ["ldd", str(binary)], text=True)
        if any(marker in libraries for marker in ("/opt/homebrew/", "/usr/local/", str(build))):
            raise ValueError(f"{key} depends on nonportable libraries")
        subprocess.run([str(binary), "-version"], check=True, stdout=subprocess.DEVNULL)
    package(prefix, source, args.output.resolve(), args.repo, args.version, os_name, arch)
    print(args.output.resolve())


if __name__ == "__main__":
    main()
