"""Exercise the piped installer without network calls or writes outside a temp dir."""
import errno
import hashlib
import json
import os
from pathlib import Path
import pty
import select
import signal
import subprocess
import sys
import tempfile
import time
import unittest

INSTALLER = Path(__file__).with_name("install.sh")
BINARY = b'''#!/bin/sh
printf '%s\\n' "$*" >> "$MF_TEST_ROOT/executions"
if [ "${1:-}" = --help ]; then
    printf 'Usage: mf [command]\\n'
else
    printf '0.1.0\\n'
fi
'''


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
        self.home = self.root / "home"
        self.home.mkdir()
        self.environment = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                                GH_TOKEN="fixture-token", MF_INSTALL_DIR=str(self.destination),
                                HOME=str(self.home), SHELL="/bin/zsh",
                                TMPDIR=str(self.temporary_downloads), MF_TEST_ROOT=str(self.root),
                                MF_TEST_OS="Linux", MF_TEST_ARCH="x86_64")
        self.environment.pop("MF_VERSION", None)
        self.environment.pop("MF_TESTING", None)
        self.environment.pop("GITHUB_TOKEN", None)
        self.environment.pop("ZDOTDIR", None)
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
if url.endswith("/releases/latest") or "/releases/tags/" in url:
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
                              capture_output=True, env=self.environment, timeout=15,
                              start_new_session=True)

    def run_interactive(self, answer="\n"):
        """Exercise a curl-style pipe with a separate controlling terminal."""
        environment = dict(self.environment, MF_TEST_INSTALLER=str(INSTALLER.resolve()))
        pid, terminal = pty.fork()
        if pid == 0:
            try:
                os.execve("/bin/sh", ["sh", "-c", 'cat "$MF_TEST_INSTALLER" | sh'], environment)
            finally:
                os._exit(127)
        output = bytearray()
        status = None
        replied = False
        deadline = time.monotonic() + 15
        try:
            while time.monotonic() < deadline:
                ready, _, _ = select.select([terminal], [], [], 0.2)
                if not ready:
                    continue
                try:
                    chunk = os.read(terminal, 65536)
                except OSError as error:
                    if error.errno != errno.EIO:
                        raise
                    break
                if not chunk:
                    break
                output.extend(chunk)
                if b"(Y/n) " in output and not replied:
                    os.write(terminal, answer.encode())
                    replied = True
            else:
                self.fail("installer timed out: " + output.decode(errors="replace"))
            _, status = os.waitpid(pid, 0)
        finally:
            os.close(terminal)
            if status is None:
                os.killpg(pid, signal.SIGKILL)
                os.waitpid(pid, 0)
        return subprocess.CompletedProcess("piped installer", os.waitstatus_to_exitcode(status),
                                           output.decode(errors="replace").replace("\r\n", "\n"))

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
        self.assertFalse((self.root / "executions").exists())
        self.assert_clean()

    def test_missing_asset_fails_before_install(self):
        self.release("mf-linux-arm64")
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing asset mf-linux-amd64", result.stderr)
        self.assertFalse(self.destination.exists())
        self.assert_clean()

    def test_testing_release_requires_explicit_native_opt_in(self):
        tag = "testing-linux-amd64-20261026120000000000-g" + "a" * 40
        release_path = self.root / "release.json"
        release = json.loads(release_path.read_text())
        release.update(tag_name=tag, prerelease=True)
        release_path.write_text(json.dumps(release))
        self.environment["MF_VERSION"] = tag
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("MF_TESTING=1", result.stderr)
        self.assertFalse(self.destination.exists())
        self.environment["MF_TESTING"] = "1"
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Installed mf " + tag, result.stdout)
        self.assertEqual((self.destination / "mf").read_bytes(), BINARY)
        self.assert_clean()

    def test_default_installer_rejects_testing_even_with_opt_in(self):
        release_path = self.root / "release.json"
        release = json.loads(release_path.read_text())
        release.update(tag_name="testing-linux-amd64-20261026120000000000-g" + "a" * 40,
                       prerelease=True)
        release_path.write_text(json.dumps(release))
        self.environment["MF_TESTING"] = "1"
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.destination.exists())

    def test_testing_release_cannot_install_another_platform(self):
        self.environment.update(MF_TESTING="1", MF_VERSION="testing-darwin-arm64-20261026120000000000-g" + "a" * 40)
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.log.exists())
        self.assertFalse(self.destination.exists())

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

    def test_noninteractive_install_does_not_modify_shell_settings(self):
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Add " + str(self.destination) + " to your PATH", result.stdout)
        self.assertNotIn("(Y/n)", result.stdout)
        self.assertEqual(list(self.home.iterdir()), [])
        self.assertTrue(result.stdout.endswith("Usage: mf [command]\n"))
        self.assertEqual((self.root / "executions").read_text(), "--help\n")

    def test_default_yes_adds_safe_zsh_path_once(self):
        profile = self.home / ".zshrc"
        profile.write_text("# existing settings without a trailing newline")
        self.environment.pop("GH_TOKEN")
        for _ in range(2):
            result = self.run_interactive()
            self.assertEqual(result.returncode, 0, result.stdout)
            self.assertIn("Would you like to automatically add 'mf' to your PATH? (Y/n)", result.stdout)
            self.assertIn("Open a new terminal", result.stdout)
            self.assertTrue(result.stdout.endswith("Usage: mf [command]\n"))
            self.assertLess(result.stdout.index("PATH configured"), result.stdout.index("Usage: mf"))
        saved = profile.read_text()
        self.assertTrue(saved.startswith("# existing settings without a trailing newline\n"))
        self.assertEqual(saved.count("# Added by mf installer"), 1)
        # Source twice to verify safe quoting and avoid repeated PATH entries.
        shell = subprocess.run(["sh", "-c", '. "$1"; . "$1"; command -v mf; printf "%s\\n" "$PATH"', "sh", str(profile)],
                               env=self.environment, text=True, capture_output=True, timeout=5)
        self.assertEqual(shell.returncode, 0, shell.stderr)
        lines = shell.stdout.splitlines()
        self.assertEqual(lines[0], str(self.destination / "mf"))
        self.assertEqual(lines[1].split(os.pathsep).count(str(self.destination)), 1)
        self.assert_clean()

    def test_declining_path_setup_preserves_profile(self):
        profile = self.home / ".zshrc"
        profile.write_text("# keep this unchanged")
        result = self.run_interactive("n\n")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("(Y/n)", result.stdout)
        self.assertEqual(profile.read_text(), "# keep this unchanged")
        self.assertTrue((self.destination / "mf").exists())
        self.assertTrue(result.stdout.endswith("Usage: mf [command]\n"))

    def test_existing_path_skips_prompt(self):
        self.environment["PATH"] += os.pathsep + str(self.destination)
        result = self.run_interactive()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertNotIn("(Y/n)", result.stdout)
        self.assertEqual(list(self.home.iterdir()), [])

    def test_bash_configures_interactive_and_login_shells(self):
        self.environment["SHELL"] = "/bin/bash"
        profile = self.home / ".bash_profile"
        profile.write_text("# existing login settings\n")
        result = self.run_interactive("Y\n")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertTrue(profile.read_text().startswith("# existing login settings\n"))
        for path in [profile, self.home / ".bashrc"]:
            self.assertIn("# Added by mf installer", path.read_text())
        self.assertFalse((self.home / ".profile").exists())

    def test_zsh_respects_zdotdir(self):
        folder = self.home / "custom zsh config"
        self.environment["ZDOTDIR"] = str(folder)
        result = self.run_interactive("yes\n")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("# Added by mf installer", (folder / ".zshrc").read_text())
        self.assertFalse((self.home / ".zshrc").exists())

    def test_unsupported_shell_keeps_successful_installation(self):
        self.environment["SHELL"] = "/bin/unknown-shell"
        result = self.run_interactive()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("not supported", result.stdout)
        self.assertIn("PATH manually", result.stdout)
        self.assertTrue((self.destination / "mf").exists())
        self.assertEqual(list(self.home.iterdir()), [])

    def test_unwritable_profile_keeps_successful_installation(self):
        # A directory in place of the file reliably fails even when tests run as root.
        (self.home / ".zshrc").mkdir()
        result = self.run_interactive()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("Could not update shell startup files", result.stdout)
        self.assertIn("PATH manually", result.stdout)
        self.assertTrue((self.destination / "mf").exists())


if __name__ == "__main__":
    unittest.main()
