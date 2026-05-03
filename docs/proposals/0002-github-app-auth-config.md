# Proposal 0002 — Wire GitHub App auth through config and main

| Field | Value |
|---|---|
| Status | In progress |
| Author | print-my-ideas operator |
| Target milestone | Follow-up to commit `55ea398` |
| Affects | `internal/config`, `cmd/symphony-go/main.go`, `cmd/symphony-go/doctor.go`, `SPEC.md` §2 (config schema) |
| Backward compatible | Yes |
| Closes gap | G18 |

## 1. Summary

Commit `55ea398 feat(github): add App-installation auth via NewAppClient`
landed `AppAuth` and `NewAppClient` in `internal/github`, with full
unit tests. They are not yet reachable from the binary: `GitHubConfig`
exposes only `TokenEnv` + `PollIntervalSeconds`, and `main.go`
unconditionally constructs a PAT client via `os.Getenv(cfg.GitHub.TokenEnv)`.

This proposal wires App auth through the config layer and the CLI
entry point so operators can choose `auth: "app"` in `config.yml`. It
adds zero capability to the package layer and ~80 LOC + tests at the
config and command layers. Backward-compatible: omitting the new
fields preserves PAT behavior exactly.

## 2. Motivation

The README already lists App-installation auth as a supported feature
("PAT or GitHub App installation (`NewAppClient`)" in the differences
table). Operators reading the README reasonably expect to flip a config
flag and have it work. Today they cannot.

A concrete operator (print-my-ideas) is blocked from running App-first:
App `3587670` is registered and scoped to one repo, the `.pem` is on
disk, but `symphony-go run` ignores it because the wiring stops at the
package boundary.

Beyond unblocking that user, App auth is the right default for any
multi-host or production-shaped deployment:

- Installation tokens are short-lived (~1 hour, auto-rotated by
  `ghinstallation`), so leaked tokens have a small blast radius vs
  long-lived PATs.
