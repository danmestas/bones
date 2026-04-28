(function () {
  'use strict';

  // Slot-D: GPU/3D rendering for Space Invaders.
  // Consumes window.SpaceInvaders.engine.state and exposes
  // window.SpaceInvaders.render = { init, draw, dispose }.

  // ----------------------------------------------------------------
  // Module-private state. All persistent GPU resources live here.
  // ----------------------------------------------------------------
  var renderer = null;
  var scene = null;
  var camera = null;
  var ambientLight = null;
  var directionalLight = null;
  var swarmPointLight = null;

  var playerMesh = null;
  var enemyMeshes = [];          // length 40 (5 rows x 8 cols)
  var playerBulletMeshes = [];   // length PLAYER_BULLET_POOL
  var enemyBulletMeshes = [];    // length ENEMY_BULLET_POOL
  var starfield = null;
  var groundGrid = null;

  // Shared resources we own (so dispose can free them).
  var ownedGeometries = [];
  var ownedMaterials = [];

  // Pool sizes — track the engine's caps. If state.bullets.length
  // exceeds the pool, we render the first N (the visible cap) and
  // silently drop the rest rather than allocating per-frame.
  var PLAYER_BULLET_POOL = 16;
  var ENEMY_BULLET_POOL = 4;
  var ENEMY_ROWS = 5;
  var ENEMY_COLS = 8;
  var ENEMY_COUNT = ENEMY_ROWS * ENEMY_COLS;

  // Color palette for enemy types 0/1/2.
  var ENEMY_COLORS = [0xff5577, 0xffcc44, 0x55ff88];

  // Track pixel ratio so we can react to monitor changes if needed.
  var lastPixelRatio = 1;

  // ----------------------------------------------------------------
  // Helpers
  // ----------------------------------------------------------------
  function trackGeometry(g) {
    ownedGeometries.push(g);
    return g;
  }
  function trackMaterial(m) {
    ownedMaterials.push(m);
    return m;
  }

  function getEngineState() {
    var SI = window.SpaceInvaders;
    if (!SI || !SI.engine || !SI.engine.state) return null;
    return SI.engine.state;
  }

  // ----------------------------------------------------------------
  // Scene construction (called once from init).
  // ----------------------------------------------------------------
  function buildPlayer() {
    var geom = trackGeometry(new THREE.BoxGeometry(0.8, 0.4, 0.6));
    var mat = trackMaterial(new THREE.MeshPhongMaterial({
      color: 0x33ccff,
      emissive: 0x002233,
      shininess: 80,
      specular: 0x99ddff,
    }));
    var mesh = new THREE.Mesh(geom, mat);
    mesh.position.set(0, 0.5, 0);
    return mesh;
  }

  function buildEnemy(type) {
    // Different small geometries per row range for visual variety.
    var geom;
    if (type === 0) {
      geom = new THREE.IcosahedronGeometry(0.32, 0);
    } else if (type === 1) {
      geom = new THREE.OctahedronGeometry(0.34, 0);
    } else {
      geom = new THREE.BoxGeometry(0.55, 0.4, 0.5);
    }
    trackGeometry(geom);
    var color = ENEMY_COLORS[type % ENEMY_COLORS.length];
    var mat = trackMaterial(new THREE.MeshPhongMaterial({
      color: color,
      emissive: color,
      emissiveIntensity: 0.25,
      shininess: 60,
    }));
    return new THREE.Mesh(geom, mat);
  }

  function buildPlayerBullet() {
    var geom = trackGeometry(new THREE.CylinderGeometry(0.06, 0.06, 0.4, 8));
    // Cylinders default to Y-axis; rotate so they extend along Z (travel direction).
    geom.rotateX(Math.PI / 2);
    var mat = trackMaterial(new THREE.MeshPhongMaterial({
      color: 0x66ffff,
      emissive: 0x33ddee,
      emissiveIntensity: 0.9,
      shininess: 100,
    }));
    var mesh = new THREE.Mesh(geom, mat);
    mesh.visible = false;
    return mesh;
  }

  function buildEnemyBullet() {
    var geom = trackGeometry(new THREE.BoxGeometry(0.18, 0.18, 0.4));
    var mat = trackMaterial(new THREE.MeshPhongMaterial({
      color: 0xff5522,
      emissive: 0xff2200,
      emissiveIntensity: 0.8,
      shininess: 80,
    }));
    var mesh = new THREE.Mesh(geom, mat);
    mesh.visible = false;
    return mesh;
  }

  function buildStarfield() {
    var STAR_COUNT = 1500;
    var positions = new Float32Array(STAR_COUNT * 3);
    for (var i = 0; i < STAR_COUNT; i++) {
      // Spread stars in a wide slab behind the swarm.
      positions[i * 3 + 0] = (Math.random() - 0.5) * 80;
      positions[i * 3 + 1] = (Math.random() - 0.5) * 40 + 5;
      positions[i * 3 + 2] = -20 - Math.random() * 60;
    }
    var geom = trackGeometry(new THREE.BufferGeometry());
    geom.setAttribute('position', new THREE.BufferAttribute(positions, 3));
    var mat = trackMaterial(new THREE.PointsMaterial({
      color: 0xffffff,
      size: 0.08,
      sizeAttenuation: true,
      transparent: true,
      opacity: 0.85,
    }));
    return new THREE.Points(geom, mat);
  }

  function buildGroundGrid() {
    // Subtle grid behind the swarm to give depth cues.
    var grid = new THREE.GridHelper(40, 40, 0x224477, 0x112244);
    grid.position.set(0, -1.5, -10);
    // GridHelper materials are owned internally by THREE; track them so we dispose them.
    if (grid.material) {
      if (Array.isArray(grid.material)) {
        for (var i = 0; i < grid.material.length; i++) ownedMaterials.push(grid.material[i]);
      } else {
        ownedMaterials.push(grid.material);
      }
    }
    if (grid.geometry) ownedGeometries.push(grid.geometry);
    return grid;
  }

  // ----------------------------------------------------------------
  // Public API
  // ----------------------------------------------------------------
  function init(canvas) {
    try {
      if (typeof THREE === 'undefined' || !THREE) return false;
      if (!canvas) return false;

      // Renderer — bind to the existing canvas.
      renderer = new THREE.WebGLRenderer({
        canvas: canvas,
        antialias: true,
        powerPreference: 'high-performance',
      });
      lastPixelRatio = window.devicePixelRatio || 1;
      renderer.setPixelRatio(lastPixelRatio);
      renderer.setSize(canvas.width, canvas.height, false);
      renderer.setClearColor(0x000010, 1);

      // Scene + camera.
      scene = new THREE.Scene();
      scene.background = new THREE.Color(0x000010);
      // Light atmospheric fog so distant stars/swarm fade rather than pop.
      scene.fog = new THREE.Fog(0x000010, 8, 60);

      var aspect = canvas.width / Math.max(1, canvas.height);
      camera = new THREE.PerspectiveCamera(60, aspect, 0.1, 100);
      camera.position.set(0, 1.5, 5);
      camera.lookAt(0, 1, -4);

      // Lights.
      ambientLight = new THREE.AmbientLight(0xffffff, 0.3);
      scene.add(ambientLight);

      directionalLight = new THREE.DirectionalLight(0xffffff, 1.0);
      directionalLight.position.set(0, 1.5, 5); // co-located with camera
      scene.add(directionalLight);

      // Colored point light near the swarm to make explosions/colors pop.
      swarmPointLight = new THREE.PointLight(0x88aaff, 0.6, 30, 2);
      swarmPointLight.position.set(0, 2, -7);
      scene.add(swarmPointLight);

      // Player.
      playerMesh = buildPlayer();
      scene.add(playerMesh);

      // Swarm: 5 rows x 8 cols. Row 0 is the back (highest type), row 4 the front.
      for (var i = 0; i < ENEMY_COUNT; i++) {
        var row = Math.floor(i / ENEMY_COLS);
        // Type maps roughly: top rows = type 0 (toughest), middle = 1, bottom = 2.
        var type;
        if (row <= 0) type = 0;
        else if (row <= 2) type = 1;
        else type = 2;
        var m = buildEnemy(type);
        m.visible = false;
        scene.add(m);
        enemyMeshes.push(m);
      }

      // Bullet pools.
      for (var pi = 0; pi < PLAYER_BULLET_POOL; pi++) {
        var pb = buildPlayerBullet();
        scene.add(pb);
        playerBulletMeshes.push(pb);
      }
      for (var ei = 0; ei < ENEMY_BULLET_POOL; ei++) {
        var eb = buildEnemyBullet();
        scene.add(eb);
        enemyBulletMeshes.push(eb);
      }

      // Polish: starfield + ground grid.
      starfield = buildStarfield();
      scene.add(starfield);

      groundGrid = buildGroundGrid();
      scene.add(groundGrid);

      return true;
    } catch (err) {
      // Renderer creation can throw on WebGL-disabled environments.
      try { console.error('render.init failed:', err); } catch (_) {}
      // Best-effort cleanup of anything we partially built.
      try { dispose(); } catch (_) {}
      return false;
    }
  }

  function syncPlayer(state) {
    if (!playerMesh) return;
    var p = state.player;
    if (!p) {
      playerMesh.visible = false;
      return;
    }
    playerMesh.visible = (p.alive !== false);
    var x = (typeof p.x === 'number') ? p.x : 0;
    var y = (typeof p.y === 'number') ? p.y : 0.5;
    var z = (typeof p.z === 'number') ? p.z : 0;
    playerMesh.position.set(x, y, z);
    if (typeof p.dir === 'number') {
      // Treat dir as a yaw angle (radians) — engine.js may set it for tilt.
      playerMesh.rotation.y = p.dir;
    }
  }

  function syncEnemies(state) {
    var enemies = state.enemies || [];
    for (var i = 0; i < ENEMY_COUNT; i++) {
      var mesh = enemyMeshes[i];
      var e = enemies[i];
      if (!e) {
        mesh.visible = false;
        continue;
      }
      var alive = (e.alive !== false);
      mesh.visible = alive;
      if (!alive) continue;
      var x = (typeof e.x === 'number') ? e.x : 0;
      var y = (typeof e.y === 'number') ? e.y : 1;
      var z = (typeof e.z === 'number') ? e.z : -8;
      mesh.position.set(x, y, z);
      // Gentle bob so the swarm feels alive even between engine ticks.
      mesh.rotation.y += 0.01;
    }
  }

  function syncBulletPool(pool, bullets) {
    var n = bullets ? bullets.length : 0;
    var cap = pool.length;
    var visible = n < cap ? n : cap; // overflow drops silently — see report.
    for (var i = 0; i < cap; i++) {
      var mesh = pool[i];
      if (i < visible) {
        var b = bullets[i];
        if (!b) {
          mesh.visible = false;
          continue;
        }
        mesh.visible = true;
        var x = (typeof b.x === 'number') ? b.x : 0;
        var y = (typeof b.y === 'number') ? b.y : 1;
        var z = (typeof b.z === 'number') ? b.z : 0;
        mesh.position.set(x, y, z);
      } else {
        mesh.visible = false;
      }
    }
  }

  function syncBullets(state) {
    syncBulletPool(playerBulletMeshes, state.playerBullets || state.bullets || []);
    syncBulletPool(enemyBulletMeshes, state.enemyBullets || []);
  }

  function draw() {
    if (!renderer || !scene || !camera) return;
    var state = getEngineState();
    if (state) {
      syncPlayer(state);
      syncEnemies(state);
      syncBullets(state);
    }
    // Slow drift on the starfield so it isn't completely static.
    if (starfield) starfield.rotation.z += 0.0005;
    renderer.render(scene, camera);
  }

  function dispose() {
    // Drop scene children references so GC can reclaim mesh objects.
    if (scene) {
      while (scene.children.length > 0) {
        scene.remove(scene.children[0]);
      }
    }

    // Free GPU buffers we own.
    for (var gi = 0; gi < ownedGeometries.length; gi++) {
      try { ownedGeometries[gi].dispose(); } catch (_) {}
    }
    ownedGeometries.length = 0;

    for (var mi = 0; mi < ownedMaterials.length; mi++) {
      var mat = ownedMaterials[mi];
      try { mat.dispose(); } catch (_) {}
    }
    ownedMaterials.length = 0;

    if (renderer) {
      try { renderer.dispose(); } catch (_) {}
      // Note: we do NOT call renderer.forceContextLoss() — index.html owns the canvas.
      renderer = null;
    }
    scene = null;
    camera = null;
    ambientLight = null;
    directionalLight = null;
    swarmPointLight = null;
    playerMesh = null;
    enemyMeshes.length = 0;
    playerBulletMeshes.length = 0;
    enemyBulletMeshes.length = 0;
    starfield = null;
    groundGrid = null;
  }

  // ----------------------------------------------------------------
  // Publish
  // ----------------------------------------------------------------
  window.SpaceInvaders = window.SpaceInvaders || {};
  window.SpaceInvaders.render = {
    init: init,
    draw: draw,
    dispose: dispose,
  };
})();
