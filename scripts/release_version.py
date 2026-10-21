"""Choose a stable release tag; retries of a commit reuse its existing tag."""
import re
import subprocess
import sys

STABLE = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")


def version(tag):
    match = STABLE.fullmatch(tag)
    return tuple(map(int, match.groups())) if match else None


def select_tag(tags, commit_tags, ref_type, ref_name):
    if ref_type == "tag":
        if version(ref_name) is None:
            raise ValueError("Release tags must be stable versions such as v0.1.0.")
        return ref_name
    if ref_type != "branch" or ref_name != "release":
        raise ValueError("Automatic releases must run from the release branch.")
    existing = [tag for tag in commit_tags if version(tag) is not None]
    if existing:
        return max(existing, key=version)
    previous = [parsed for tag in tags if (parsed := version(tag)) is not None]
    if previous:
        major, minor, patch = max(previous)
        next_version = max((0, 1, 0), (major, minor, patch + 1))
    else:
        next_version = (0, 1, 0)
    return "v" + ".".join(map(str, next_version))


def main():
    tags = subprocess.check_output(["git", "tag", "--list"], text=True).splitlines()
    commit_tags = subprocess.check_output(
        ["git", "tag", "--points-at", "HEAD"], text=True).splitlines()
    try:
        print(select_tag(tags, commit_tags, *sys.argv[1:]))
    except ValueError as error:
        sys.exit(str(error))


if __name__ == "__main__":
    main()
