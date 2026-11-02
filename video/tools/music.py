#!/usr/bin/env python3
"""Synthesize the film's score.

Standard library only (wave/struct/math) — no numpy, no downloaded assets, so the
soundtrack has no licensing question attached to it and the render is
reproducible offline.

First version was a slow ambient pad; it put people to sleep. This one is a
proper upbeat bed: 122 BPM, four-on-the-floor kick with a sidechain duck, a
backbeat snare, off-beat hats, eighth-note bass and a bright plucked arpeggio,
over I–V–vi–IV in D major. Drums stay out under the title and the outro so the
opening and closing read.

Waveforms come from small wavetables rather than per-sample sin() calls — a
naive version spends most of its time in the math module.

Usage: python3 tools/music.py out/score.wav [duration]
"""
import math
import random
import struct
import sys
import wave

SR = 44100
DUR = float(sys.argv[2]) if len(sys.argv) > 2 else 83.5
OUT = sys.argv[1] if len(sys.argv) > 1 else "out/score.wav"
N = int(SR * DUR)

BPM = 122.0
BEAT = 60.0 / BPM
BAR = 4 * BEAT

# Drums are gated so the title and the outro breathe.
DRUM_FROM, DRUM_TO = 6.0, 76.0
# One riser into the first downbeat with drums.
RISERS = [DRUM_FROM]

# I–V–vi–IV in D major: bright, familiar, never resolves awkwardly on a loop.
# (bass root, chord tones for pad/arp)
CHORDS = [
    (146.83, [293.66, 369.99, 440.00, 587.33]),  # D
    (110.00, [329.63, 415.30, 440.00, 554.37]),  # A
    (123.47, [293.66, 369.99, 493.88, 587.33]),  # Bm
    (98.00, [293.66, 392.00, 493.88, 587.33]),   # G
]
# Which pool note each eighth of the bar plays (the pool is the chord tones plus
# two octave-up notes, so the line keeps moving without wandering).
ARP = [0, 2, 1, 3, 2, 0, 3, 1]

TBL_SIZE = 4096


def table(harmonics):
    return [sum(a * math.sin(2 * math.pi * i / TBL_SIZE * h) for h, a in harmonics)
            for i in range(TBL_SIZE)]


TBL_PLUCK = table([(1, 1.0), (2, 0.40), (3, 0.20), (4, 0.09), (5, 0.04)])
TBL_BASS = table([(1, 1.0), (2, 0.32), (3, 0.10)])
TBL_PAD = table([(1, 1.0), (2, 0.20), (3, 0.09), (4, 0.04)])

# Separate buses: the duck from the kick is applied to music/lead only.
dr = [0.0] * N
mus = [0.0] * N
lead = [0.0] * N


def add(bus, start, dur, gen, gain=1.0):
    i0 = int(start * SR)
    i1 = min(N, int((start + dur) * SR))
    if i0 >= N or i1 <= i0:
        return
    for i in range(max(0, i0), i1):
        bus[i] += gen((i - i0) / SR) * gain


def osc(tbl, freq, t):
    return tbl[int((freq * t % 1.0) * TBL_SIZE)]


# ----------------------------------------------------------------- instruments

def kick(start, gain=0.62):
    """Sine with a fast pitch drop plus a click; the low end of the groove."""
    def gen(t):
        ph = 2 * math.pi * (45 * t + (90 / 22) * (1 - math.exp(-22 * t)))
        body = math.sin(ph) * math.exp(-t * 8.5) * (1 - math.exp(-t * 400))
        click = math.exp(-t * 150) * 0.25
        return body + click
    add(dr, start, 0.42, gen, gain)


def hat(start, gain=0.085, open_=False):
    """High-passed noise: successive-sample difference is a cheap HP filter."""
    prev = [0.0]

    def gen(t):
        n = rnd.uniform(-1, 1)
        hp = n - prev[0]
        prev[0] = n
        return hp * math.exp(-t * (16 if open_ else 70))
    rnd = random.Random(int(start * 7919))
    add(dr, start, 0.24 if open_ else 0.09, gen, gain)


def snare(start, gain=0.19):
    rnd = random.Random(int(start * 104729))

    def gen(t):
        n = rnd.uniform(-1, 1)
        tone = math.sin(2 * math.pi * 190 * t) * 0.45
        return (n * 0.8 + tone) * math.exp(-t * 24)
    add(dr, start, 0.26, gen, gain)


def bass(freq, start, dur, gain):
    def gen(t):
        env = (1 - math.exp(-t * 200)) * math.exp(-t * 7.5)
        return osc(TBL_BASS, freq, t) * env
    add(mus, start, dur, gen, gain)


def pluck(freq, start, gain):
    """The arpeggio: short, bright, and what makes it read as 'upbeat'."""
    def gen(t):
        env = (1 - math.exp(-t * 500)) * math.exp(-t * 10.5)
        return osc(TBL_PLUCK, freq, t) * env
    add(lead, start, 0.30, gen, gain)


