# Production Deployment Plan

## Goal

Deploy the Trackkr server and PostgreSQL database to the existing homelab
Dokku installation, then establish a repeatable path for releasing the server,
the workstation daemon, and the browser extensions.

The production topology is deliberately split:

```text
Workstation
|- trackkrd
|- Firefox/Chrome extension -> trackkrd loopback listener
`- HTTPS activity uploads
       |
       v
Dokku
|- trackkr-server web process
`- linked PostgreSQL service
```

Dokku runs only the stateless server. `trackkrd` remains installed on every
tracked workstation because it needs access to the local desktop, idle state,
and browser extensions. The extensions continue to report to the daemon over
loopback, never directly to Dokku.

## Production Principles

- Build one immutable server artifact and promote that artifact to production.
- Keep runtime configuration and secrets outside the image.
- Run database migrations once per release, before routing traffic to the new
  server.
- Keep migrations compatible with the previous server and the next, so a
  zero-downtime strategy remains possible later.
- Fail a release migration before stopping the old container; after a new
  container starts, treat readiness failure as a rollback event.
- Back up PostgreSQL outside the Dokku host and test restoration.
- Release the server, daemon, and extensions independently under an explicit
  compatibility policy.

## Scope

The first production milestone includes:

- a production server container;
- environment-driven server configuration;
- an explicit migration command;
- liveness and readiness endpoints;
- a Dokku release definition and deploy checks;
- PostgreSQL backups and a restore procedure;
- a manual, repeatable deployment runbook;
- production smoke tests and rollback instructions.

The following are deferred until the first manual deployment is stable:

- automatic continuous deployment;
- horizontal web scaling;
- a metrics backend and dashboards;
- automatic workstation daemon updates;
- Firefox Add-ons or Chrome Web Store publication.

## Phase 1: Runtime Contract

Make the server runnable without a checked-in or mounted `config.toml`. Keep
TOML configuration for local development, and configure production entirely
through the environment.

Add or standardize these variables:

| Variable | Purpose |
| --- | --- |
| `PORT` | Dokku-provided HTTP port. |
| `DATABASE_URL` | PostgreSQL DSN supplied by the linked Dokku service. |
| `TRACKKR_SESSION_SECRET` | Session signing secret of at least the current minimum length. |
| `TRACKKR_TIMEZONE` | Timezone used to define dashboard days. |
| `TRACKKR_ALLOW_REGISTRATION` | Explicitly enable or disable public registration. |
| `TRACKKR_SECURE_COOKIES` | Keep enabled in production; allow disabling for local HTTP development. |
| `TRACKKR_TRUSTED_PROXY_CIDRS` | Proxy peer addresses allowed to supply `X-Forwarded-For`. |
| `TRACKKR_CONFIG` | Optional TOML path for development and exceptional deployments. |

Configuration precedence should be defaults, then an optional TOML file, then
environment variables. `DATABASE_URL` should take precedence over the
individual database fields. Before the DSN reaches either pgx or
golang-migrate, parse it and add an explicit `sslmode` when the URL has none.
For a Dokku-linked PostgreSQL service on the private Docker network, the
default is `disable`; this is not a safe universal default. If the host is not
the configured private Dokku database network, require an explicit `sslmode`
such as `require` or `verify-full` instead. The application pool and the
migrate PostgreSQL driver otherwise choose different defaults. Validation must
run after all overrides have been applied.

The chosen ingress disables Dokku's proxy, so Tailscale Serve is the only hop
before the application. Do not blindly trust `X-Forwarded-For`: configure one
trusted proxy address for the observed `tailscaled` peer, and derive the client
address from the rightmost forwarded hop, never the first value. Before
production, verify the installed Tailscale version's Serve behavior with a
spoofed inbound `X-Forwarded-For`: it must append or replace the value, never
pass through a client-supplied value. Keep the app's fixed host port bound to
loopback so callers cannot bypass Serve and inject identity headers. If Serve
forwards `Tailscale-User-Login` for tailnet users, prefer that verified identity
as the limiter key; otherwise use the verified forwarded address. This identity
feeds the login and registration attempt limiter, so a misconfigured proxy must
not collapse every visitor into one global bucket.

Production defaults should bind to `0.0.0.0`, enable secure cookies, and disable
registration. Do not place secrets or a production DSN in the repository.

Acceptance criteria:

