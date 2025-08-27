/** @type {import('next').NextConfig} */
const nextConfig = {
  async rewrites() {
    return [
      {
        source: '/api/:path*',
        destination: 'http://localhost:8080/api/:path*',
      },
    ];
  },
  compress: false,
  output: 'export',
  distDir: 'dist',
};

module.exports = nextConfig;
