#!/usr/bin/env python3
"""Verify managed-tool release archives without compiling or downloading FFmpeg."""
import hashlib
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from release_media_tools import package


class ReleaseTests(unittest.TestCase):
    def test_archive_sizes_checksums_and_entrypoints(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            prefix, source, output = root / "runtime", root / "source", root / "output"
            (prefix / "bin").mkdir(parents=True)
            source.mkdir()
            for key in ("ffmpeg", "ffprobe"):
                binary = prefix / "bin" / key
                binary.write_text("#!/bin/sh\nexit 0\n")
                binary.chmod(0o755)
            for name in ("COPYING.LGPLv2.1", "COPYING.LGPLv3"):
                (source / name).write_text("Fixture license\n")
            package(prefix, source, output, "owner/media", 1, "darwin", "arm64")
            for key in ("ffmpeg", "ffprobe"):
                manifest = json.loads((output / f"{key}.json").read_text())
                artifact = manifest["artifacts"][0]
                archive = output / artifact["url"].rsplit("/", 1)[1]
                self.assertEqual(artifact["download_bytes"], archive.stat().st_size)
                self.assertEqual(artifact["sha256"], hashlib.sha256(archive.read_bytes()).hexdigest())
                with tarfile.open(archive) as stream:
                    self.assertEqual(stream.getnames(), [f"bin/{key}", "LICENSE", "LICENSE.LGPLv3"])
                    self.assertEqual(stream.getmember(artifact["entrypoint"]).mode, 0o755)


if __name__ == "__main__":
    unittest.main()
