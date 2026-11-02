// Single timing source for the picture.
//
// Loaded as a plain script before film.js, and deliberately DOM-free so tooling
// can evaluate it in a bare context. Every animation beat lives here with a
// name: retiming a scene means editing one line, and nothing can drift out of
// sync because there is only one copy of the number.
//
// Times are in the scene's own local clock, which starts XFADE seconds before the
// scene's entry in SC (the pre-roll exists so a scene's entrance animations play
// during the cross-fade).
const FPS = 30;
const DUR = 83.5;
const XFADE = 0.75;

// scene key → global start (seconds)
const SC = {
  title: 0, problem: 6.0, engines: 14.5, chat: 23.5, peers: 37.5,
  group: 49.5, panels: 60.5, mobile: 70.0, outro: 75.5,
};
const ORD = ['title', 'problem', 'engines', 'chat', 'peers', 'group', 'panels', 'mobile', 'outro'];
const span = (k) => {
  const i = ORD.indexOf(k);
  return [SC[k], i + 1 < ORD.length ? SC[ORD[i + 1]] : DUR];
};
// Scene 0 has no previous scene to cross-fade from, so it gets no pre-roll —
// otherwise its whole entrance plays at negative time and the film opens on a
// half-drawn title. Everything that maps local time to global must agree on
// this, hence one helper instead of an inline `- XFADE`.
const PRE = (scene) => (ORD.indexOf(scene) === 0 ? 0 : XFADE);
// Every animation beat, named. Retiming a scene means editing one line here, and
// src/film.js reads the beats instead of carrying its own copies.
const B = {
  title: {
    bars: [0.10, 0.23, 0.36], word: 0.42, rule: 0.95, cn: 1.15, en: 1.32,
    chips: [1.60, 1.74, 1.88],
  },
  problem: {
    lines: [0.45, 0.87, 1.29, 1.71, 2.13, 2.55],
    rows: [1.50, 2.65, 3.80],
  },
  engines: {
    hub: 0.20, badges: [0.35, 0.97, 1.59, 2.21, 2.83], done: 3.60,
    // a connector starts drawing 0.25 s after its badge appears
    conn: [0.60, 1.22, 1.84, 2.46, 3.08],
  },
  chat: {
    enter: 0.05, push: 4.10, pan: 8.20, back: 12.10,
    think: 1.90, bash: 4.90, task: 9.00,
    caps: [4.60, 8.60, 12.50],
  },
  peers: {
    enter: 0.05, link: 0.50,
    sendSweep: [1.05, 1.75], packetUp: [1.75, 2.60], upLand: 2.60,
    switchView: [4.20, 5.00], packetDown: [5.00, 5.80], bubbleIn: 5.45,
    replySweep: [5.95, 6.60], packetBack: [6.60, 7.60], backLand: 7.55,
    toMechanism: [8.30, 9.20], popover: 8.50, chips: [9.00, 9.22, 9.44],
    caps: [5.20, 8.90],
  },
  group: {
    enter: 0.05, members: [1.30, 1.55, 1.80],
    toAsk: 4.20, askRise: 4.60, askLit: 5.20,
    toCross: 7.00, toastDrop: 7.90, toastLand: 8.55,
    rowPulse: 8.60, badge: 9.20,
    caps: [5.40, 8.40],
  },
  panels: {
    enter: 0.05, cards: [0.25, 0.55, 0.85], hilite: [1.50, 3.20, 4.90],
    zoom: 6.30, diff: [6.90, 7.80],
  },
  mobile: { enter: 0.05, bullets: [0.95, 1.30, 1.65] },
  outro: { enter: 0.05, typed: 0.50, ok: 2.30, big: 3.00, mark: 4.20, sub: 4.90, fade: 6.90 },
};

globalThis.__TL = { FPS, DUR, XFADE, SC, ORD, B };
