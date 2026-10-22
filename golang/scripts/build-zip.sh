#!/usr/bin/env bash
# Builds the Lambda package (static Go binary) into terraform/bootstrap.zip.
#
# Mirrors python/scripts/build-zip.py: the binary goes to the zip root with
# the executable bit set (the provided.* runtime executes /var/task/bootstrap
# directly).
set -euo pipefail

cd "$(dirname "$0")/.."

echo "[build-zip] building static binary (linux/amd64)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o worker/bootstrap ./worker

echo "[build-zip] zipping with the executable bit..."
python3 -c "
import zipfile

z = zipfile.ZipFile('terraform/bootstrap.zip', 'w', zipfile.ZIP_DEFLATED)
i = zipfile.ZipInfo('bootstrap')
i.external_attr = 0o755 << 16
with open('worker/bootstrap', 'rb') as f:
    z.writestr(i, f.read())
z.close()
print('[build-zip] done: terraform/bootstrap.zip')
"