- the server starts with environment variables and no `config.toml`;
- the existing local TOML workflow still works;
- an invalid port, DSN, timezone, or short session secret fails startup;
- a private Dokku DSN without `sslmode` is normalized to `disable` for pgx
  and golang-migrate, while an off-network DSN without `sslmode` is rejected;
- configuration precedence has unit-test coverage;
- trusted and untrusted proxy peers have separate client-address tests;
- secrets are not emitted in logs or error messages.

## Phase 2: Release Artifact

Add a root multi-stage `Dockerfile` that builds `cmd/server` and copies only the
server binary into the runtime image. Use a small pinned Alpine runtime with:

- CA certificates for outbound favicon requests;
- timezone data for configured dashboard timezones;
- a non-root runtime user;
- no compiler, source tree, package manager cache, or development config.

Add a root `.dockerignore` alongside it. At minimum exclude `.git/`, `.cache/`,
`.codegraph/`, `dist/`, `coverage.out`, `trackkr-backend`, `trackkrd`, local
configuration, dependency caches, and other generated artifacts. `.gitignore`
does not control Docker build context size.

Templates, static assets, fonts, and SQL migrations are already embedded in the
Go binary and should remain embedded. The image should expose the application
port for documentation, while the process must still honor Dokku's `PORT`.

Add a root `Procfile`:

```text
release: migrate
web: serve
```

Explicit `serve` and `migrate` commands make process intent clear and prevent
ad-hoc administrative commands from unexpectedly starting HTTP.

The image declares `trackkr-server` as its entrypoint. Dokku passes each
Procfile command to that entrypoint, so the Procfile contains subcommands, not
the binary path.

The command bootstrap must load configuration and then dispatch explicitly.
`create-user` and `create-device` already exist and should retain their current
one-off behavior; they must not run migrations as a side effect. The current
bare invocation means "serve", so either preserve that compatibility alias or
make the transition explicit in the release notes while using `serve` in the
Procfile.

The image must be buildable locally and in CI. Record the source revision and
application version in the binary or image metadata, so you can identify the
running release without trusting a mutable tag.

Acceptance criteria:

- the container builds from a clean checkout;
- it runs as a non-root user;
- it starts without repository files mounted into it;
- TLS certificate validation and timezone loading work in the runtime image;
- `trackkr-server serve` and `migrate` have defined behavior inside the image;
- existing `create-user` and `create-device` commands work as one-off image
  commands without starting HTTP or running migrations;
- the `.dockerignore` keeps build context free of local caches, indexes,
  binaries, and secrets.

## Phase 3: Migration Lifecycle

Move schema migration out of unconditional process startup. Add an explicit
`trackkr-server migrate` command and run it through the Dokku release phase.
The web process may verify database connectivity, but it must not independently
run migrations.

The change is a shared-bootstrap refactor: today `cmd/server` runs migrations
before it dispatches `create-user` and `create-device`. Load configuration and
construct only the dependencies required by the selected command. The `serve`
path creates the pool and HTTP server; `migrate` creates the migration
connection; one-off user/device commands create a pool without invoking
`RunMigrations`.

Migration rules from this point forward:

- use expand-and-contract changes across separate releases;
- do not remove or rename a column still used by the previous server release;
- make data backfills bounded and safe to retry;
- avoid long blocking DDL in the release phase;
- document irreversible migrations before deployment;
- take an on-demand backup before a destructive or high-risk migration.

The release command must be idempotent. A failed migration fails the release and
leaves the current web container serving traffic. PostgreSQL migrations also
record a dirty version when a migration fails. Until that state is inspected
and cleared, every later `Up()` fails, including a deployment of the previous
application image.

Provide an explicit administrative recovery path, for example
`trackkr-server migration-status` and `trackkr-server migration-force VERSION`.
The runbook must require operators to inspect the failed migration and database
state first, repair or restore data as appropriate, and only then force the
recorded version before retrying `migrate`. Never force a version merely to make
the next deployment pass.

Acceptance criteria:

- the web path never calls `RunMigrations`, and the migration driver's own
  advisory lock serializes whatever does;
- a release migration failure prevents the new release from becoming active;
- rerunning `migrate` after success is a no-op;
- a dirty migration can be inspected and recovered using the documented
  status/force procedure;
- the previous server remains compatible with the migrated schema during
  deployment overlap and rollback.

## Phase 4: Health and Shutdown

Add unauthenticated operational endpoints that reveal no user or configuration
data:

