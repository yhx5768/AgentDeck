#!/usr/bin/env bash
# Render the film in N slices in parallel, then concatenate them with -c copy.
#
# The frame stepper is dominated by the browser's screenshot encode (~280ms per
# 2880x1620 frame), which is single-threaded per page — so the only real lever is
# running several browsers at once. Slices are rendered independently and joined
# bitstream-wise; render.mjs forces an IDR on each slice's first frame so the
# join is valid.
#
#   CHUNKS=3 ./tools/render-parallel.sh [out.mp4]
set -euo pipefail
cd "$(dirname "$0")/.."
VIDEO=$(pwd)

N=${CHUNKS:-3}
OUT=${1:-out/silent.mp4}
DSF=${DSF:-1.5}
CRF=${CRF:-16}
D=$(dirname "$OUT")
mkdir -p "$D/chunks"
rm -f "$D"/chunks/*.mp4 "$D"/chunks/*.txt

echo "rendering $N slices in parallel (dsf=$DSF crf=$CRF)"
start=$(date +%s)
for i in $(seq 0 $((N - 1))); do
  node tools/render.mjs --chunk "$i/$N" --dsf "$DSF" --crf "$CRF" \
    -o "$D/chunks/part-$i.mp4" >"$D/chunks/log-$i.txt" 2>&1 &
done
wait
for i in $(seq 0 $((N - 1))); do
  tail -1 "$D/chunks/log-$i.txt" | sed "s/^/  slice $i: /"
done

: >"$D/chunks/list.txt"
for i in $(seq 0 $((N - 1))); do echo "file 'part-$i.mp4'" >>"$D/chunks/list.txt"; done
ffmpeg -y -hide_banner -loglevel error -f concat -safe 0 -i "$D/chunks/list.txt" \
  -c copy -movflags +faststart "$OUT"
rm -rf "$D/chunks"

echo "→ $OUT  ($(( ($(date +%s) - start) / 60 ))m$(( ($(date +%s) - start) % 60 ))s)"
