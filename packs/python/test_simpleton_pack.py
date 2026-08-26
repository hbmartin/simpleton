import json
import pathlib
import subprocess
import sys
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


if __name__ == "__main__":
    unittest.main()
