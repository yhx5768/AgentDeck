#!/usr/bin/env bash
# Contact sheet for reviewing the film the way Code2Video does: look at frames
# as images, not at the timeline. Default samples every 4 seconds into a grid.
#
#   ./tools/contact-sheet.sh out/final.mp4 [every-seconds] [out.png]
set -euo pipefail
cd "$(dirname "$0")/.."
VIDEO=$(pwd)

SRC=${1:-out/final.mp4}
STEP=${2:-4}
OUT=${3:-out/contact-sheet.png}

mkdir -p out/contact
rm -f out/contact/*.png
ffmpeg -y -hide_banner -loglevel error -i "$SRC" -vf "fps=1/$STEP" out/contact/f-%03d.png

count=$(ls out/contact/*.png | wc -l | tr -d ' ')
cols=4
rows=$(( (count + cols - 1) / cols ))
ffmpeg -y -hide_banner -loglevel error -pattern_type glob -i 'out/contact/*.png' \
  -vf "scale=480:270,tile=${cols}x${rows}" -frames:v 1 "$OUT"
echo "$count frames → $OUT (${cols}x${rows})"
