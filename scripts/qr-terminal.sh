#!/usr/bin/env bash
# Prints an instance's WhatsApp QR code directly in the terminal (local testing).
#
# Usage: ./scripts/qr-terminal.sh <instance-id> [base-url]
# Example: ./scripts/qr-terminal.sh prueba
#          ./scripts/qr-terminal.sh prueba http://localhost:8081
#
# Re-run it when the QR expires (~20s). Sends the apikey header if API_KEY is set.
# Requires: curl, python3 with Pillow.
set -euo pipefail

ID="${1:?Usage: $0 <instance-id> [base-url]}"
BASE="${2:-http://localhost:8081}"

curl -s -X POST "$BASE/v1/instance/$ID/connect" -H "apikey: ${API_KEY:-}" | python3 -I -c '
import sys, json, base64, io
from PIL import Image

resp = json.load(sys.stdin)
b64 = resp.get("base64", "")
if not b64:
    print("connected:", resp.get("connected"), "-", resp.get("message", resp))
    sys.exit(0)

img = Image.open(io.BytesIO(base64.b64decode(b64.split(",", 1)[1]))).convert("L")
w, h = img.size
px = img.load()
dark = lambda x, y: px[x, y] < 128

# Module size: the top-left finder pattern is 7 modules wide.
y0 = next(y for y in range(h) if any(dark(x, y) for x in range(w)))
x0 = next(x for x in range(w) if dark(x, y0))
x1 = x0
while x1 < w and dark(x1, y0):
    x1 += 1
module = (x1 - x0) / 7.0
n = round((w - 2 * x0) / module)

grid = [[dark(int(x0 + (c + 0.5) * module), int(y0 + (r + 0.5) * module)) for c in range(n)] for r in range(n)]
quiet = 2
size = n + 2 * quiet
cell = lambda r, c: 0 <= r - quiet < n and 0 <= c - quiet < n and grid[r - quiet][c - quiet]

# Two QR rows per text line using half blocks; light background for scanners.
out = []
for r in range(0, size, 2):
    line = "\033[30;47m"
    for c in range(size):
        top, bot = cell(r, c), cell(r + 1, c)
        line += "█" if top and bot else "▀" if top else "▄" if bot else " "
    out.append(line + "\033[0m")
print("\n".join(out))
print("Scan with WhatsApp > Linked devices. Re-run if it expires.")
'
