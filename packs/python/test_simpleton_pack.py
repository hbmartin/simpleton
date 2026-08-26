import json
import pathlib
import subprocess
import sys
import tempfile
import unittest


class ProtocolTest(unittest.TestCase):
    def test_initialize(self) -> None:
        script = pathlib.Path(__file__).with_name("simpleton_pack.py")
        request = json.dumps(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {"protocol_version": "1"},
            }
        )
        result = subprocess.run(
            [sys.executable, str(script)],
            input=request + "\n",
            text=True,
            capture_output=True,
            check=True,
        )
        response = json.loads(result.stdout)
        self.assertEqual(response["result"]["capability"]["language"], "python")

    def test_qualified_symbols_prevent_method_id_collisions(self) -> None:
        script = pathlib.Path(__file__).with_name("simpleton_pack.py")
        with tempfile.TemporaryDirectory() as directory:
            repo = pathlib.Path(directory)
            subprocess.run(["git", "init", "-q", str(repo)], check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.email", "simpleton@example.invalid"], check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.name", "Simpleton Test"], check=True)
            (repo / "sample.py").write_text(
                "class First:\n    def same(self):\n        return 1\n\n"
                "class Second:\n    def same(self):\n        return 2\n",
                encoding="utf-8",
            )
            subprocess.run(["git", "-C", str(repo), "add", "sample.py"], check=True)
            subprocess.run(["git", "-C", str(repo), "commit", "-qm", "sample"], check=True)
            revision = subprocess.check_output(
                ["git", "-C", str(repo), "rev-parse", "HEAD"], text=True
            ).strip()
            requests = [
                {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocol_version": "1"}},
                {
                    "jsonrpc": "2.0",
                    "id": 2,
                    "method": "analyze",
                    "params": {
                        "repository": str(repo),
                        "head_revision": revision,
                        "changed_files": [{"path": "sample.py", "language": "python"}],
                        "budget_ms": 1000,
                    },
                },
            ]
            result = subprocess.run(
                [sys.executable, str(script)],
                input="\n".join(json.dumps(request) for request in requests) + "\n",
                text=True,
                capture_output=True,
                check=True,
            )
            responses = [json.loads(line) for line in result.stdout.splitlines()]
            targets = responses[1]["result"]["targets"]
            self.assertEqual({target["symbol"] for target in targets}, {"First.same", "Second.same"})
            self.assertEqual(len({target["id"] for target in targets}), 2)
            self.assertTrue(all(len(target["id"]) == 16 for target in targets))


if __name__ == "__main__":
    unittest.main()