```text
GET /healthz
GET /readyz
```

`/healthz` is a liveness check. It should return success whenever the HTTP
process is running and must not depend on PostgreSQL.

`/readyz` is a readiness check. It should use a short timeout to verify that the
server can reach PostgreSQL and is capable of serving requests. Template parsing
already happens at startup, so a template failure naturally prevents readiness.

Configure the Dokku deploy check to use `/readyz`. Keep the server's graceful
SIGTERM handling. The server's `serverShutdownTimeout` is 15 seconds; set
Dokku's `stop-timeout-seconds` to 20 seconds, leaving a small margin for the
server to finish its graceful shutdown before Docker sends SIGKILL.

`/api/v1/heartbeat` already exists, but it is API-key authenticated and is a
device liveness endpoint, not a process/readiness endpoint. Dokku checks must
use `/healthz` or `/readyz`, never the heartbeat route.

Add `Strict-Transport-Security` when `TRACKKR_SECURE_COOKIES=true`, for example
with a one-year max age after the tailnet hostname and Serve certificate are
verified. Every production request reaches the app through Serve over HTTPS;
do not emit HSTS when local development disables secure cookies, otherwise a
browser can cache a policy that breaks subsequent local work.

Verify the proxy-aware login and registration throttle here as an operational
requirement. Behind Dokku, ten failed attempts from one client must not lock
out unrelated users, and registration must use the same correctly scoped client
identity. Add an integration test that sends requests through a trusted proxy
peer with an `X-Forwarded-For` chain, plus a test proving an untrusted peer
cannot spoof it.

Acceptance criteria:

- liveness remains successful during a transient database outage;
- readiness fails promptly when PostgreSQL is unavailable;
- neither endpoint creates a session or requires authentication;
- Dokku uses a 20-second stop timeout against the server's 15-second shutdown
  deadline;
- `/api/v1/heartbeat` is not used as a Dokku check;
- HSTS is present when secure cookies are enabled and absent from local HTTP
  development;
- the login and registration limiter keys a request by the real client only
  when the immediate peer is a configured trusted proxy;
- a failed release migration leaves the old container active;
- a failed readiness check triggers the documented previous-image rollback;
- SIGTERM drains HTTP requests and exits within the configured Dokku timeout.

## Phase 5: First Production Deployment

Perform the first deployment manually from a reviewed commit. The Dokku app,
PostgreSQL service, domain, TLS configuration, and service link are host-level
concerns and are intentionally not encoded in application scripts.

Set production configuration through Dokku. At minimum:

- use the linked service's `DATABASE_URL`;
- generate a unique, high-entropy session secret and treat it as durable
  deployment state: every web container must use the same value;
- set the intended timezone explicitly;
- disable registration after the initial account exists;
- keep secure cookies enabled;
- use an HTTPS domain trusted by every reporting workstation.

`TRACKKR_SESSION_SECRET` is not an ordinary per-deploy secret. Rotating it
invalidates every existing signed session cookie and signs all users out, so
rotation requires planned maintenance, a new value applied atomically to every
web container, and a communication/reauthentication step.

If Trackkr is reachable from the public internet, expose only the HTTPS proxy
ports. If every reporting workstation participates in the homelab overlay
network, an internal TLS hostname is acceptable. Plain HTTP is not acceptable
because daemon API keys and browser session cookies cross this connection.

### Ingress Choice

For a personal homelab, prefer Tailscale-only ingress first:

```text
workstation tailnet -> Tailscale Serve HTTPS -> fixed loopback host port -> app
```

