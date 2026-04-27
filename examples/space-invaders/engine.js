// Space Invaders — engine.js (slot B)
// Pure game logic. No DOM access, no event listeners, no audio calls.
// Exposes window.SpaceInvaders.engine = { state, update, reset, render }.

(function () {
  'use strict';

  // ---- Canvas / world constants (canvas is 640x480 per slot A) ----
  var CANVAS_W = 640;
  var CANVAS_H = 480;

  // ---- Enemy grid layout ----
  var ENEMY_COLS = 8;
  var ENEMY_ROWS = 5;
  var ENEMY_W = 28;
  var ENEMY_H = 18;
  var ENEMY_GAP_X = 14;
  var ENEMY_GAP_Y = 12;
  var ENEMY_ORIGIN_X = 60;
  var ENEMY_ORIGIN_Y = 50;

  // ---- Player ----
  var PLAYER_W = 32;
  var PLAYER_H = 16;
  var PLAYER_Y = 440;
  var PLAYER_SPEED = 0.28;       // px/ms
  var FIRE_COOLDOWN_MS = 200;

  // ---- Bullets ----
  var PLAYER_BULLET_VY = -0.55;  // px/ms (upward)
  var PLAYER_BULLET_W = 3;
  var PLAYER_BULLET_H = 10;
  var ENEMY_BULLET_VY = 0.30;    // px/ms (downward)
  var ENEMY_BULLET_W = 3;
  var ENEMY_BULLET_H = 10;

  // ---- Enemy movement: discrete steps for that classic march feel ----
  // Step interval shortens as fewer enemies remain (tension ramp).
  var BASE_STEP_MS = 600;
  var MIN_STEP_MS = 90;
  var ENEMY_STEP_X = 8;          // pixels per horizontal step
  var ENEMY_DROP_Y = 14;         // pixels dropped when reversing direction
  var ENEMY_GROUND_Y = PLAYER_Y; // if any enemy reaches this row, game over

  // ---- Enemy fire ----
  // Average ~one shot per 900ms across the swarm; rolled per-tick on a fixed
  // probability so behavior is independent of frame rate (we scale by dt).
  var ENEMY_FIRE_PER_MS = 1 / 900;
  var MAX_ENEMY_BULLETS = 4;

  // ---- Colors ----
  var COL_BG = '#111';
  var COL_PLAYER = '#4ade80';      // green
  var COL_BULLET_PLAYER = '#fde68a';
  var COL_BULLET_ENEMY = '#f87171';
  var COL_ENEMY = ['#a78bfa', '#60a5fa', '#f472b6']; // 3 types

  // ---- Internal (non-state) tracking ----
  // Kept off the public state object so external readers don't trip over it.
  var internal = {
    fireCooldown: 0,    // ms remaining until player can fire
    enemyDir: 1,        // +1 right, -1 left
    enemyStepAccum: 0,  // ms accumulated toward next step
    pendingDrop: false, // request a drop+reverse on next step
  };

  function buildEnemies() {
    var enemies = [];
    for (var r = 0; r < ENEMY_ROWS; r++) {
      for (var c = 0; c < ENEMY_COLS; c++) {
        // Type by row band: top row type 2, next two type 1, bottom two type 0.
        var type;
        if (r === 0) type = 2;
        else if (r < 3) type = 1;
        else type = 0;
        enemies.push({
          x: ENEMY_ORIGIN_X + c * (ENEMY_W + ENEMY_GAP_X),
          y: ENEMY_ORIGIN_Y + r * (ENEMY_H + ENEMY_GAP_Y),
          w: ENEMY_W,
          h: ENEMY_H,
          alive: true,
          type: type,
        });
      }
    }
    return enemies;
  }

  function freshState() {
    return {
      player: {
        x: (CANVAS_W - PLAYER_W) / 2,
        y: PLAYER_Y,
        w: PLAYER_W,
        h: PLAYER_H,
      },
      enemies: buildEnemies(),
      bullets: [],
      enemyBullets: [],
      score: 0,
      lives: 3,
      gameOver: false,
      win: false,
    };
  }

  function resetInternal() {
    internal.fireCooldown = 0;
    internal.enemyDir = 1;
    internal.enemyStepAccum = 0;
    internal.pendingDrop = false;
  }

  // AABB overlap.
  function aabb(a, b) {
    return (
      a.x < b.x + b.w &&
      a.x + a.w > b.x &&
      a.y < b.y + b.h &&
      a.y + a.h > b.y
    );
  }

  // Count alive enemies and find horizontal extents in one pass.
  function aliveStats(enemies) {
    var n = 0, minX = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (var i = 0; i < enemies.length; i++) {
      var e = enemies[i];
      if (!e.alive) continue;
      n++;
      if (e.x < minX) minX = e.x;
      if (e.x + e.w > maxX) maxX = e.x + e.w;
      if (e.y + e.h > maxY) maxY = e.y + e.h;
    }
    return { n: n, minX: minX, maxX: maxX, maxY: maxY };
  }

  function currentStepInterval(aliveCount, total) {
    if (total <= 0) return BASE_STEP_MS;
    // Linear ramp from BASE down to MIN as the swarm thins.
    var frac = aliveCount / total;
    var ms = MIN_STEP_MS + (BASE_STEP_MS - MIN_STEP_MS) * frac;
    return ms;
  }

  function update(dtMs, inputs) {
    var events = [];
    var s = engine.state;
    if (s.gameOver) return events;
    if (!inputs) inputs = { left: false, right: false, fire: false };

    // Clamp dt to avoid huge steps after a tab pause.
    if (dtMs > 50) dtMs = 50;

    // ---- Player movement ----
    var dx = 0;
    if (inputs.left) dx -= PLAYER_SPEED * dtMs;
    if (inputs.right) dx += PLAYER_SPEED * dtMs;
    s.player.x += dx;
    if (s.player.x < 0) s.player.x = 0;
    if (s.player.x + s.player.w > CANVAS_W) s.player.x = CANVAS_W - s.player.w;

    // ---- Player fire (rate-limited) ----
    if (internal.fireCooldown > 0) internal.fireCooldown -= dtMs;
    if (inputs.fire && internal.fireCooldown <= 0) {
      s.bullets.push({
        x: s.player.x + s.player.w / 2 - PLAYER_BULLET_W / 2,
        y: s.player.y - PLAYER_BULLET_H,
        vy: PLAYER_BULLET_VY,
      });
      internal.fireCooldown = FIRE_COOLDOWN_MS;
      events.push('laser');
    }

    // ---- Bullets (player) advance + cull ----
    for (var i = s.bullets.length - 1; i >= 0; i--) {
      var b = s.bullets[i];
      b.y += b.vy * dtMs;
      if (b.y + PLAYER_BULLET_H < 0) s.bullets.splice(i, 1);
    }

    // ---- Bullets (enemy) advance + cull ----
    for (var j = s.enemyBullets.length - 1; j >= 0; j--) {
      var eb = s.enemyBullets[j];
      eb.y += eb.vy * dtMs;
      if (eb.y > CANVAS_H) s.enemyBullets.splice(j, 1);
    }

    // ---- Enemy stepping ----
    var stats = aliveStats(s.enemies);
    var total = ENEMY_COLS * ENEMY_ROWS;
    var stepInterval = currentStepInterval(stats.n, total);
    internal.enemyStepAccum += dtMs;

    while (internal.enemyStepAccum >= stepInterval && stats.n > 0) {
      internal.enemyStepAccum -= stepInterval;

      if (internal.pendingDrop) {
        // Drop + reverse direction.
        for (var k = 0; k < s.enemies.length; k++) {
          if (s.enemies[k].alive) s.enemies[k].y += ENEMY_DROP_Y;
        }
        internal.enemyDir = -internal.enemyDir;
        internal.pendingDrop = false;
      } else {
        // Horizontal march.
        var step = ENEMY_STEP_X * internal.enemyDir;
        for (var m = 0; m < s.enemies.length; m++) {
          if (s.enemies[m].alive) s.enemies[m].x += step;
        }
        // Recompute extents to decide if next step should drop+reverse.
        stats = aliveStats(s.enemies);
        if (
          (internal.enemyDir > 0 && stats.maxX >= CANVAS_W - 2) ||
          (internal.enemyDir < 0 && stats.minX <= 2)
        ) {
          internal.pendingDrop = true;
        }
      }
    }

    // ---- Enemy fire ----
    if (s.enemyBullets.length < MAX_ENEMY_BULLETS && stats.n > 0) {
      // Roll a frame-rate-independent shot probability.
      var p = ENEMY_FIRE_PER_MS * dtMs;
      if (Math.random() < p) {
        // Pick a random alive enemy from the front rank per column for fairness.
        // Build a list of front-most alive enemies (lowest row per column).
        var front = {};
        for (var n2 = 0; n2 < s.enemies.length; n2++) {
          var e2 = s.enemies[n2];
          if (!e2.alive) continue;
          var col = Math.round((e2.x - ENEMY_ORIGIN_X) / (ENEMY_W + ENEMY_GAP_X));
          if (!front[col] || e2.y > front[col].y) front[col] = e2;
        }
        var keys = Object.keys(front);
        if (keys.length > 0) {
          var pick = front[keys[Math.floor(Math.random() * keys.length)]];
          s.enemyBullets.push({
            x: pick.x + pick.w / 2 - ENEMY_BULLET_W / 2,
            y: pick.y + pick.h,
            vy: ENEMY_BULLET_VY,
          });
        }
      }
    }

    // ---- Collisions: player bullets vs enemies ----
    for (var bi = s.bullets.length - 1; bi >= 0; bi--) {
      var pb = s.bullets[bi];
      var pbBox = { x: pb.x, y: pb.y, w: PLAYER_BULLET_W, h: PLAYER_BULLET_H };
      var hit = false;
      for (var ei = 0; ei < s.enemies.length; ei++) {
        var en = s.enemies[ei];
        if (!en.alive) continue;
        if (aabb(pbBox, en)) {
          en.alive = false;
          s.score += 10;
          events.push('explosion');
          hit = true;
          break;
        }
      }
      if (hit) s.bullets.splice(bi, 1);
    }

    // ---- Collisions: enemy bullets vs player ----
    for (var ebi = s.enemyBullets.length - 1; ebi >= 0; ebi--) {
      var ebb = s.enemyBullets[ebi];
      var ebBox = { x: ebb.x, y: ebb.y, w: ENEMY_BULLET_W, h: ENEMY_BULLET_H };
      if (aabb(ebBox, s.player)) {
        s.enemyBullets.splice(ebi, 1);
        s.lives -= 1;
        events.push('hit');
        if (s.lives <= 0) {
          s.lives = 0;
          s.gameOver = true;
          s.win = false;
        }
      }
    }

    // ---- Lose condition: enemies reach player row ----
    var stats2 = aliveStats(s.enemies);
    if (stats2.n > 0 && stats2.maxY >= ENEMY_GROUND_Y) {
      s.gameOver = true;
      s.win = false;
    }

    // ---- Win condition: all enemies dead ----
    if (stats2.n === 0) {
      s.gameOver = true;
      s.win = true;
    }

    return events;
  }

  function reset() {
    engine.state = freshState();
    resetInternal();
  }

  function render(ctx) {
    var s = engine.state;

    // Background.
    ctx.fillStyle = COL_BG;
    ctx.fillRect(0, 0, CANVAS_W, CANVAS_H);

    // Player.
    ctx.fillStyle = COL_PLAYER;
    ctx.fillRect(s.player.x, s.player.y, s.player.w, s.player.h);
    // Little turret nub on top.
    ctx.fillRect(
      s.player.x + s.player.w / 2 - 2,
      s.player.y - 4,
      4,
      4
    );

    // Enemies.
    for (var i = 0; i < s.enemies.length; i++) {
      var e = s.enemies[i];
      if (!e.alive) continue;
      ctx.fillStyle = COL_ENEMY[e.type] || COL_ENEMY[0];
      ctx.fillRect(e.x, e.y, e.w, e.h);
      // Eye-style detail: two small dark squares so types feel distinct.
      ctx.fillStyle = COL_BG;
      ctx.fillRect(e.x + 6, e.y + 5, 4, 4);
      ctx.fillRect(e.x + e.w - 10, e.y + 5, 4, 4);
    }

    // Player bullets.
    ctx.fillStyle = COL_BULLET_PLAYER;
    for (var b = 0; b < s.bullets.length; b++) {
      var pb = s.bullets[b];
      ctx.fillRect(pb.x, pb.y, PLAYER_BULLET_W, PLAYER_BULLET_H);
    }

    // Enemy bullets.
    ctx.fillStyle = COL_BULLET_ENEMY;
    for (var eb = 0; eb < s.enemyBullets.length; eb++) {
      var ebt = s.enemyBullets[eb];
      ctx.fillRect(ebt.x, ebt.y, ENEMY_BULLET_W, ENEMY_BULLET_H);
    }
  }

  // Public engine object.
  var engine = {
    state: freshState(),
    update: update,
    reset: reset,
    render: render,
  };

  // resetInternal is a no-op the first time (defaults already correct), but
  // calling it keeps the contract clean if something pre-populated internal.
  resetInternal();

  window.SpaceInvaders.engine = engine;
})();
