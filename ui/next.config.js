/** @type {import('next').NextConfig} */
// Static export: `next build` produces a fully static site in dist/, which
// the nginx image (see Dockerfile) serves as plain files. No rewrites, no
// proxying — the gateway routes /api/jobs and /api/repos/ to the controller
// and everything else to this UI, so fetch('/api/...') is same-origin.
// Routing within the app is hash-based (#/mirrors/<id>), exactly like the
// legacy dashboard, so no server-side rewrite rules are needed either.
const nextConfig = {
  output: 'export',
  distDir: 'dist',
  compress: false,
};

module.exports = nextConfig;
