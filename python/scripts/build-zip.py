"""Builds the Lambda package (worker.py + dependencies for python3.13).

The result goes to ``terraform/worker.zip`` (referenced by
``aws_lambda_function``).
"""

import shutil
import subprocess
import tempfile
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parent.parent


def main() -> None:
    tmp = Path(tempfile.mkdtemp())
    pkg = tmp / "pkg"
    pkg.mkdir()
    print("[build-zip] installing dependencies (python 3.13)...")
    subprocess.run(
        [
            "docker", "run", "--rm",
            "-v", f"{pkg}:/pkg",
            "python:3.13",
            "pip", "install", "--quiet", "--target", "/pkg", "dbos~=3.3",
        ],
        check=True,
    )
    shutil.copy(PROJECT_ROOT / "worker" / "worker.py", pkg / "worker.py")
    zip_path = Path(
        shutil.make_archive(str(PROJECT_ROOT / "terraform" / "worker"), "zip", str(pkg))
    )
    print(f"[build-zip] done: {zip_path} ({zip_path.stat().st_size / 1e6:.1f} MB)")


if __name__ == "__main__":
    main()
