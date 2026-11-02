#!/usr/bin/env bash
# End-to-end: fixture → screenshots → frames → score → out/final.mp4
#
# Everything it needs is local: the demo server is started on a private port
# with its own data dir, and nothing here touches the dogfooding instance on
# :8321 or the user's real transcripts.
set -euo pipefail
cd "$(dirname "$0")/.."   # → video/
VIDEO=$(pwd)
ROOT=$(cd .. && pwd)      # → worktree root

PORT=${DEMO_PORT:-8323}
BASE="http://127.0.0.1:${PORT}"
TOKEN=agentdeck-demo

echo "== 1/6  workspace fixture"
"$VIDEO/tools/mk-demo-project.sh" >/dev/null
echo "   ok"

echo "== 2/6  build server + seed demo data"
(cd "$ROOT" && go build -o cc-webui .)
python3 "$VIDEO/tools/seed-demo.py"

echo "== 3/6  start demo server on :$PORT"
(cd "$ROOT" && ./cc-webui -config video/demo-config.yaml >/tmp/opencode/demo-server.log 2>&1 &
 echo $! > /tmp/opencode/demo-server.pid)
trap 'kill "$(cat /tmp/opencode/demo-server.pid)" 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null "$BASE/api/sessions?token=$TOKEN"; then break; fi
  sleep 0.5
done
curl -fsS -o /dev/null "$BASE/api/sessions?token=$TOKEN" || { echo "demo server did not come up"; exit 1; }
echo "   up"

echo "== 4/6  capture screenshots"
(cd "$VIDEO" && DEMO_BASE="$BASE" DEMO_TOKEN="$TOKEN" node tools/capture.mjs)

echo "== 5/6  render frames"
# Slices in parallel by default: the stepper is bound by the browser's
# screenshot encode, one thread per page, so N browsers is the only real lever
# (11.7 min serial → 5.2 min at CHUNKS=3 on 4 cores). CHUNKS=1 falls back.
CHUNKS=${CHUNKS:-3}
if [ "$CHUNKS" -gt 1 ]; then
  CHUNKS="$CHUNKS" DSF="${DSF:-1.5}" CRF="${CRF:-16}" \
    "$VIDEO/tools/render-parallel.sh" "$VIDEO/out/silent.mp4"
else
  (cd "$VIDEO" && node tools/render.mjs --dsf "${DSF:-1.5}" --crf "${CRF:-16}" -o out/silent.mp4)
fi

echo "== 6/6  score + mux"
# The score has to match the film exactly; take the duration from the render
# rather than keeping a second copy of it in sync by hand.
FILM_DUR=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$VIDEO/out/silent.mp4")
python3 "$VIDEO/tools/music.py" "$VIDEO/out/score.wav" "$FILM_DUR"
ffmpeg -y -hide_banner -loglevel error \
  -i "$VIDEO/out/silent.mp4" -i "$VIDEO/out/score.wav" \
  -c:v copy -c:a aac -b:a 192k -ar 48000 -movflags +faststart \
  "$VIDEO/out/final.mp4"
rm -f "$VIDEO/out/score.wav"

echo
ffprobe -v error -show_entries format=duration,size -show_entries stream=codec_name,width,height,r_frame_rate \
  -of default=noprint_wrappers=1 "$VIDEO/out/final.mp4"
