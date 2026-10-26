"""Publish the committed testing branch from this machine, without Actions."""
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time

from release_version import version

REPOSITORY = "ariym/mf-v2"
HOOK = '''#!/bin/sh
# mf local testing release hook
[ "${MF_SKIP_TESTING_RELEASE:-0}" != 1 ] || exit 0
[ "$(git symbolic-ref --quiet --short HEAD)" = testing ] || exit 0
root=$(git rev-parse --show-toplevel) || exit 0
if ! python3 "$root/scripts/testing_release.py"; then
    printf '%s\\n' 'Commit saved; testing publication failed. Retry with make testing-release.' >&2
fi
exit 0
'''


def output(*args):
    return subprocess.check_output(args, text=True).strip()


def install_hook():
    hook = Path(output("git", "rev-parse", "--git-path", "hooks/post-commit"))
    if hook.exists() and hook.read_text() != HOOK:
        raise ValueError(f"Existing hook preserved: {hook}. Add a call to python3 scripts/testing_release.py on branch testing yourself.")
    hook.parent.mkdir(parents=True, exist_ok=True)
    hook.write_text(HOOK)
    hook.chmod(0o755)
    print(f"Installed {hook}. Commits on testing now build and publish this platform synchronously.")


def publish():
    if output("git", "symbolic-ref", "--quiet", "--short", "HEAD") != "testing":
        raise ValueError("Switch to branch testing before publishing a testing release.")
    for tool in ("go", "gh", "tar"):
        if not shutil.which(tool):
            raise ValueError(f"{tool} is required for local testing releases.")
    # One publisher per checkout, including linked worktrees. A second terminal
    # must not push or publish a newer commit while this one is being built.
    lock_path = Path(output("git", "rev-parse", "--git-common-dir")) / "mf-testing-release.lock"
    with lock_path.open("w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError("Another testing build is running. Retry make testing-release after it finishes.") from None
        publish_locked()


def publish_locked():
    commit = output("git", "rev-parse", "HEAD")
    platform, architecture = output("go", "env", "GOHOSTOS", "GOHOSTARCH").splitlines()
    if platform not in ("darwin", "linux") or architecture not in ("amd64", "arm64"):
        raise ValueError(f"Unsupported native platform: {platform}/{architecture}")
    repository = output("gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
    if repository.lower() != REPOSITORY.lower():
        raise ValueError(f"Expected repository {REPOSITORY}, found {repository}.")
    # Do network/Git preparation before compiling; only upload and publish remain
    # once the compiler finishes. Never force-push the testing branch.
    subprocess.run(["git", "fetch", "origin", "--tags"], check=True)
    subprocess.run(["git", "push", "origin", f"{commit}:refs/heads/testing"], check=True)
    stamp = datetime.datetime.fromtimestamp(
        int(output("git", "show", "-s", "--format=%ct", commit)), datetime.timezone.utc
    ).strftime("%Y%m%d%H%M%S%f")
    tag = f"testing-{platform}-{architecture}-{stamp}-g{commit}"
    gh = ["gh", "release"]
    repo = ["--repo", REPOSITORY]
    existing = subprocess.run(gh + ["view", tag, *repo, "--json", "isDraft,isPrerelease"],
                              capture_output=True, text=True)
    draft = existing.returncode == 0
    if draft:
        state = json.loads(existing.stdout)
        if not state["isPrerelease"]:
            raise ValueError(f"Refusing to change a non-testing release: {tag}")
        if not state["isDraft"]:
            print(f"Testing release already published: {tag}")
            return

    with tempfile.TemporaryDirectory(prefix="mf-testing-build-") as temporary:
        source = Path(temporary)
        # Compile exactly the commit, including when unstaged/untracked files exist.
        archive = subprocess.check_output(["git", "archive", "--format=tar", commit])
        subprocess.run(["tar", "-xf", "-", "-C", str(source)], input=archive, check=True)
        declared = re.search(r'var Version = "([^"]+)"',
                             (source / "internal/config/config.go").read_text())
        if declared is None or version("v" + declared[1]) is None:
            raise ValueError("Committed config.Version must be a stable major.minor.patch version.")
        candidates = [version("v" + declared[1])]
        candidates += [parsed for tag_name in output("git", "tag", "--merged", commit).splitlines()
                       if (parsed := version(tag_name)) is not None]
        base_version = ".".join(map(str, max(candidates)))
        asset = source / "dist" / f"mf-{platform}-{architecture}"
        asset.parent.mkdir()
        environment = dict(os.environ, CGO_ENABLED="0", GOOS=platform, GOARCH=architecture,
                           GOWORK="off", GOFLAGS="")
        ldflags = (f"-s -w -X github.com/ariym/mf-v2/internal/config.Version={base_version} "
                   f"-X github.com/ariym/mf-v2/internal/config.TestingRelease={tag}")
        print(f"Building {tag} (compatibility version {base_version})...", flush=True)
        subprocess.run(["go", "build", "-trimpath", "-ldflags", ldflags,
                        "-o", str(asset), "./cmd/mf"], cwd=source, env=environment, check=True)
        compiled_at = time.monotonic()
        checksum = asset.with_suffix(".sha256")
        checksum.write_text(f"{hashlib.sha256(asset.read_bytes()).hexdigest()}  {asset.name}\n")
        installer = asset.parent / "install.sh"
        shutil.copyfile(source / "scripts/install.sh", installer)
        assets = list(map(str, (asset, checksum, installer)))
        if draft:
            subprocess.run(gh + ["upload", tag, *assets, *repo, "--clobber"], check=True)
        else:
            subprocess.run(gh + ["create", tag, *assets, *repo, "--target", commit,
                                "--draft", "--prerelease", "--latest=false", "--title", tag,
                                "--notes", f"Native testing build of {commit} for {platform}/{architecture}."],
                           check=True)
        # No testing tag starts with v: this cannot trigger the stable workflow.
        subprocess.run(gh + ["edit", tag, *repo, "--draft=false", "--prerelease", "--latest=false"], check=True)
        subprocess.run(gh + ["view", tag, *repo, "--json", "url", "--jq", ".url"], check=True)
        elapsed = time.monotonic() - compiled_at
        print(f"Published {elapsed:.1f}s after compilation (target: under 30s).", flush=True)
        if elapsed >= 30:
            print("Upload/GitHub latency exceeded the 30s target; the release is published.", file=sys.stderr)


def main():
    os.chdir(output("git", "rev-parse", "--show-toplevel"))
    if sys.argv[1:] == ["--install-hook"]:
        install_hook()
    elif not sys.argv[1:]:
        publish()
    else:
        raise ValueError("Usage: python3 scripts/testing_release.py [--install-hook]")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        sys.exit(f"mf testing release: {error}")
