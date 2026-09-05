# Falcon web UI

The UI is a read-only Next.js static export served by nginx for the single namespace where Falcon is installed. The controller web API is same-origin under `/api`.

## Pages

Overview shows a 24-hour synchronization activity clock. Mirrors lists synchronization state and usage, with a detail view for one Mirror. The sidebar links to Overview, Mirrors, and `/mirrorz.json`, and provides theme and collapse controls. Authentication is supplied by the controller's GitHub OAuth when the admin route is enabled. Resource mutation is intentionally absent; use Kubernetes APIs.

## API and refresh

`GET /api/jobs` returns synchronization state, `/api/repos/<name>` returns a Mirror specification, and `/api/usage` returns optional ZFS usage. Jobs refresh every five seconds, usage every thirty seconds, and specifications when opened. Clock and relative times update every second. Optional request failures are logged without breaking the page; the last successful usage remains visible.

## Build

`npm run build` creates the static export packaged in the UI image. Gateway API routes expose the UI and API; nginx performs no API proxying.
