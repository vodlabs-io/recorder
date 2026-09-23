// Electrobun's bundled bun/index.ts imports 'three' (its WGPU template demo
// dependency, unrelated to anything this app uses) with no shipped types and
// no @types/three installed. Without this, tsc fails on that import before it
// ever reaches our own code.
declare module "three";
