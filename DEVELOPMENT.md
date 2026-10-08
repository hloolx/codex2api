# Local Development on Windows

This checkout includes upstream `main` at `b1bfe1b4` (v3.0.7 plus nine commits)
and the State management and Basispoints extensions published in
`hloolx/codex2api`. Releases retain credential-level Turn State injection,
managed account/model state reuse, and the fork's per-key upstream routing.
See [the integration report](docs/upstream-sync-20261008.md) for compatibility
decisions and validation.

Requirements: Go with automatic toolchain selection (the project requires
1.26.6), Node.js 22.12 or newer, npm, and PowerShell.

This machine has a verified official Go 1.26.6 installation in `.dev/go`.
The launcher prefers it without changing the system Go installation. Dependency
downloads that fail directly can be retried with `HTTP_PROXY` and `HTTPS_PROXY`
set to `http://127.0.0.1:9000`.

From this directory:

```powershell
.\dev.ps1
.\dev.ps1 -Action status
.\dev.ps1 -Action stop
```

The first start installs Air v1.67.4 in `.dev/tools`, installs missing frontend
dependencies, and builds the frontend assets required by Go's embed directive.
The two development services run in the background. Inspect `.dev/*.log` for
startup and compilation errors.

- Frontend with Vite HMR: http://127.0.0.1:5173/admin/
- Backend API: http://127.0.0.1:8080/v1
- Backend health: http://127.0.0.1:8080/health
- Initialize the admin password on the first browser visit.

Air rebuilds and restarts the backend when Go source, `go.mod`, `go.sum`, or
`.env` changes. Frontend edits are handled by Vite without restarting Go.
The backend's embedded frontend is a build snapshot; use the Vite URL while
developing, and run `npm.cmd run build` in `frontend` before a release build.

If a port is occupied, select unused ports explicitly:

```powershell
.\dev.ps1 -BackendPort 18080 -FrontendPort 15173
```

The launcher overrides `CODEX_PORT` and `CODEX_BIND` for its child processes.
`VITE_API_TARGET` makes the frontend proxy follow the selected backend port.
The checked-in Vite config also proxies `/api`, `/health`, and `/v1`.

Local configuration is in the ignored `.env`. SQLite data and image assets
are under `data/`; runtime logs are under `logs/` and `.dev/`. No existing
account files are imported. Stopping or restarting preserves the database.

Validation commands:

```powershell
go build -o .dev/tmp/codex2api-check.exe .
npm.cmd --prefix frontend run build
```
