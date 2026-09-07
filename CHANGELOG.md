# Changelog

## v0.11.0-beta.2 - 2026-09-07

- Added a light appearance with a compact theme switch in the top toolbar.
- The selected appearance is remembered per browser; first use follows the operating-system preference.
- Updated sidebars, messages, composer, menus, dialogs, file cards, and code blocks for readable light-mode contrast.

## v0.11.0-beta.1 - 2026-09-07

- Added a native Windows desktop app-tools bridge with automatic pipe discovery.
- Replaced log-derived task listings and local title/archive/pin overrides with desktop queries and mutations.
- Added project selection and local/worktree environment selection for first-message task creation.
- Added persistent delivery receipts and duplicate-request detection, including uncertain-result handling across restarts.
- Added individual multipart attachment uploads, progress, authenticated downloads, and historical attachment cards.
- Added per-task drafts and cancellation of stale history requests.
- Added API and desktop-protocol regression tests and a browser-test fixture.
- The new bridge currently supports local Codex tasks and sends attachments as local file references. Stop, model controls, SSH tasks and token streaming remain unavailable in this version.
- Verified read-only access against desktop build 26.901.5280.0. Mutation workflows were tested using a simulated desktop, pending user testing on real tasks.

## v0.10.4 - 2026-09-02

- Added a Windows tray application with no console window.
- Added LAN and FRP-compatible browser access instead of loopback-only binding.
- Added responsive phone and desktop layouts.
- Added Codex task discovery and project-oriented task grouping.
- Added message delivery retries and clearer delivery status.
- Added image and general file attachment uploads.
- Added optional startup with Windows.
- Added a blue application icon for the executable and taskbar.
- Included the tested Windows client binary in `client/` and the GitHub Release.
- Added public repository documentation, security guidance, tests, and CI builds.
