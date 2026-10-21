import unittest

from release_version import select_tag


class ReleaseVersionTests(unittest.TestCase):
    def test_first_release(self):
        self.assertEqual(select_tag([], [], "branch", "release"), "v0.1.0")

    def test_numeric_patch_increment_ignores_nonstable_tags(self):
        tags = ["v0.1.9", "v0.1.10", "v0.2.0-beta.1", "v01.2.3", "other"]
        self.assertEqual(select_tag(tags, [], "branch", "release"), "v0.1.11")

    def test_continue_new_major_minor(self):
        self.assertEqual(select_tag(["v1.2.0", "v0.9.99"], [], "branch", "release"), "v1.2.1")

    def test_retry_reuses_commit_tag(self):
        self.assertEqual(select_tag(["v0.1.5", "v0.1.6"], ["v0.1.5"], "branch", "release"), "v0.1.5")

    def test_explicit_tag(self):
        self.assertEqual(select_tag(["v0.1.9"], [], "tag", "v0.2.0"), "v0.2.0")
        for tag in ["v0.2", "v0.2.0-rc.1", "v01.2.0", "v0.2.0+build"]:
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                select_tag([], [], "tag", tag)

    def test_other_branches_are_rejected(self):
        with self.assertRaises(ValueError):
            select_tag([], [], "branch", "master")


if __name__ == "__main__":
    unittest.main()
