"""Test native publication with real local Git and fake Go/GitHub; no network."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from testing_release import HOOK

SCRIPT = Path(__file__).with_name("testing_release.py").resolve()


class TestingReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="mf-publish-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.environment = dict(os.environ, PATH=str(self.tools) + os.pathsep + os.environ["PATH"],
                                MF_PUBLISH_TEST=str(self.root), MF_SKIP_TESTING_RELEASE="0")
        self.environment.pop("MF_FAKE_UPLOAD_FAILURE", None)
        self.git("init", "-b", "testing")
        self.git("config", "user.name", "Testing")
        self.git("config", "user.email", "testing@example.invalid")
        self.git("config", "commit.gpgsign", "false")
        self.git("config", "core.hooksPath", str(self.repo / ".git/hooks"))
        config = self.repo / "internal/config/config.go"
        config.parent.mkdir(parents=True)
        config.write_text('var Version = "0.1.0"\n// committed snapshot\n')
        scripts = self.repo / "scripts"
        scripts.mkdir()
        for name in ("testing_release.py", "release_version.py"):
            (scripts / name).write_bytes(SCRIPT.with_name(name).read_bytes())
        (scripts / "install.sh").write_text("#!/bin/sh\n# committed installer\n")
        self.git("add", ".")
        self.git("commit", "-m", "snapshot")
        self.commit = self.git("rev-parse", "HEAD").strip()
        self.git("tag", "v0.1.5")
        self.git("init", "--bare", str(self.root / "origin.git"))
        self.git("remote", "add", "origin", str(self.root / "origin.git"))
        self.executable("go", '''import json, os, pathlib, sys
args = sys.argv[1:]
if args[0] == "env":
    print("darwin\\narm64")
else:
    source = pathlib.Path("internal/config/config.go").read_text()
    assert "committed snapshot" in source and "dirty" not in source
    assert os.environ["CGO_ENABLED"] == "0"
    assert os.environ["GOOS"] == "darwin" and os.environ["GOARCH"] == "arm64"
    binary = pathlib.Path(args[args.index("-o") + 1])
    binary.write_bytes(b"compiled committed snapshot")
    root = pathlib.Path(os.environ["MF_PUBLISH_TEST"])
    (root / "build.json").write_text(json.dumps(args))
''')
        self.executable("gh", '''import hashlib, json, os, pathlib, sys
root = pathlib.Path(os.environ["MF_PUBLISH_TEST"])
args = sys.argv[1:]
with (root / "gh.jsonl").open("a") as log:
    log.write(json.dumps(args) + "\\n")
state = root / "state.json"
if args[:2] == ["repo", "view"]:
    print("ariym/mf-v2")
elif args[:2] == ["release", "view"]:
    if not state.exists(): sys.exit(1)
    if args[args.index("--json") + 1] == "url":
        print("https://github.com/ariym/mf-v2/releases/tag/" + args[2])
    else:
        print(state.read_text())
elif args[:2] in (["release", "create"], ["release", "upload"]):
    if os.environ.get("MF_FAKE_UPLOAD_FAILURE"): sys.exit(1)
    binary, checksum, installer = map(pathlib.Path, args[3:6])
    assert checksum.read_text() == hashlib.sha256(binary.read_bytes()).hexdigest() + "  " + binary.name + "\\n"
    assert "committed installer" in installer.read_text()
    assert "--clobber" in args or ("--draft" in args and "--prerelease" in args and "--latest=false" in args)
    state.write_text(json.dumps({"isDraft": True, "isPrerelease": True}))
elif args[:2] == ["release", "edit"]:
    assert "--draft=false" in args and "--latest=false" in args
    state.write_text(json.dumps({"isDraft": False, "isPrerelease": True}))
else:
    sys.exit("unexpected gh call: " + str(args))
''')

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.repo, env=self.environment,
                                       stderr=subprocess.DEVNULL, text=True)

    def executable(self, name, script):
        path = self.tools / name
        path.write_text(f"#!{sys.executable}\n" + script)
        path.chmod(0o755)

    def run_script(self, *args):
        return subprocess.run([sys.executable, str(SCRIPT), *args], cwd=self.repo,
                              env=self.environment, capture_output=True, text=True, timeout=20)

    def calls(self):
        return [json.loads(line) for line in (self.root / "gh.jsonl").read_text().splitlines()]

    def test_publishes_committed_native_snapshot_and_never_replaces_published_release(self):
        (self.repo / "internal/config/config.go").write_text('var Version = "99.0.0"\n// dirty\n')
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("after compilation", result.stdout)
        self.assertEqual(self.git("rev-parse", "refs/remotes/origin/testing").strip(), self.commit)
        args = json.loads((self.root / "build.json").read_text())
        flags = args[args.index("-ldflags") + 1]
        self.assertIn("config.Version=0.1.5", flags)
        self.assertIn("config.TestingRelease=testing-darwin-arm64-", flags)
        self.assertIn("-g" + self.commit, flags)
        calls = self.calls()
        self.assertEqual([args[1] for args in calls if args[0] == "release"], ["view", "create", "edit", "view"])
        create = next(args for args in calls if args[:2] == ["release", "create"])
        self.assertEqual(create[create.index("--target") + 1], self.commit)
        self.assertEqual(json.loads((self.root / "state.json").read_text())["isDraft"], False)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("already published", result.stdout)
        self.assertEqual(sum(args[1] == "create" for args in self.calls()), 1)

    def test_failed_upload_is_never_published(self):
        self.environment["MF_FAKE_UPLOAD_FAILURE"] = "1"
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(args[:2] == ["release", "edit"] for args in self.calls()))

    def test_existing_draft_resumes_before_publication(self):
        (self.root / "state.json").write_text(json.dumps({"isDraft": True, "isPrerelease": True}))
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any(args[:2] == ["release", "upload"] for args in self.calls()))
        self.assertFalse(any(args[:2] == ["release", "create"] for args in self.calls()))

    def test_hook_installation_preserves_existing_hooks_and_runs_on_commit(self):
        hook = self.repo / ".git/hooks/post-commit"
        hook.write_text("#!/bin/sh\n# existing hook\n")
        result = self.run_script("--install-hook")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Existing hook preserved", result.stderr)
        hook.unlink()
        result = self.run_script("--install-hook")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(hook.read_text(), HOOK)
        self.git("commit", "--allow-empty", "-m", "automatic publication")
        self.assertTrue((self.root / "state.json").exists())

    def test_other_branches_cannot_publish(self):
        self.git("switch", "-c", "other")
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Switch to branch testing", result.stderr)
        self.assertFalse((self.root / "gh.jsonl").exists())


if __name__ == "__main__":
    unittest.main()
