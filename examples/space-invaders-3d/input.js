(function () {
  'use strict';

  var SI = (window.SpaceInvaders = window.SpaceInvaders || {});

  var KEY_CODES = ['ArrowLeft', 'ArrowRight', 'Space', 'Enter'];

  var running = false;
  var rafId = 0;
  var lastTs = 0;

  // Held-key state
  var inputs = { left: false, right: false, fire: false };
  // Tracks Enter key state so we only fire restart on the keydown edge
  // (not while the key is held).
  var enterDown = false;

  function clearInputs() {
    inputs.left = false;
    inputs.right = false;
    inputs.fire = false;
    enterDown = false;
  }

  function dispatchEvent(name) {
    var audio = SI.audio;
    if (!audio) return;
    if (name === 'shoot' || name === 'laser' || name === 'fire') {
      if (typeof audio.playLaser === 'function') audio.playLaser();
    } else if (name === 'explosion' || name === 'invader_killed' || name === 'kill') {
      if (typeof audio.playExplosion === 'function') audio.playExplosion();
    } else if (name === 'hit' || name === 'player_hit' || name === 'damage') {
      if (typeof audio.playHit === 'function') audio.playHit();
    }
  }

  function onKeyDown(ev) {
    var code = ev.code;
    if (KEY_CODES.indexOf(code) === -1) return;
    ev.preventDefault();

    if (code === 'ArrowLeft') inputs.left = true;
    else if (code === 'ArrowRight') inputs.right = true;
    else if (code === 'Space') inputs.fire = true;
    else if (code === 'Enter') {
      // Edge detect: only act on the transition from up -> down so a held
      // Enter does not loop-reset the game on every frame.
      if (!enterDown) {
        enterDown = true;
        var engine = SI.engine;
        if (engine && engine.state && engine.state.gameOver) {
          if (typeof engine.reset === 'function') engine.reset();
          if (typeof SI.showGameOver === 'function') SI.showGameOver(false);
        }
      }
    }
  }

  function onKeyUp(ev) {
    var code = ev.code;
    if (KEY_CODES.indexOf(code) === -1) return;
    ev.preventDefault();

    if (code === 'ArrowLeft') inputs.left = false;
    else if (code === 'ArrowRight') inputs.right = false;
    else if (code === 'Space') inputs.fire = false;
    else if (code === 'Enter') enterDown = false;
  }

  function onBlur() {
    clearInputs();
  }

  function frame(now) {
    if (!running) return;

    if (!lastTs) lastTs = now;
    var dt = now - lastTs;
    lastTs = now;
    if (dt < 0) dt = 0;
    if (dt > 100) dt = 100;

    var engine = SI.engine;
    var render = SI.render;

    // Engine update + event dispatch
    var events = null;
    if (engine && typeof engine.update === 'function') {
      events = engine.update(dt, {
        left: inputs.left,
        right: inputs.right,
        fire: inputs.fire,
      });
    }
    if (events && events.length) {
      for (var i = 0; i < events.length; i++) {
        dispatchEvent(events[i]);
      }
    }

    // 3D render (slot D owns canvas + Three.js)
    if (render && typeof render.draw === 'function') render.draw();

    // HUD helpers (optional)
    var state = engine && engine.state;
    if (state) {
      if (typeof SI.updateScore === 'function') SI.updateScore(state.score);
      if (typeof SI.updateLives === 'function') SI.updateLives(state.lives);
      if (state.gameOver && typeof SI.showGameOver === 'function') {
        SI.showGameOver(true);
      }
    }

    rafId = window.requestAnimationFrame(frame);
  }

  function start() {
    if (running) return; // idempotent

    if (!SI.engine || !SI.render || !SI.audio) {
      console.warn(
        '[SpaceInvaders.input] Missing engine/render/audio; refusing to start.'
      );
      return;
    }

    running = true;
    lastTs = 0;
    clearInputs();

    window.addEventListener('keydown', onKeyDown);
    window.addEventListener('keyup', onKeyUp);
    window.addEventListener('blur', onBlur);

    rafId = window.requestAnimationFrame(frame);
  }

  function stop() {
    if (!running && !rafId) {
      // Still strip listeners defensively, then bail.
      window.removeEventListener('keydown', onKeyDown);
      window.removeEventListener('keyup', onKeyUp);
      window.removeEventListener('blur', onBlur);
      clearInputs();
      return;
    }
    running = false;
    if (rafId) {
      window.cancelAnimationFrame(rafId);
      rafId = 0;
    }
    window.removeEventListener('keydown', onKeyDown);
    window.removeEventListener('keyup', onKeyUp);
    window.removeEventListener('blur', onBlur);
    clearInputs();
    lastTs = 0;
  }

  SI.input = { start: start, stop: stop };
})();
