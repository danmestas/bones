// audio.js — Space Invaders 3D SFX synthesizer
// Self-contained WebAudio module. No external sound files.
// Exposes window.SpaceInvaders.audio = { playLaser, playExplosion, playHit }.

(function () {
  'use strict';

  window.SpaceInvaders = window.SpaceInvaders || {};

  var AC = window.AudioContext || window.webkitAudioContext;

  // If WebAudio is entirely unavailable, install no-op stubs and bail.
  if (!AC) {
    window.SpaceInvaders.audio = {
      playLaser: function () {},
      playExplosion: function () {},
      playHit: function () {}
    };
    return;
  }

  var ctx = null;

  function ensureCtx() {
    if (!ctx) {
      ctx = new AC();
    }
    if (ctx.state === 'suspended') {
      // Resume returns a Promise; we don't await — just kick it.
      try { ctx.resume(); } catch (e) { /* no-op */ }
    }
    return ctx;
  }

  // ---------------------------------------------------------------------------
  // playLaser: square wave 880Hz -> 220Hz over 80ms, gain 0.15 -> 0.
  // Classic arcade pew — quick downward pitch swoop.
  // ---------------------------------------------------------------------------
  function playLaser() {
    try {
      var ac = ensureCtx();
      var now = ac.currentTime;
      var dur = 0.08;

      var osc = ac.createOscillator();
      osc.type = 'square';
      osc.frequency.setValueAtTime(880, now);
      osc.frequency.exponentialRampToValueAtTime(220, now + dur);

      var gain = ac.createGain();
      gain.gain.setValueAtTime(0.15, now);
      gain.gain.exponentialRampToValueAtTime(0.0001, now + dur);

      osc.connect(gain).connect(ac.destination);
      osc.start(now);
      osc.stop(now + dur + 0.02);
    } catch (e) {
      // Silently no-op on any WebAudio failure.
    }
  }

  // ---------------------------------------------------------------------------
  // playExplosion: white-noise burst through a lowpass biquad that sweeps
  // 1000Hz -> 200Hz over ~250ms, gain 0.2 -> 0.
  // Optional spatialization: if `x` is provided, pan via StereoPannerNode
  // (or fallback to PannerNode for older browsers/Safari).
  // pan = clamp(x / 5, -1, 1)  (engine X is roughly +/-5).
  // ---------------------------------------------------------------------------
  function playExplosion(x) {
    try {
      var ac = ensureCtx();
      var now = ac.currentTime;
      var dur = 0.25;

      // Generate a short white-noise buffer.
      var bufferSize = Math.floor(ac.sampleRate * dur);
      var noiseBuf = ac.createBuffer(1, bufferSize, ac.sampleRate);
      var data = noiseBuf.getChannelData(0);
      for (var i = 0; i < bufferSize; i++) {
        data[i] = Math.random() * 2 - 1;
      }
      var noise = ac.createBufferSource();
      noise.buffer = noiseBuf;

      // Lowpass sweep 1000 -> 200 Hz.
      var filter = ac.createBiquadFilter();
      filter.type = 'lowpass';
      filter.frequency.setValueAtTime(1000, now);
      filter.frequency.exponentialRampToValueAtTime(200, now + dur);
      filter.Q.value = 1;

      // Volume envelope.
      var gain = ac.createGain();
      gain.gain.setValueAtTime(0.2, now);
      gain.gain.exponentialRampToValueAtTime(0.0001, now + dur);

      // Spatialization. Default center pan if no arg.
      var panValue = 0;
      if (typeof x === 'number' && isFinite(x)) {
        panValue = x / 5;
        if (panValue < -1) panValue = -1;
        if (panValue > 1) panValue = 1;
      }

      var panNode = null;
      if (typeof ac.createStereoPanner === 'function') {
        // Modern path — StereoPannerNode (cheap, exactly what we want).
        panNode = ac.createStereoPanner();
        panNode.pan.setValueAtTime(panValue, now);
      } else if (typeof ac.createPanner === 'function') {
        // Fallback for older Safari etc — full 3D PannerNode positioned
        // along the X axis. Listener is at origin, so positive X = right.
        panNode = ac.createPanner();
        if (panNode.panningModel !== undefined) {
          panNode.panningModel = 'equalpower';
        }
        // Map pan [-1, 1] to a small X offset; keep Y/Z = 0.
        if (typeof panNode.positionX !== 'undefined') {
          panNode.positionX.setValueAtTime(panValue, now);
          panNode.positionY.setValueAtTime(0, now);
          panNode.positionZ.setValueAtTime(0, now);
        } else if (typeof panNode.setPosition === 'function') {
          panNode.setPosition(panValue, 0, 0);
        }
      }

      // Wire graph: noise -> filter -> gain -> [pan?] -> destination.
      noise.connect(filter);
      filter.connect(gain);
      if (panNode) {
        gain.connect(panNode);
        panNode.connect(ac.destination);
      } else {
        gain.connect(ac.destination);
      }

      noise.start(now);
      noise.stop(now + dur + 0.02);
    } catch (e) {
      // Silently no-op on any WebAudio failure.
    }
  }

  // ---------------------------------------------------------------------------
  // playHit: short low square thump, ~120ms, slight downward pitch bend.
  // ---------------------------------------------------------------------------
  function playHit() {
    try {
      var ac = ensureCtx();
      var now = ac.currentTime;
      var dur = 0.12;

      var osc = ac.createOscillator();
      osc.type = 'square';
      osc.frequency.setValueAtTime(110, now);
      osc.frequency.exponentialRampToValueAtTime(70, now + dur);

      var gain = ac.createGain();
      gain.gain.setValueAtTime(0.18, now);
      gain.gain.exponentialRampToValueAtTime(0.0001, now + dur);

      osc.connect(gain).connect(ac.destination);
      osc.start(now);
      osc.stop(now + dur + 0.02);
    } catch (e) {
      // Silently no-op on any WebAudio failure.
    }
  }

  window.SpaceInvaders.audio = {
    playLaser: playLaser,
    playExplosion: playExplosion,
    playHit: playHit
  };
})();
