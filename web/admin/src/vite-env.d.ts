/// <reference types="vite/client" />

// Vite client types declare the asset modules this SPA imports
// (e.g. `import './style.css'`). TypeScript 7 made side-effect
// imports type-checked (TS2882), so without this reference the
// build fails even though Vite bundles the CSS fine.
