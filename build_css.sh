#!/usr/bin/env bash
# Regenerates server/wwwroot/assets/app.css from web/app.src.css.
#
# This is NOT part of the release build. `go build ./cmd` and build_all.sh stay
# the only thing needed to ship, because the generated stylesheet is committed
# like any other asset in wwwroot. Node is required only here, only when the
# markup's class names change.
#
# Run it after editing any file under server/wwwroot/ that adds or removes a
# Tailwind class, then commit the regenerated app.css alongside the HTML.
set -euo pipefail

cd "$(dirname "$0")"

SRC="web/app.src.css"
OUT="server/wwwroot/assets/app.css"
CFG="web/tailwind.config.js"

for f in "$SRC" "$CFG"; do
  [ -f "$f" ] || { echo "build_css: missing $f" >&2; exit 1; }
done

# Tailwind 3 — the play CDN the pages used to load served v3, and v4 renames
# enough utilities that a bump is a markup change, not a build-script change.
if [ -x node_modules/.bin/tailwindcss ]; then
  TW=(node_modules/.bin/tailwindcss)
elif command -v tailwindcss >/dev/null 2>&1; then
  TW=(tailwindcss)
elif command -v npx >/dev/null 2>&1; then
  TW=(npx -y tailwindcss@3)
else
  echo "build_css: need the tailwindcss v3 CLI (or npx) on PATH" >&2
  exit 1
fi

"${TW[@]}" -c "$CFG" -i "$SRC" -o "$OUT" --minify

printf 'build_css: wrote %s (%s bytes)\n' "$OUT" "$(wc -c < "$OUT")"

# A class the scanner never saw produces no rule and fails silently in the
# browser, so name the one construct that defeats it rather than trusting luck.
if grep -rnE 'class="[^"]*\$\{[^}]*\}[^"]*"' server/wwwroot/*.html \
   | grep -vE '\$\{(boxClass|labelClass|valueClass|name|escapeHtml)' >/dev/null 2>&1; then
  echo "build_css: note — a class attribute interpolates a variable; make sure the" >&2
  echo "           full class string still appears literally somewhere in the file." >&2
fi