- Per-repo install scope is tighter than user-PAT scope.
- The bot identity gives audit-clean provenance on label transitions
  and PR creation ("symphony-bot[bot]" vs the operator's user).

## 3. Goals

- Operators select PAT or App via one config field.
- Existing PAT-only configs keep working unchanged (no migration).
- App credentials are loaded from env-var-indirected paths, never from
  inline literals in `config.yml` (consistent with §2's "config carries
  references, not secrets" rule).
- `doctor` exercises whichever auth path is configured and fails fast
  with an actionable error if it doesn't work.
- The package-level `AppAuth` shape is unchanged.

## 4. Non-goals

- A pluggable auth interface for non-GitHub trackers.
- Inline PEM literals in `config.yml`.
- Auto-discovery of installation IDs (operator provides it).
- Token caching across restarts (`ghinstallation` handles this in-process;
  one minted token per process lifetime is fine).
- Per-job auth (one auth identity for the whole orchestrator).

## 5. Design

### 5.1 Config schema additions

```yaml
github:
  auth: "pat"                              # "pat" (default) | "app"

  # Used when auth == "pat" (existing).
  token_env: "GITHUB_TOKEN"

  # Used when auth == "app" (new). Each is an env-var *name* whose value
  # is the actual credential (or path to it).
  app_id_env:            "GITHUB_APP_ID"
  installation_id_env:   "GITHUB_APP_INSTALLATION_ID"
  private_key_path_env:  "GITHUB_APP_PRIVATE_KEY_PATH"   # path to .pem
  # Mutually exclusive with private_key_path_env: inline PEM contents.
  # Useful for environments without filesystem (Cloudflare Workers).
  # Most installs use the path form; this is an escape hatch.
  private_key_pem_env:   ""

  poll_interval_seconds: 30
```

Resolution rules at startup:

1. If `auth` is empty or `"pat"`: use `token_env`. Existing behavior.
2. If `auth == "app"`: read `app_id_env`, `installation_id_env`, and
   exactly one of `private_key_path_env` or `private_key_pem_env`.
3. Any other `auth` value: hard fail at config validation with the list
   of accepted values.
4. Setting both `private_key_path_env` and `private_key_pem_env` is a
   validation error.
5. Setting `auth: "app"` with any of the three required fields empty is
   a validation error, listing the missing one.
6. `app_id` and `installation_id` env values must parse as positive
   `int64`. Empty or non-numeric → validation error with the specific
   field named.

### 5.2 Code changes

**`internal/config/config.go`** — extend `GitHubConfig`:

```go
type GitHubConfig struct {
    Auth string `yaml:"auth"`                         // "pat" | "app"

    // PAT (existing)
    TokenEnv string `yaml:"token_env"`

    // App (new)
    AppIDEnv           string `yaml:"app_id_env"`
    InstallationIDEnv  string `yaml:"installation_id_env"`
    PrivateKeyPathEnv  string `yaml:"private_key_path_env"`
    PrivateKeyPEMEnv   string `yaml:"private_key_pem_env"`

    PollIntervalSeconds int `yaml:"poll_interval_seconds"`
}
```

Defaults applied in `applyDefaults`: `Auth = "pat"` when empty,
preserving existing behavior.

**`internal/config/validate.go`** — add the rules from §5.1.

**`cmd/symphony-go/main.go`** — replace the unconditional PAT block:

```go
// before
token := os.Getenv(cfg.GitHub.TokenEnv)
if token == "" {
    slog.Error("github token env var is empty", "env", cfg.GitHub.TokenEnv)
    os.Exit(1)
}
gh := github.NewClient(ctx, token)
```

with a small switch:

```go
gh, err := newGitHubClient(ctx, cfg.GitHub)
if err != nil {
    slog.Error("github auth failed", "err", err)
    os.Exit(1)
}
```

`newGitHubClient` (in main.go or a new `cmd/symphony-go/auth.go`):

```go
func newGitHubClient(ctx context.Context, c config.GitHubConfig) (*github.Client, error) {
    switch c.Auth {
    case "", "pat":
        token := os.Getenv(c.TokenEnv)
        if token == "" {
            return nil, fmt.Errorf("github: %s is empty", c.TokenEnv)
        }
        return github.NewClient(ctx, token), nil

    case "app":
        appID, err := strconv.ParseInt(os.Getenv(c.AppIDEnv), 10, 64)
        if err != nil || appID <= 0 {
            return nil, fmt.Errorf("github: %s is not a positive int64", c.AppIDEnv)
        }
        instID, err := strconv.ParseInt(os.Getenv(c.InstallationIDEnv), 10, 64)
        if err != nil || instID <= 0 {
            return nil, fmt.Errorf("github: %s is not a positive int64", c.InstallationIDEnv)
        }
        pem, err := loadPEM(c)
        if err != nil {
            return nil, err
        }
        return github.NewAppClient(ctx, github.AppAuth{
            AppID:          appID,
            InstallationID: instID,
            PrivateKeyPEM:  pem,
        })

    default:
        return nil, fmt.Errorf("github.auth %q: must be \"pat\" or \"app\"", c.Auth)
    }
}

func loadPEM(c config.GitHubConfig) ([]byte, error) {
    if path := os.Getenv(c.PrivateKeyPathEnv); path != "" {
        return os.ReadFile(path)
    }
    if pem := os.Getenv(c.PrivateKeyPEMEnv); pem != "" {
        return []byte(pem), nil
    }
    return nil, fmt.Errorf("github: neither %s nor %s is set",
        c.PrivateKeyPathEnv, c.PrivateKeyPEMEnv)
}
```

**`cmd/symphony-go/doctor.go`** — augment the existing GitHub-reach
check to construct the client via `newGitHubClient` (so it exercises
the actual auth path), then make one read call (e.g. `Get(repo)`) to
verify the token works.

### 5.3 Existing source already supports this

- `internal/github/app.go`: `AppAuth`, `NewAppClient` — done at `55ea398`.
- `internal/github/app_test.go`: validation cases for missing `AppID`,
  missing `InstallationID`, missing `PrivateKeyPEM`, malformed PEM —
  done.
- `internal/config/config.go:75`: `IgnoredUsers` already documents
  "the orchestrator's own bot self-approve when running as a GitHub
  App" — the approval-side guard is in place.

This proposal does not touch `internal/github/`.

### 5.4 Token lifetime

`ghinstallation.New` (used by `NewAppClient`, line `internal/github/app.go:56`)
mints installation tokens on demand and refreshes them before expiry.
No symphony-go-side caching, rotation, or retry logic is needed.
Tested behavior: tokens mid-flight survive past the 1-hour expiry as
long as the long-running orchestrator process stays up.

### 5.5 Doctor output

`symphony-go doctor` should explicitly report which auth mode is
active so operators can confirm at a glance:

```
github auth: app (app_id=3587670, installation_id=129186370)
github auth: pat (token_env=GITHUB_TOKEN)
```

Failure modes get specific messages:

```
github: GITHUB_APP_ID is not a positive int64
github: read /home/long/.symphony-go/github-app.pem: permission denied
github: ghinstallation token request failed: 401 Bad credentials
```

The last one usually means: AppID and InstallationID don't match (the
key is valid but it's for a different App), or the App isn't installed
on the configured repo.

## 6. Backward compatibility

- Configs that omit `auth` get `auth = "pat"` by default. Existing PAT
  behavior bit-for-bit unchanged.
- Configs that omit `token_env` still default to `"GITHUB_TOKEN"` (existing
  default).
- App fields are all optional and ignored when `auth != "app"`.
- No migration script needed; no on-disk state changes.

## 7. Test plan

**Config:**
- Parse + validate `auth: "pat"` (default + explicit).
- Parse + validate `auth: "app"` with all required fields.
- Reject `auth: "app"` with each individual missing field.
- Reject `auth: "app"` with both `private_key_path_env` and
  `private_key_pem_env` set.
- Reject unknown `auth` values.
- Default `auth` to `"pat"` when empty.

**Auth path (cmd-level, table-driven, mocking `os.Getenv` and `os.ReadFile`):**
- PAT happy path.
- PAT empty token → error.
- App happy path with path-based PEM.
- App happy path with inline PEM.
- App: non-numeric AppID → specific error.
- App: missing PEM file → wrapped IO error.

**Integration:**
- A test config with `auth: "app"` boots the orchestrator, reaches
  doctor's GitHub round-trip, and doctor exits 0 against a fake server
  that issues installation tokens.
- A second integration test boots with PAT, asserts existing fake
  paths still work.

## 8. Documentation

- `SPEC.md` §2: extend the `github:` block in the config example with
  the new fields, gated under a "Auth modes" subheading.
- `README.md`: in the "Diverged — product fit" table, the
  "PAT (`NewClient`) or GitHub App installation (`NewAppClient`)" row
  links to a new `docs/github-app-setup.md` walkthrough.
- New `docs/github-app-setup.md`: step-by-step App registration,
  permissions, install, key download, env-var setup, doctor verification.
  Borrows liberally from the print-my-ideas migration doc, which
  already contains a working setup script.

## 9. Rollout

Single PR. Order of changes:

1. `internal/config` schema + validation + defaults + tests.
2. `cmd/symphony-go/auth.go` (new file) + main.go switch + tests.
3. `cmd/symphony-go/doctor.go` augmentation + tests.
4. `SPEC.md` + `README.md` + `docs/github-app-setup.md`.
5. Smoke a real run on a sandbox repo with App auth before merging.

## 10. Risks

- **Operator confusion between `app_id_env` and the literal App ID.**
  The field is *the name of an env var that holds the App ID*, not the
  App ID itself. Mitigation: doctor's confirmation line shows the
  resolved values; the new walkthrough doc leads with a worked example.
- **PEM permission issues.** The `.pem` must be readable by the
  orchestrator process but not group/world. Doctor warns if the file
  mode is broader than `0600`.
- **Cloudflare Workers and other no-filesystem environments** that
  consume symphony-go internals would prefer `private_key_pem_env`.
  Symphony-go itself doesn't run on Workers, so this is mostly
  forward-compatibility for the package; the Worker-side use case
  (chief-of-staff in print-my-ideas) goes through `internal/github`
  directly anyway.
- **App not installed on the configured repo.** A common setup error.
  Doctor's first GitHub call surfaces this as a 404; the message
  should hint: "is App `<id>` installed on `<repo>`?".

## 11. Open questions

1. **Should the field name be `private_key_path` (literal path) instead
   of `private_key_path_env` (env-var name)?** Literal paths in
   `config.yml` would be easier to read but break the project's
   "config holds references, not secrets/paths" convention from SPEC §2.
   Recommendation: keep `_env` shape consistent with `token_env`.
2. **Should `installation_id` be discoverable?** The
   `go-github`/`ghinstallation` libs can list installations of an App
   given its private key, allowing config to omit it. Convenience vs.
   one more network call at startup. Recommendation: defer; ask
   operator to provide it explicitly. Discoverability can be added in
   a follow-up.
3. **Should auth mode default to `"app"` after this lands?**
   Recommendation: no. Default `"pat"` for backward compatibility with
   existing operators; document that `"app"` is the recommended setup
   for new installs.

## 12. Estimated effort

- Config schema + validation + tests: ~30 LOC + 60 LOC test, ~1 hour.
- main.go switch + auth.go + tests: ~50 LOC + 80 LOC test, ~1.5 hours.
- Doctor augmentation: ~20 LOC + 40 LOC test, ~30 min.
- Docs: `SPEC.md` patch + new `github-app-setup.md`, ~1 hour.
- Manual smoke against a real App: 30 min.

**Total: ~4–5 hours.** Should land as a single small PR.
