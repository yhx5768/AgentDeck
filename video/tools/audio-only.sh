#!/usr/bin/env bash
# Re-generate the score and remux it onto the existing video, without
# re-rendering 2505 frames. Use when only tools/music.py changed — iterating on
# the music should not cost 12 minutes.
#
#   ./tools/audio-only.sh [in.mp4]
set -euo pipefail
cd "$(dirname "$0")/.."
VIDEO=$(pwd)

IN=${1:-out/final.mp4}
[ -f "$IN" ] || { echo "no video at $IN"; exit 1; }

DUR=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$IN")
python3 tools/music.py out/score.wav "$DUR"

ffmpeg -y -hide_banner -loglevel error \
  -i "$IN" -i out/score.wav \
  -map 0:v -map 1:a -c:v copy -c:a aac -b:a 192k -ar 48000 -movflags +faststart \
  out/final.remux.mp4
mv out/final.remux.mp4 "$IN"
rm -f out/score.wav

echo
ffprobe -v error -show_entries format=duration,size -show_entries stream=codec_type,codec_name \
  -of default=noprint_wrappers=1 "$IN"