def pad(tones, start, dur, gain):
    """Sustained bed, deliberately quiet and high so it fills without dragging."""
    def gen(t):
        env = min(1.0, t / 0.5) * max(0.0, (dur - t) / 0.9)
        v = 0.0
        for k, f in enumerate(tones[:3]):
            v += osc(TBL_PAD, f * (1 + 0.0011 * (k + 1)), t)
        return v / 3.0 * env
    add(mus, start, dur, gen, gain)


def riser(start, gain=0.13):
    """Noise + rising tone in the two seconds before a section lands."""
    dur = 2.0
    rnd = random.Random(int(start * 31337))
    lp = [0.0]

    def gen(t):
        lp[0] += (rnd.uniform(-1, 1) - lp[0]) * (0.03 + 0.10 * t / dur)
        x = t / dur
        env = x ** 2.2
        tone = math.sin(2 * math.pi * (220 + 660 * x) * t) * 0.35
        return (lp[0] * 0.9 + tone) * env
    add(mus, start - dur, dur, gen, gain)


def swell(start, gain=0.05):
    """A short breath on each scene cut, so the edit and the score agree."""
    dur = 1.6
    rnd = random.Random(int(start * 1000))
    lp = [0.0]

    def gen(t):
        lp[0] += (rnd.uniform(-1, 1) - lp[0]) * 0.06
        return lp[0] * math.sin(math.pi * t / dur) ** 2
    add(mus, start - dur, dur, gen, gain)


# ------------------------------------------------------------------- arrange

CUTS = [6.0, 14.5, 23.5, 37.5, 49.5, 60.5, 70.0, 75.5]

n_bars = int(DUR / BAR) + 1
for b in range(n_bars):
    t0 = b * BAR
    if t0 >= DUR:
        break
    root, tones = CHORDS[b % 4]
    pool = tones + [tones[0] * 2, tones[1] * 2]
    drums = DRUM_FROM <= t0 < DRUM_TO

    for k in range(8):  # eighth notes
        at = t0 + k * BEAT / 2
        # bass: octave bounce on the "and" of 2 and 4
        bass(root * (2 if k % 4 in (2, 6) else 1), at, BEAT * 0.46, 0.17)
        # arpeggio, with a rest every other bar so it does not machine-gun
        if not (b % 2 == 1 and k == 3):
            pluck(pool[ARP[k]], at, 0.075)
        if drums:
            hat(at + BEAT / 4, 0.05)          # sixteenths
            if k % 2 == 1:
                hat(at, 0.085)                # off-beats

    if drums:
        for k in range(4):
            kick(t0 + k * BEAT, 0.62)
            if k % 2 == 1:
                snare(t0 + k * BEAT, 0.19)
        if b % 4 == 3:
            hat(t0 + 3 * BEAT + BEAT / 2, 0.075, open_=True)
        if b % 8 == 7:  # rising sixteenth fill into the next phrase
            for j in range(4):
                snare(t0 + 3 * BEAT + j * BEAT / 4, 0.09 + 0.035 * j)

    pad(tones, t0, BAR + 0.5, 0.085)

for t in RISERS:
    riser(t)
for t in CUTS:
    swell(t)

# --------------------------------------------------------------------- mix

# Sidechain: dip the music right after each kick so the low end stays clear.
for i in range(N):
    t = i / SR
    d = 1.0
    if DRUM_FROM <= t < DRUM_TO:
        ph = t % BEAT
        if ph < 0.16:
            d = 0.42 + 0.58 * (ph / 0.16) ** 0.6
    dr[i] = dr[i] + mus[i] * d + lead[i] * (0.5 + 0.5 * d)

# fade in / out
fi, fo = 1.0, 3.2
for i in range(N):
    t = i / SR
    g = 1.0
    if t < fi:
        g = t / fi
    if t > DUR - fo:
        g = min(g, max(0.0, (DUR - t) / fo))
    dr[i] *= g

# A gentle limiter, not for loudness war reasons: the kick peaks ~15 dB above
# the bed, so without it the mix has to sit low to avoid clipping and the whole
# thing reads as quiet. Saturating the peaks buys ~3 dB of perceived level.
peak = max(abs(v) for v in dr) or 1.0
drive = 1.8
k = math.tanh(drive)
for i in range(N):
    dr[i] = math.tanh(dr[i] / peak * drive) / k
peak = max(abs(v) for v in dr) or 1.0
scale = 10 ** (-11.5 / 20) / peak  # normalise to about -11.5 dBFS

with wave.open(OUT, "w") as w:
    w.setnchannels(2)
    w.setsampwidth(2)
    w.setframerate(SR)
    frames = bytearray()
    for v in dr:
        s = int(max(-1.0, min(1.0, v * scale)) * 32767)
        frames += struct.pack("<hh", s, s)
    w.writeframes(bytes(frames))

print(f"score → {OUT}  ({DUR:.1f}s, {BPM:.0f} BPM, peak {20*math.log10(peak*scale):.1f} dBFS)")
