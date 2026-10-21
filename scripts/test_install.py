"""Exercise the piped installer without network calls or writes outside a temp dir."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

INSTALLER = Path(__file__).with_name("install.sh")
BINARY = b"#!/bin/sh\nprintf '0.1.0\\n'\n"


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="mf-install-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root / "tools"
        self.bin.mkdir()
        self.destination = self.root / "install with spaces and 'quotes'"
        self.temporary_downloads = self.root / "downloads"
        self.temporary_downloads.mkdir()
        self.log = self.root / "requests.jsonl"
        self.environment = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                                GH_TOKEN="fixture-token", MF_INSTALL_DIR=str(self.destination),
                                TMPDIR=str(self.temporary_downloads), MF_TEST_ROOT=str(self.root),
                                MF_TEST_OS="Linux", MF_TEST_ARCH="x86_64")
        self.environment.pop("MF_VERSION", None)
        self.environment.pop("GITHUB_TOKEN", None)
        self.executable("gh", "#!/bin/sh\nexit 1\n")
        self.executable("uname", "#!/bin/sh\ncase \"$1\" in -s) printf '%s\\n' \"$MF_TEST_OS\" ;; -m) printf '%s\\n' \"$MF_TEST_ARCH\" ;; esac\n")
        self.executable("curl", f"#!{sys.executable}\n" + '''import json, os, pathlib, sys
args = sys.argv[1:]
root = pathlib.Path(os.environ["MF_TEST_ROOT"])
with (root / "requests.jsonl").open("a") as log:
    log.write(json.dumps(args) + "\\n")
if os.environ.get("MF_TEST_DOWNLOAD_FAILURE"):
    sys.exit(22)
url = args[-1]
if url.endswith("/releases/latest") or url.endswith("/releases/tags/v0.1.0"):
    source = root / "release.json"
elif url.endswith("/assets/101"):
    source = root / "binary"
elif url.endswith("/assets/102"):
    source = root / "checksum"
else:
    sys.exit("unexpected download URL: " + url)
pathlib.Path(args[args.index("-o") + 1]).write_bytes(source.read_bytes())
''')
        self.release("mf-linux-amd64")

    def executable(self, name, content):
        target = self.bin / name
        target.write_text(content)
        target.chmod(0o755)

    def release(self, asset, checksum=None):
        release = {"tag_name": "v0.1.0", "assets": [
            {"name": asset, "id": 101}, {"name": asset + ".sha256", "id": 102}]}
        (self.root / "release.json").write_text(json.dumps(release))
        (self.root / "binary").write_bytes(BINARY)
        digest = checksum or hashlib.sha256(BINARY).hexdigest()
        (self.root / "checksum").write_text(f"{digest}  {asset}\n")

    def run_installer(self):
        return subprocess.run(["sh"], input=INSTALLER.read_text(), text=True,
                              capture_output=True, env=self.environment, timeout=15)

    def assert_clean(self):
        self.assertEqual(list(self.temporary_downloads.iterdir()), [])
        if self.destination.exists():
            self.assertEqual(list(self.destination.glob(".mf.*")), [])

    def test_platform_and_architecture_selection(self):
        for system, machine, asset in [
                ("Darwin", "x86_64", "mf-darwin-amd64"),
                ("Darwin", "arm64", "mf-darwin-arm64"),
                ("Linux", "x86_64", "mf-linux-amd64"),
                ("Linux", "aarch64", "mf-linux-arm64")]:
            with self.subTest(system=system, machine=machine):
                self.environment.update(MF_TEST_OS=system, MF_TEST_ARCH=machine)
                self.release(asset)
                result = self.run_installer()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual((self.destination / "mf").read_bytes(), BINARY)
                self.assertTrue(os.access(self.destination / "mf", os.X_OK))
                self.assertIn("Installed mf v0.1.0", result.stdout)
                self.assert_clean()
        requests = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertTrue(all("Authorization: Bearer fixture-token" in args for args in requests))
        self.assertTrue(all("--proto" in args and "=https" in args for args in requests))
        self.assertEqual(len(requests), 12)

    def test_checksum_failure_keeps_existing_installation(self):
        self.destination.mkdir()
        existing = self.destination / "mf"
        existing.write_bytes(b"previous executable")
        self.release("mf-linux-amd64", checksum="0" * 64)
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("verification failed", result.stderr)
        self.assertEqual(existing.read_bytes(), b"previous executable")
        self.assert_clean()

    def test_missing_asset_fails_before_install(self):
        self.release("mf-linux-arm64")
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing asset mf-linux-amd64", result.stderr)
        self.assertFalse(self.destination.exists())
        self.assert_clean()

    def test_download_failure_explains_private_access(self):
        self.environment["MF_TEST_DOWNLOAD_FAILURE"] = "1"
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("GH_TOKEN", result.stderr)
        self.assertFalse(self.destination.exists())
        self.assert_clean()

    def test_unsupported_platform_and_architecture(self):
        for key, value in [("MF_TEST_OS", "Windows_NT"), ("MF_TEST_ARCH", "riscv64")]:
            with self.subTest(key=key):
                previous = self.environment[key]
                self.environment[key] = value
                result = self.run_installer()
                self.environment[key] = previous
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Unsupported", result.stderr)
                self.assertFalse(self.log.exists())
                self.assertFalse(self.destination.exists())

    def test_public_download_without_token(self):
        self.environment.pop("GH_TOKEN")
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        requests = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertFalse(any("Authorization:" in arg for args in requests for arg in args))
        self.assert_clean()

    def test_pinned_release(self):
        self.environment["MF_VERSION"] = "v0.1.0"
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        requests = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertTrue(requests[0][-1].endswith("/releases/tags/v0.1.0"))
        self.assertEqual((self.destination / "mf").read_bytes(), BINARY)
        self.assert_clean()

    def test_invalid_pinned_version_fails_before_network(self):
        for version in ["0.1.0", "v0.1.0-rc.1", "../../latest", "v01.1.0"]:
            with self.subTest(version=version):
                self.environment["MF_VERSION"] = version
                result = self.run_installer()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("MF_VERSION", result.stderr)
                self.assertFalse(self.log.exists())
                self.assertFalse(self.destination.exists())

    def test_pinned_release_must_match_response(self):
        self.environment["MF_VERSION"] = "v0.1.0"
        release = json.loads((self.root / "release.json").read_text())
        release["tag_name"] = "v0.1.1"
        (self.root / "release.json").write_text(json.dumps(release))
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("requested v0.1.0, received v0.1.1", result.stderr)
        self.assertFalse(self.destination.exists())
        self.assert_clean()

    def test_draft_and_prerelease_are_rejected(self):
        for flag in ["draft", "prerelease"]:
            with self.subTest(flag=flag):
                self.release("mf-linux-amd64")
                release = json.loads((self.root / "release.json").read_text())
                release[flag] = True
                (self.root / "release.json").write_text(json.dumps(release))
                result = self.run_installer()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("published and stable", result.stderr)
                self.assertFalse(self.destination.exists())
                self.assert_clean()

    def test_alternate_credentials(self):
        self.environment.pop("GH_TOKEN")
        for source in ["GITHUB_TOKEN", "gh"]:
            with self.subTest(source=source):
                if self.log.exists():
                    self.log.unlink()
                if source == "GITHUB_TOKEN":
                    self.environment["GITHUB_TOKEN"] = "alternate-token"
                else:
                    self.environment.pop("GITHUB_TOKEN", None)
                    self.executable("gh", "#!/bin/sh\nprintf alternate-token\n")
                result = self.run_installer()
                self.assertEqual(result.returncode, 0, result.stderr)
                requests = [json.loads(line) for line in self.log.read_text().splitlines()]
                self.assertTrue(all("Authorization: Bearer alternate-token" in args for args in requests))
                self.assert_clean()

    def test_wrong_checksum_filename_is_rejected(self):
        (self.root / "checksum").write_text(hashlib.sha256(BINARY).hexdigest() + "  another-file\n")
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("wrong executable", result.stderr)
        self.assertFalse(self.destination.exists())
        self.assert_clean()

    def test_malformed_release_is_rejected(self):
        (self.root / "release.json").write_text("not JSON")
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("mf installer:", result.stderr)
        self.assertFalse(self.destination.exists())
        self.assert_clean()


if __name__ == "__main__":
    unittest.main()
