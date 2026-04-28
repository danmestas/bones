// engine.js — Pure 3D Space Invaders game logic.
// Loaded after index.html sets window.SpaceInvaders = {}.
// Coordinate system: X-right, Y-up, Z-out-of-screen (Three.js RH default).
// Player at (0,0,0). Enemies start centered around z = -8.
// Player bullets travel z-- (vz = -0.02/ms). Enemy bullets travel z++ (vz = +0.015/ms).

(function () {
  'use strict';

  // ---- Tunables -----------------------------------------------------------
  var PLAYER_SPEED = 0.006;        // X units per ms
  var PLAYER_X_CLAMP = 5;          // ±5 on X
  var PLAYER_FIRE_COOLDOWN = 200;  // ms between shots
  var PLAYER_BULLET_VZ = -0.02;    // per ms
  var ENEMY_BULLET_VZ = 0.015;     // per ms
  var MAX_ENEMY_BULLETS = 4;
  var ENEMY_FIRE_PROB_PER_MS = 1 / 900; // dt-scaled probability per ms
  var DT_CLAMP_MS = 50;

  // Swarm grid: 5 rows × 8 cols = 40 enemies.
  var ROWS = 5;
  var COLS = 8;
  var ENEMY_SPACING_X = 1.0;       // X between enemies
  var ENEMY_SPACING_Y = 0.8;       // Y between rows
  var ENEMY_W = 0.7, ENEMY_H = 0.5, ENEMY_D = 0.5;

  // Swarm horizontal bounds (X). Stays within ±5 ish.
  var SWARM_X_MIN = -4.5;
  var SWARM_X_MAX = 4.5;

  // Swarm step pacing.
  var SWARM_STEP_X = 0.25;         // X distance per step
  var SWARM_STEP_Z = 0.4;          // Z drop when bouncing off edge
  var SWARM_STEP_INTERVAL_FULL = 700; // ms when all 40 alive
  var SWARM_STEP_INTERVAL_MIN = 80;   // ms when only a few left

  // Scoring by type.
  var TYPE_SCORE = [10, 20, 30];

  // Bullet sizes for AABB collision.
  var BULLET_W = 0.15, BULLET_H = 0.15, BULLET_D = 0.4;

  // ---- State --------------------------------------------------------------
  var state = makeFreshState();

  function makeFreshState() {
    var s = {
      player: { x: 0, y: 0, z: 0, w: 0.8, h: 0.4, d: 0.6, dir: 0 },
      enemies: [],
      bullets: [],
      enemyBullets: [],
      score: 0,
      lives: 3,
      gameOver: false,
      win: false,
      // Internal bookkeeping (not part of public contract but lives on state).
      _fireCooldown: 0,
      _swarmDir: 1,         // 1 = drifting right, -1 = drifting left
      _swarmStepTimer: 0,
      _pendingDrop: false,  // bounce off edge → drop on next step
    };
    buildSwarm(s);
    return s;
  }

  function buildSwarm(s) {
    s.enemies.length = 0;
    // Center swarm horizontally; place rows at y = 0..2.
    var totalW = (COLS - 1) * ENEMY_SPACING_X;
    var x0 = -totalW / 2;
    for (var r = 0; r < ROWS; r++) {
      // Top row (r=0) → type 2 (30 pts)
      // Middle two rows (r=1,2) → type 1 (20 pts)
      // Bottom two rows (r=3,4) → type 0 (10 pts)
      var type;
      if (r === 0) type = 2;
      else if (r <= 2) type = 1;
      else type = 0;

      // y: top row highest. Spread y across 0..2 for 5 rows → 0, 0.5, 1.0, 1.5, 2.0
      var y = (ROWS - 1 - r) * (2.0 / (ROWS - 1));

      for (var c = 0; c < COLS; c++) {
        s.enemies.push({
          x: x0 + c * ENEMY_SPACING_X,
          y: y,
          z: -8 + r * ENEMY_SPACING_Y * 0.0, // start row coplanar at z ≈ -8
          w: ENEMY_W,
          h: ENEMY_H,
          d: ENEMY_D,
          alive: true,
          type: type,
          col: c,
          row: r,
        });
      }
    }
    // Optional: push back rows slightly so they're not all at exactly the same z.
    for (var i = 0; i < s.enemies.length; i++) {
      var e = s.enemies[i];
      e.z = -8 - (ROWS - 1 - e.row) * 0.2; // top row furthest, bottom closest
    }
  }

  // ---- AABB collision (3D) -----------------------------------------------
  function aabb3(ax, ay, az, aw, ah, ad, bx, by, bz, bw, bh, bd) {
    return (
      Math.abs(ax - bx) * 2 < (aw + bw) &&
      Math.abs(ay - by) * 2 < (ah + bh) &&
      Math.abs(az - bz) * 2 < (ad + bd)
    );
  }

  // ---- Update -------------------------------------------------------------
  function update(dtMs, inputs) {
    var events = [];
    if (state.gameOver) return events;

    var dt = dtMs;
    if (dt > DT_CLAMP_MS) dt = DT_CLAMP_MS;
    if (dt < 0) dt = 0;

    inputs = inputs || {};

    // ---- Player movement -------------------------------------------------
    var move = 0;
    if (inputs.left) move -= 1;
    if (inputs.right) move += 1;
    state.player.dir = move;
    state.player.x += move * PLAYER_SPEED * dt;
    if (state.player.x < -PLAYER_X_CLAMP) state.player.x = -PLAYER_X_CLAMP;
    if (state.player.x > PLAYER_X_CLAMP) state.player.x = PLAYER_X_CLAMP;

    // ---- Player firing ---------------------------------------------------
    if (state._fireCooldown > 0) state._fireCooldown -= dt;
    if (inputs.fire && state._fireCooldown <= 0) {
      state.bullets.push({
        x: state.player.x,
        y: state.player.y + 0.2,
        z: state.player.z - 0.4,
        vx: 0,
        vy: 0,
        vz: PLAYER_BULLET_VZ,
      });
      state._fireCooldown = PLAYER_FIRE_COOLDOWN;
      events.push('laser');
    }

    // ---- Bullet motion ---------------------------------------------------
    var i;
    for (i = state.bullets.length - 1; i >= 0; i--) {
      var b = state.bullets[i];
      b.x += b.vx * dt;
      b.y += b.vy * dt;
      b.z += b.vz * dt;
      // Despawn when off-field (well past furthest enemy).
      if (b.z < -12) state.bullets.splice(i, 1);
    }
    for (i = state.enemyBullets.length - 1; i >= 0; i--) {
      var eb = state.enemyBullets[i];
      eb.x += eb.vx * dt;
      eb.y += eb.vy * dt;
      eb.z += eb.vz * dt;
      // Despawn when past player.
      if (eb.z > 2) state.enemyBullets.splice(i, 1);
    }

    // ---- Swarm stepping --------------------------------------------------
    var aliveCount = 0;
    for (i = 0; i < state.enemies.length; i++) {
      if (state.enemies[i].alive) aliveCount++;
    }

    if (aliveCount === 0) {
      state.win = true;
      state.gameOver = true;
      return events;
    }

    // Step interval scales linearly between MIN and FULL based on aliveCount.
    var aliveFrac = aliveCount / (ROWS * COLS);
    var stepInterval = SWARM_STEP_INTERVAL_MIN +
      (SWARM_STEP_INTERVAL_FULL - SWARM_STEP_INTERVAL_MIN) * aliveFrac;

    state._swarmStepTimer += dt;
    if (state._swarmStepTimer >= stepInterval) {
      state._swarmStepTimer -= stepInterval;
      stepSwarm();
    }

    // ---- Enemy firing ----------------------------------------------------
    if (state.enemyBullets.length < MAX_ENEMY_BULLETS) {
      // Probability scales with dt so it's frame-rate independent.
      // Build per-column front-most-alive map.
      var frontMost = {}; // col -> enemy with greatest z (closest to player)
      for (i = 0; i < state.enemies.length; i++) {
        var ee = state.enemies[i];
        if (!ee.alive) continue;
        var cur = frontMost[ee.col];
        if (!cur || ee.z > cur.z) frontMost[ee.col] = ee;
      }
      // Each column independently rolls; cap total simultaneous bullets.
      var keys = Object.keys(frontMost);
      // Shuffle iteration order so columns aren't biased.
      for (var k = keys.length - 1; k > 0; k--) {
        var j = Math.floor(Math.random() * (k + 1));
        var tmp = keys[k]; keys[k] = keys[j]; keys[j] = tmp;
      }
      for (var ki = 0; ki < keys.length; ki++) {
        if (state.enemyBullets.length >= MAX_ENEMY_BULLETS) break;
        var fm = frontMost[keys[ki]];
        if (Math.random() < ENEMY_FIRE_PROB_PER_MS * dt) {
          state.enemyBullets.push({
            x: fm.x,
            y: fm.y,
            z: fm.z + 0.3,
            vx: 0,
            vy: 0,
            vz: ENEMY_BULLET_VZ,
          });
        }
      }
    }

    // ---- Collisions: player bullets vs enemies ---------------------------
    for (i = state.bullets.length - 1; i >= 0; i--) {
      var pb = state.bullets[i];
      var hit = false;
      for (var ei = 0; ei < state.enemies.length; ei++) {
        var en = state.enemies[ei];
        if (!en.alive) continue;
        if (aabb3(
          pb.x, pb.y, pb.z, BULLET_W, BULLET_H, BULLET_D,
          en.x, en.y, en.z, en.w, en.h, en.d
        )) {
          en.alive = false;
          state.score += TYPE_SCORE[en.type];
          events.push('explosion');
          hit = true;
          break;
        }
      }
      if (hit) state.bullets.splice(i, 1);
    }

    // ---- Collisions: enemy bullets vs player -----------------------------
    for (i = state.enemyBullets.length - 1; i >= 0; i--) {
      var ebh = state.enemyBullets[i];
      if (aabb3(
        ebh.x, ebh.y, ebh.z, BULLET_W, BULLET_H, BULLET_D,
        state.player.x, state.player.y, state.player.z,
        state.player.w, state.player.h, state.player.d
      )) {
        state.enemyBullets.splice(i, 1);
        state.lives -= 1;
        events.push('hit');
        if (state.lives <= 0) {
          state.lives = 0;
          state.gameOver = true;
          state.win = false;
          return events;
        }
      }
    }

    // ---- Lose condition: swarm reaches player line -----------------------
    for (i = 0; i < state.enemies.length; i++) {
      var ec = state.enemies[i];
      if (ec.alive && ec.z >= state.player.z) {
        state.gameOver = true;
        state.win = false;
        return events;
      }
    }

    // ---- Win condition: all enemies dead ---------------------------------
    // (Already checked above when aliveCount === 0; redundant but cheap.)

    return events;
  }

  function stepSwarm() {
    // Determine current X bounds of the alive swarm.
    var minX = Infinity, maxX = -Infinity;
    var anyAlive = false;
    for (var i = 0; i < state.enemies.length; i++) {
      var e = state.enemies[i];
      if (!e.alive) continue;
      anyAlive = true;
      if (e.x < minX) minX = e.x;
      if (e.x > maxX) maxX = e.x;
    }
    if (!anyAlive) return;

    var dropThisStep = state._pendingDrop;
    state._pendingDrop = false;

    if (dropThisStep) {
      // Drop step: move all alive enemies +z (toward player) and reverse dir.
      for (var di = 0; di < state.enemies.length; di++) {
        var ed = state.enemies[di];
        if (ed.alive) ed.z += SWARM_STEP_Z;
      }
      state._swarmDir = -state._swarmDir;
      return;
    }

    // Horizontal step.
    var dx = state._swarmDir * SWARM_STEP_X;
    // Check if this step would push any enemy past the swarm bound.
    var nextMinX = minX + dx;
    var nextMaxX = maxX + dx;
    if (nextMinX < SWARM_X_MIN || nextMaxX > SWARM_X_MAX) {
      // Schedule a drop for the next step; do a partial nudge to the bound this step
      // so movement still feels smooth.
      var nudge = 0;
      if (nextMaxX > SWARM_X_MAX) nudge = SWARM_X_MAX - maxX;
      else if (nextMinX < SWARM_X_MIN) nudge = SWARM_X_MIN - minX;
      for (var ni = 0; ni < state.enemies.length; ni++) {
        var en = state.enemies[ni];
        if (en.alive) en.x += nudge;
      }
      state._pendingDrop = true;
    } else {
      for (var hi = 0; hi < state.enemies.length; hi++) {
        var eh = state.enemies[hi];
        if (eh.alive) eh.x += dx;
      }
    }
  }

  function reset() {
    state = makeFreshState();
    // Re-bind the public state reference.
    window.SpaceInvaders.engine.state = state;
  }

  // ---- Publish ------------------------------------------------------------
  window.SpaceInvaders.engine = {
    state: state,
    update: update,
    reset: reset,
  };
})();
