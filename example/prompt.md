# Procedural Galaxy Explorer: One Pass Benchmark

Build a single self contained `galaxy.html` that opens in a modern browser and renders an explorable procedural galaxy in WebGL. The player flies first person through an effectively infinite universe of stars, planets, moons, rings, asteroid belts, nebulae, and a black hole. Bias every choice toward cinematic and atmospheric: the player feels small, the universe feels vast.

## Constraints
* One HTML file, inline CSS and JS only. No build step. Serve the file over local HTTP. Three.js and its addons are the only external dependencies.
* Three.js from a CDN via an ES module import map; use its post processing addons for bloom and a custom finishing pass.
* Generate every texture procedurally (canvas 2D + your own GLSL noise). Synthesize every sound with the Web Audio API.
* ACES tone mapping, low exposure, sRGB output, gentle bloom. Handle the huge near/far range so meter scale objects and cosmic distances coexist.
* Deterministic world: the same coordinates always regenerate identically (hash seeded PRNG). Stream chunks of space around the camera so flight never ends and never repeats artificially.

## Include
* **Stars:** varied spectral classes and sizes, rendered efficiently (instancing/points), with glow and subtle twinkle.
* **Star systems**, generated lazily for the nearest star: several planets (rocky / gas / ice) on inclined orbits with procedurally colored surfaces, atmospheres, clouds, moons, and asteroid belts. At least one dramatic ringed gas giant with a shadowed ring.
* **Deep space backdrop:** a distant star field plus a shader painted Milky Way band.
* **A few drifting nebulae** and **one black hole** with an accretion disc.
* **6 DOF flight:** pointer lock mouse look, WASD thrust/strafe, roll and brake, speed readout; smooth and gimbal lock free (quaternion based).
* **A weapon:** fire at a star to trigger a large multi stage supernova; fire at a planet to blow it apart into debris. Shockwaves, fragments, particles, sound.
* **A minimal HUD** (speed, nearest body) and a cinematic title intro that hands off to flight on click.

## Done when
It opens with no console errors, the intro shows a ringed planet, flying reveals an endless and varied universe, and shooting things produces convincing explosions with synthesized audio.

Deliver the complete `galaxy.html` and nothing else: no commentary, no markdown fences.