import pathlib
import unittest


class CIWorkflowTest(unittest.TestCase):
    def test_required_macos_job_uses_intel_runner(self):
        repo_root = pathlib.Path(__file__).resolve().parents[2]
        workflow = (repo_root / ".github/workflows/ci.yml").read_text()

        self.assertIn("macos-15-intel", workflow)
        self.assertNotIn("os: [ubuntu-latest, macos-latest", workflow)


if __name__ == "__main__":
    unittest.main()