Tailscale Serve gives the app a private tailnet DNS name and HTTPS, and keeps
it unreachable from outside the tailnet. Every reporting workstation must
be enrolled in the tailnet and permitted by ACLs. Tailscale documents Serve as
a private HTTPS proxy for local services. [Tailscale Serve](https://tailscale.com/docs/reference/examples/serve)

Disable Dokku's built-in proxy for this app. Do not point Serve at the
container's Docker-network IP: Dokku replaces that IP on every deploy. Publish
the server on a fixed host port bound to loopback (for example `127.0.0.1:18080`)
through Dokku's Docker options, and point Serve at that address. Because two
containers cannot bind the same host port, choose deliberate stop-then-start
deployments for the homelab instead of pretending this topology switches
without downtime. Dokku's checks still gate whether the new container
is healthy, but a failed readiness check after the old container stopped
requires redeploying the previous image.

Run the release migration while the old container is still serving. Then stop
the old container, start the new one on the fixed port, wait for `/readyz`, and
run a tailnet smoke check. If startup or readiness fails, roll back the image
and restart it on the same port. The sequence makes the brief outage explicit
and keeps Serve's target stable across releases. A future Caddy or CDN layer would
require a new trusted-proxy chain policy before it is introduced; do not
blindly add the whole LAN or tailnet to the trusted list.

The daemon's production `server_url` should use the selected HTTPS hostname.
Use the tailnet DNS name and make ACL access part of workstation onboarding.
The daemon's API key remains application credential material, but the service
itself is not exposed to non-tailnet clients.

Create the initial user and device with one-off Dokku commands. Store the device
API key in the workstation daemon configuration or its supported environment
variable, not in shell history, repository files, or CI logs.

Validate the complete production flow:

1. Deploy and confirm the release migration succeeds.
2. Confirm `/healthz` and `/readyz` through the tailnet hostname from an
   enrolled checker.
3. Create the initial user and one device.
4. Configure one workstation daemon with the HTTPS server URL and device key.
5. Generate native desktop and browser-extension activity.
6. Confirm ingestion, login, timeline rendering, filters, device management,
   logout, and session persistence.
7. Restart the web process and PostgreSQL separately and verify recovery.
8. Deploy the same revision again and confirm the release is idempotent.

Do not connect all workstations until this smoke test passes.

## Phase 6: Backup and Recovery

PostgreSQL is the only durable application state. Embedded assets and server
images are reproducible from source and release artifacts.

The `app_icons` and `site_icons` tables contain bounded `BYTEA` cache blobs.
They are regenerable, so exclude them from routine logical data dumps and keep
backup and restore time predictable. Each user is capped at 512 app icons
and 2,048 site icons, with each blob capped at 64 KiB, so the worst-case
regenerable cache is roughly 160 MiB per user. Use
`pg_dump --exclude-table-data=app_icons --exclude-table-data=site_icons`: keep
the table schemas and constraints in the dump, but restore the tables empty.
Keep a separate full-database backup before risky schema work, and verify the
restore drill proves that activity, users, devices, and planned session
reauthentication work with empty icon caches. Document that icons are absent
immediately after restore and refill as daemons and the site-icon refresher
observe activity again.

Implement:

- nightly logical PostgreSQL dumps;
- encrypted storage outside the Dokku host;
- retention with daily, weekly, and monthly restore points;
- monitoring for missing, empty, or failed backups;
- a documented restore command and verification checklist;
- a scheduled restore drill into a separate database.

The restore drill should verify migrations, user login, device keys, activity
counts, timeline queries, empty icon caches, and subsequent icon refill. A
backup is not considered operational until a restore has succeeded.

Before each schema-changing production release, confirm that a recent off-host
backup exists. Take an additional on-demand dump for migrations identified as
high risk.

## Phase 7: CI and Promotion

Keep deployment manual until the runbook has produced multiple uneventful
releases. Then add a protected production workflow after the existing quality
jobs.

Required pre-deploy gates:

- formatting and linting;
- unit and database tests;
- race tests;
- coverage threshold;
- portability builds;
- extension lint, tests, and package validation;
- container build;
- container smoke test against PostgreSQL;
- vulnerability and secret scans.

The preferred mature flow is:

```text
merge to master
  -> CI tests
  -> build and scan one OCI image
  -> tag image with commit SHA
  -> push to registry
  -> protected production approval
  -> deploy the exact tested image digest to Dokku
  -> release migration while the old container serves
  -> stop old container and start the new fixed-port container
  -> readiness check
  -> tailnet smoke check
```

Avoid rebuilding a different image on the Dokku host once registry-based
promotion is enabled. Release tags may provide human-friendly names, but the
deployment record should retain the immutable digest and Git commit.

This topology intentionally accepts a brief outage during the stop/start
window. It has no upstream switch, so it has no zero-downtime retirement
phase; the rollback procedure must be fast and rehearsed instead.

## Phase 8: Client Releases

The server, daemon, and extensions are separate release surfaces. Establish a
small compatibility contract before automating their distribution.

### Daemon

- Build Linux and macOS binaries from version tags.
- Publish checksums for every artifact.
- Sign and notarize the macOS application bundle when distribution expands
  beyond the development machines.
- Install the Linux daemon as a user-level service where desktop session access
  is required.
- Preserve the daemon's pending-record queue across upgrades.
- Test upgrades with the server's current and previous production versions.

### Browser Extensions

- Produce versioned Firefox and Chrome packages from the same release commit.
- Keep browser-to-daemon protocol compatibility explicit.
- Start with controlled sideloaded packages for the homelab.
- Defer store publication until update policy, signing, privacy disclosures,
  and support expectations are defined.

### Compatibility Order

For a backward-compatible server change, deploy the server first and release
clients afterward. For a protocol-breaking change, use a staged rollout:

1. Deploy a server that accepts old and new clients.
2. Upgrade daemons and extensions.
3. Confirm old-client traffic has stopped.
4. Remove the compatibility path in a later server release.

## Phase 9: Operations

Minimum production monitoring should cover:

- tailnet-enrolled HTTPS liveness and Tailscale Serve availability;
- internal readiness and PostgreSQL connectivity;
- web container restart count;
- HTTP 5xx responses and authentication failures;
- database disk usage and connection saturation;
- Dokku host disk and memory;
- Tailscale node health and Serve certificate renewal status;
- backup age and restore-drill status.

Retain application logs outside ephemeral containers, and stamp each with a
release identifier. Logs must not contain passwords, session secrets, device
API keys, cookies, or full database URLs.

Metrics and tracing can follow observed needs. They are not prerequisites for
the first deployment, but health alerts, backup alerts, and searchable logs are.

## Rollback Strategy

Application rollback means redeploying the previous immutable image while
leaving the database at its current schema version. Every migration must
survive contact with the application release before it.

For a failed release:

1. If the release phase or readiness check fails, keep the current release and
   inspect migration and application logs.
2. If the migration is dirty, inspect the recorded version and failed statement,
   repair or restore the database as needed, then run the documented force
   command for that exact version before retrying the release. The force step is
   required even when rolling back to the previous application image.
3. If the new fixed-port container is faulty, stop it and restore the previous
   image immediately.
4. Do not run down migrations as a routine rollback mechanism.
5. If data was corrupted, stop writes, preserve the database, and decide
   between a corrective migration and an off-host backup restore.
6. Record the failed revision, migration version, image digest, and remediation.

Destructive schema rollback is a disaster-recovery action, not a normal deploy
operation.

## Definition of Production Ready

Trackkr is production ready when all of the following are true:

- the server runs from an immutable non-root container;
- the Docker build context excludes local caches, indexes, binaries, and secrets;
- production requires no repository-mounted configuration;
- migrations execute once in the release phase;
- dirty migration recovery has been rehearsed without using a down migration;
- deploy readiness checks gate the new container and the fixed-port rollback
  has been rehearsed;
- proxy-aware login throttling has been tested through Tailscale Serve;
- HTTPS is enforced, secure cookies are enabled, and conditional HSTS is
  verified for production responses;
- registration is disabled unless intentionally opened;
- one complete workstation-to-dashboard flow has passed in production;
- PostgreSQL backups are off-host, monitored, and restore-tested;
- rollback to the previous server image has been rehearsed;
- server, daemon, and extension versions are identifiable;
- the deployment and recovery runbooks are sufficient for a release without
  reconstructing commands from shell history.

## Implementation Order

1. Environment-only server configuration.
2. DSN normalization and proxy-aware client identity.
3. Explicit `serve` and `migrate` commands, with dirty-state recovery.
4. Liveness, readiness, and graceful-shutdown verification.
5. Production `Dockerfile`, `.dockerignore`, `Procfile`, and deploy check.
6. Container and migration tests in CI.
7. Manual Dokku deployment and single-workstation smoke test.
8. Off-host backups and a successful restore drill.
9. Repeatable manual releases and rollback rehearsal.
10. Protected immutable-image promotion.
11. Versioned daemon and extension release automation.

## References

- [Dokku Dockerfile deployment](https://dokku.com/docs/deployment/builders/dockerfiles/)
- [Dokku process management](https://dokku.com/docs/processes/process-management/)
- [Dokku deployment tasks](https://dokku.com/docs/advanced-usage/deployment-tasks/)
- [Dokku zero-downtime deploy checks](https://dokku.com/docs/deployment/zero-downtime-deploys/)
- [Dokku backup and recovery](https://dokku.com/docs/advanced-usage/backup-recovery/)
