// Space Invaders - input.js
// Slot C: keyboard handling + requestAnimationFrame loop.
// Self-contained, no imports/exports. Attaches to window.SpaceInvaders.input.
(function () {
  'use strict';

  var ns = (window.SpaceInvaders = window.SpaceInvaders || {});

  // Keys we capture (and prevent default on) so arrow keys / space don't
  // scroll the page.
  var CAPTURED_KEYS = {
    ArrowLeft: true,
    ArrowRight: true,
    Space: true,
    Enter: true,
    KeyR: true,
  };

  // Maximum dt per frame in ms. Clamped to handle tab-pause / breakpoint
  // resumes — without this, a 30s pause would advance the simulation by
  // 30s in one tick and fly the player off the map.
  var MAX_DT_MS = 100;

  // Closure-private state.
  var started = false;
  var rafId = 0;
  var lastTs = 0;
  var inputs = { left: false, right: false, fire: false };

  // Restart edge detection — we want a single reset per Enter/R press,
  // not one per frame the key is held.
  var restartHeld = false;

  function getCtx() {
    var canvas = document.getElementById('game');
    if (!canvas || !canvas.getContext) return null;
    return canvas.getContext('2d');
  }

  function dispatchEvents(events) {
    if (!events || !events.length) return;
    var audio = ns.audio;
    if (!audio) return;
    for (var i = 0; i < events.length; i++) {
      var e = events[i];
      if (e === 'laser' && audio.playLaser) audio.playLaser();
      else if (e === 'hit' && audio.playHit) audio.playHit();
      else if (e === 'explosion' && audio.playExplosion) audio.playExplosion();
    }
  }

  function frame(ts) {
    if (!started) return;
    if (!lastTs) lastTs = ts;
    var dt = ts - lastTs;
    lastTs = ts;
    if (dt < 0) dt = 0;
    if (dt > MAX_DT_MS) dt = MAX_DT_MS;

    var engine = ns.engine;
    var ctx = getCtx();

    if (engine && ctx) {
      // Handle restart edge before the engine update so a fresh frame
      // renders post-reset.
      var state = engine.state;
      if (state && state.gameOver) {
        if (restartHeld && engine.reset) {
          engine.reset();
          if (typeof ns.showGameOver === 'function') ns.showGameOver(false);
          // Consume the press so we don't immediately re-fire.
          restartHeld = false;
        }
      }

      var events = engine.update ? engine.update(dt, inputs) : null;
      dispatchEvents(events);
      if (engine.render) engine.render(ctx);

      // Mirror score + gameOver into the index.html UI helpers if present.
      var s = engine.state;
      if (s) {
        if (typeof ns.updateScore === 'function') ns.updateScore(s.score || 0);
        if (s.gameOver && typeof ns.showGameOver === 'function') {
          ns.showGameOver(true);
        }
      }
    }

    rafId = window.requestAnimationFrame(frame);
  }

  function onKeyDown(ev) {
    var code = ev.code;
    if (CAPTURED_KEYS[code]) ev.preventDefault();
    if (code === 'ArrowLeft') inputs.left = true;
    else if (code === 'ArrowRight') inputs.right = true;
    else if (code === 'Space') inputs.fire = true;
    else if (code === 'Enter' || code === 'KeyR') restartHeld = true;
  }

  function onKeyUp(ev) {
    var code = ev.code;
    if (CAPTURED_KEYS[code]) ev.preventDefault();
    if (code === 'ArrowLeft') inputs.left = false;
    else if (code === 'ArrowRight') inputs.right = false;
    else if (code === 'Space') inputs.fire = false;
    else if (code === 'Enter' || code === 'KeyR') restartHeld = false;
  }

  // Lose focus → drop all inputs so we don't strand a key as "held".
  function onBlur() {
    inputs.left = false;
    inputs.right = false;
    inputs.fire = false;
    restartHeld = false;
  }

  function start() {
    if (started) return;
    if (!ns.engine) {
      console.warn('[SpaceInvaders.input] engine not found; aborting start()');
      return;
    }
    if (!ns.audio) {
      console.warn('[SpaceInvaders.input] audio not found; aborting start()');
      return;
    }
    started = true;
    lastTs = 0;
    window.addEventListener('keydown', onKeyDown);
    window.addEventListener('keyup', onKeyUp);
    window.addEventListener('blur', onBlur);
    rafId = window.requestAnimationFrame(frame);
  }

  function stop() {
    if (!started) return;
    started = false;
    if (rafId) {
      window.cancelAnimationFrame(rafId);
      rafId = 0;
    }
    window.removeEventListener('keydown', onKeyDown);
    window.removeEventListener('keyup', onKeyUp);
    window.removeEventListener('blur', onBlur);
    inputs.left = false;
    inputs.right = false;
    inputs.fire = false;
    restartHeld = false;
    lastTs = 0;
  }

  ns.input = {
    start: start,
    stop: stop,
  };
})();
