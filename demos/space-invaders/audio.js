// Space Invaders audio module — synthesizes SFX via WebAudio API.
// Loaded after window.SpaceInvaders = {} is established by index.html.
// Self-contained: no imports/exports, no external sound files.
(function () {
  'use strict';

  var AC = window.AudioContext || window.webkitAudioContext;

  // If WebAudio is unavailable, install no-op stubs and bail.
  if (!AC) {
    window.SpaceInvaders = window.SpaceInvaders || {};
    window.SpaceInvaders.audio = {
      playLaser: function () {},
      playExplosion: function () {},
      playHit: function () {},
    };
    return;
  }

  var ctx = null;
  var noiseBuffer = null;

  // Lazy AudioContext: created + resumed on first play call (which is
  // expected to happen during a user gesture, e.g. keydown in input.js).
  function ensureCtx() {
    try {
      if (!ctx) {
        ctx = new AC();
      }
      if (ctx.state === 'suspended' && typeof ctx.resume === 'function') {
        // resume() returns a Promise — we don't await; play methods return immediately.
        ctx.resume();
      }
      return ctx;
    } catch (e) {
      return null;
    }
  }

  // One-shot white noise buffer, lazily built. ~0.5s is plenty for any burst.
  function getNoiseBuffer(audioCtx) {
    if (noiseBuffer) return noiseBuffer;
    var sampleRate = audioCtx.sampleRate;
    var length = Math.floor(sampleRate * 0.5);
    var buf = audioCtx.createBuffer(1, length, sampleRate);
    var data = buf.getChannelData(0);
    for (var i = 0; i < length; i++) {
      data[i] = Math.random() * 2 - 1;
    }
    noiseBuffer = buf;
    return noiseBuffer;
  }

  // playLaser — short downward pitch swoop, square wave.
  // 880Hz → 220Hz over 80ms, gain 0.15 → 0.
  function playLaser() {
    var c = ensureCtx();
    if (!c) return;
    try {
      var now = c.currentTime;
      var dur = 0.08;

      var osc = c.createOscillator();
      var gain = c.createGain();

      osc.type = 'square';
      osc.frequency.setValueAtTime(880, now);
      osc.frequency.exponentialRampToValueAtTime(220, now + dur);

      gain.gain.setValueAtTime(0.15, now);
      gain.gain.exponentialRampToValueAtTime(0.0001, now + dur);

      osc.connect(gain);
      gain.connect(c.destination);

      osc.start(now);
      osc.stop(now + dur + 0.02);
    } catch (e) {
      // Silently swallow — audio is non-critical.
    }
  }

  // playExplosion — white noise burst through a sweeping low-pass filter.
  // Filter cutoff 1000Hz → 200Hz over 250ms, gain 0.2 → 0.
  function playExplosion() {
    var c = ensureCtx();
    if (!c) return;
    try {
      var now = c.currentTime;
      var dur = 0.25;

      var src = c.createBufferSource();
      src.buffer = getNoiseBuffer(c);

      var filter = c.createBiquadFilter();
      filter.type = 'lowpass';
      filter.Q.value = 1;
      filter.frequency.setValueAtTime(1000, now);
      filter.frequency.exponentialRampToValueAtTime(200, now + dur);

      var gain = c.createGain();
      gain.gain.setValueAtTime(0.2, now);
      gain.gain.exponentialRampToValueAtTime(0.0001, now + dur);

      src.connect(filter);
      filter.connect(gain);
      gain.connect(c.destination);

      src.start(now);
      src.stop(now + dur + 0.02);
    } catch (e) {
      // no-op
    }
  }

  // playHit — short low square thump.
  // 110Hz square, gain 0.2 → 0 over 120ms with a brief attack.
  function playHit() {
    var c = ensureCtx();
    if (!c) return;
    try {
      var now = c.currentTime;
      var dur = 0.12;
      var attack = 0.005;

      var osc = c.createOscillator();
      var gain = c.createGain();

      osc.type = 'square';
      // Slight downward pitch bend gives the thump some weight.
      osc.frequency.setValueAtTime(140, now);
      osc.frequency.exponentialRampToValueAtTime(70, now + dur);

      gain.gain.setValueAtTime(0.0001, now);
      gain.gain.exponentialRampToValueAtTime(0.2, now + attack);
      gain.gain.exponentialRampToValueAtTime(0.0001, now + dur);

      osc.connect(gain);
      gain.connect(c.destination);

      osc.start(now);
      osc.stop(now + dur + 0.02);
    } catch (e) {
      // no-op
    }
  }

  window.SpaceInvaders = window.SpaceInvaders || {};
  window.SpaceInvaders.audio = {
    playLaser: playLaser,
    playExplosion: playExplosion,
    playHit: playHit,
  };
})();
