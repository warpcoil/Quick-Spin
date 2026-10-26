# QuickSpin

A portable WordPress development environment for **Windows, macOS (Apple Silicon) and Linux**, in a single executable.

Put it in any folder (a USB stick works too) and start it. On first start it downloads its own toolchain into that folder. It then opens a Chromium window on the QuickSpin dashboard. From there you paste a Git URL and get a running copy of the site on a domain like `acme.test`. QuickSpin imports the SQL dump, rewrites the URLs and gives you a one-click admin login and VS Code in the same browser.

Nothing is installed system-wide. QuickSpin needs no admin rights, no hosts-file edits and no Docker. Delete the folder and it's gone.

## Quick start

1. Copy the executable for your system to a folder, for example `C:\QuickSpin\` or `~/QuickSpin/`:

   | System | File |
   |---|---|
   | Windows (x64) | `quickspin.exe` |
   | macOS, Apple Silicon (M-series) | `quickspin-mac-arm64` |
   | Linux x64 / arm64 | `quickspin-linux-x86_64` / `quickspin-linux-arm64` |

2. Start it. On Windows, double-click it. On macOS and Linux, run it from a terminal. The first run downloads about 400 MB, then Chromium opens the dashboard.
3. Click **New project**, paste a repository URL and click **Create project**.
4. When the job finishes, use **Open site**, **WP Admin** (logs you in as the first administrator) or **Edit code**.

Close the Chromium window (or press Ctrl+C in the console) and QuickSpin stops every service. Start it again and all your projects are still there.

## What gets downloaded

| Component | Source | Used for |
|---|---|---|
| FrankenPHP + Xdebug (PHP 8.5) | [warpcoil/FrankenPHP-XDebug](https://github.com/warpcoil/FrankenPHP-XDebug) releases | Web server and PHP in one process |
| Chromium | chromium-browser-snapshots, falling back to Chrome for Testing | Browser where your dev domains resolve |
| Database | Windows and Linux: MariaDB LTS from mariadb.org. macOS: MySQL 8.4 LTS from mysql.com | Databases. On Linux an installed MariaDB/MySQL is used if present |
| VS Code CLI (`code serve-web`) | update.code.visualstudio.com | Editor and debugger in the browser |
| MinGit (Windows only) | git-for-windows releases | Cloning; skipped if Git is already installed, so your existing credentials and SSH keys work. macOS and Linux use the system Git |
| WP-CLI, Adminer, Mozilla CA bundle | upstream | Imports and URL rewrites, database browser, HTTPS from PHP |

QuickSpin always fetches the latest version and falls back to versions pinned in the source if discovery fails. `quickspin update` re-downloads everything and keeps your projects. Run it while QuickSpin is stopped.

Chromium is downloaded as a prebuilt build, not compiled. Compiling Chromium takes hours and around 100 GB of disk, which would defeat the point.

### PHP and Xdebug

QuickSpin runs **PHP 8.5** in FrankenPHP, using the Xdebug-enabled builds from the FrankenPHP-XDebug repository.

Until a build for your platform is published, QuickSpin uses the official FrankenPHP release, which is also PHP 8.5. That release includes Xdebug on Windows only, and the **Debug** button is greyed out wherever Xdebug isn't available. Once your build is published, the next start switches over automatically.

Set `frankenphpBuilds` in `config.json` to use your own fork.

**Upgrading from an earlier QuickSpin:** projects and settings that used an older PHP version move to 8.5 automatically, and the old PHP downloads are deleted. Several PHP versions side by side are still supported internally, so a future version only needs adding to the version list in `components.go` and to the builds workflow.

## How domains work without a hosts file

Chromium is started with `--host-resolver-rules`. That flag maps `*.test` and every domain you add to QuickSpin's web server, **inside that browser only**.

- **`.test` domains** work immediately.
- **Other domains** (`client.co.uk`, `staging.client.io`, …) work too. The browser needs one restart to pick them up, and the dashboard shows a button for it (open tabs are restored).
- **`.dev`, `.app` and similar** HSTS-preloaded domains are rejected, because browsers force them onto HTTPS.

Your normal browser is unaffected. The sites are reachable only through QuickSpin's Chromium.

## What happens when you create a project

1. **Clone** the repository into `data\projects\<name>`. You can pick a branch and a shallow clone. A local folder is used in place instead.
2. **Detect the web root.** QuickSpin looks for the folder containing `wp-settings.php`, `wp-config.php` or `wp-content`. It handles repo-root, `public/`, `web/` and Bedrock-style `web/wp` layouts. If the repo has no WordPress core (for example a `wp-content`-only repo), core is downloaded and git-ignored.
3. **Detect the database dump:** the largest `.sql`, `.sql.gz` or `.sql.zip` in the repo.
4. **Import** it into its own database (`qs_<name>`). MySQL 8 collations are converted for MariaDB on the fly.
5. **Detect the table prefix** from the imported tables.
6. **Rewrite URLs** from the original domain to the dev domain using WP-CLI. This is serialization-safe and covers http/https, `www`, protocol-relative and JSON-escaped (`https:\/\/`) forms. GUIDs and email addresses are left alone, and so are look-alike domains such as `site.com.au`.
7. **Leave `wp-config.php` untouched.** A PHP `auto_prepend_file` injects the local `DB_*`, `WP_HOME`, `WP_SITEURL` and `WP_ENVIRONMENT_TYPE=local` constants for that site only, so your committed config, even with production credentials, works as-is and `git status` stays clean. If the repo has no `wp-config.php`, one is generated and added to `.git/info/exclude`.

If the repository has no dump, you get a fresh install with the login `admin` / `admin`.

### Committing the layout for your team

You can skip auto-detection by committing a `.quickspin.json` file at the repository root:

```json
{
  "webRoot": "public",
  "dumpPath": "db/site.sql.gz",
  "php": "8.5"
}
```

Use `"dumpPath": ""` for a fresh install, or `"webRoot": ""` to serve the repository root.

## Project actions (⋯ menu on each card)

- **Git pull**: fast-forward pull.
- **Rebuild**: re-import the dump and redo the URL rewrite. Use it after pulling a new dump.
- **Export database**: writes to the exports folder, or overwrites the dump in the repo (gzip kept). Optionally restores the original production URL, ready to commit.
- **Settings**: name, domains, PHP version and WP_DEBUG. Changing the main domain rewrites the database.
- **Delete**: drops the database and optionally the cloned files.

## Exporting and importing your configuration

Open the **⋯** menu at the top right and choose **Export configuration…** to download a `.zip`. You can choose:

- **Settings**: ports, default PHP, PHP workers and browser options. Tool paths are left out because they are specific to each machine.
- **Projects**: name, domains, repository URL and branch, web root, dump path, PHP version, WP_DEBUG and the original production URL. Passwords or tokens embedded in clone URLs are removed.
- **Databases** (optional): each project's current local database, so work you did locally (content, settings, users) moves with it.

**Import configuration…** on another machine (or the same one, after a reinstall) shows what the file contains:

- Projects already set up, or whose domains are taken, are left unticked.
- Git projects are cloned again.
- Local-folder projects are re-attached if the same folder exists on the new machine.
- If you included databases, they are imported instead of the repository dump. QuickSpin keeps remembering the real production URL, so "Export database → restore original URL" still works.
- Applying settings needs a QuickSpin restart.

The file format is plain JSON plus gzip SQL dumps inside a zip, so it also works as a backup.

## Editing and debugging

**Edit code** opens VS Code (`code serve-web`) in a new tab with a workspace for the project. It's pre-configured with the project's PHP binary and a **Listen for Xdebug (QuickSpin)** launch configuration. Install the recommended *PHP Debug* extension when prompted.

To debug:

1. In the editor, start **Listen for Xdebug (QuickSpin)**.
2. Click **Debug** on the project card. This sets the Xdebug trigger cookie, so breakpoints hit on that page and the ones after it.

Xdebug uses `start_with_request=trigger`, so normal browsing stays fast.

## Folder layout

```
QuickSpin/
  quickspin(.exe)
  runtime\          downloaded tools; safe to delete, re-downloaded on next start
  data\
    config.json     ports, default PHP, PHP workers, tool path overrides
    projects.json   your projects
    projects\       cloned repositories
    mysql\          database files (MariaDB or MySQL)
    php\<version>\  generated php.ini files; on macOS and Linux also bin/php for terminals and editors
    web\            generated FrankenPHP (Caddy) configuration, one per PHP version
    browser\        Chromium profile (bookmarks, logins, extensions)
    vscode\         editor settings and extensions
    workspaces\     .code-workspace per project
    exports\        database exports
    logs\           one log per service and per project
    php\custom.ini  your php.ini overrides (all versions)
```

## Configuration (`data\config.json`)

| Key | Default | Notes |
|---|---|---|
| `ports.dashboard` / `http` / `editor` / `mariadb` | 47080 / 47081 / 47082 / 47306 | Everything binds to 127.0.0.1 |
| `defaultPhp` | `8.5` | PHP 8.5 is the version QuickSpin offers |
| `phpWorkers` | 8 | PHP threads per FrankenPHP process |
| `frankenphpBuilds` | warpcoil/FrankenPHP-XDebug latest release | Where to find `manifest.json` and the FrankenPHP + Xdebug packages |
| `ports.phpBase` | 47100 | Reserved for extra PHP versions (`phpBase + n`, admin `phpBase + 50 + n`) if more are added later |
| `mariadbSeries` | `11.8` | LTS series to download (Windows, Linux) |
| `browser.exitWithBrowser` | `true` | Closing Chromium stops QuickSpin |
| `browser.proxyServer` | | Corporate proxy; dev domains always bypass it |
| `git.preferSystem` | `true` | Use an installed Git and its credential manager |
| `paths` | `{}` | Use existing tools, e.g. `{"chromium": "D:\\chrome\\chrome.exe", "php-8.5": "/opt/frankenphp/frankenphp", "mariadb": "/usr/local/mariadb"}` |

The database login for your own tools is `127.0.0.1:47306`, user `quickspin`, password `quickspin`. **DB** on each card opens Adminer already logged in.

## Security notes

- Every service listens on 127.0.0.1 only.
- The dashboard API rejects cross-origin and DNS-rebinding requests, so websites you visit can't drive it.
- The editor needs a token, which is stored in `data\vscode\token`.
- Admin login links are single-use and expire after 2 minutes.

## Known limitations

- **Testing so far:** QuickSpin was tested end to end on Linux. The Windows and macOS builds compile and share all of that code, but haven't been run on those systems yet.
- **macOS:** the executable isn't signed. The first time, right-click it and choose **Open** (or run `xattr -d com.apple.quarantine quickspin-mac-arm64`).
- **Linux:** QuickSpin prefers an installed MariaDB/MySQL (`apt install mariadb-server`), because MariaDB's generic Linux archives need extra system libraries. Chromium needs the usual desktop libraries. On Linux arm64, install Chromium yourself, because there's no portable build to download.
- Private repositories need working Git credentials: Git for Windows' credential manager, or an SSH key for `git@` URLs.
- Composer and Node builds are not run automatically. Run them in the editor's terminal (Bedrock needs `composer install`).
- Windows SmartScreen may warn about an unsigned executable the first time it runs.
- Sites run on plain HTTP; HTTPS dev certificates aren't included.

## Downloads

Releases are at https://github.com/warpcoil/Quick-Spin/releases. Each release has the four executables, this README and `SHA256SUMS.txt`.

## Building from source

You need Go 1.24 or newer. There are no third-party Go dependencies and no cgo, so every target can be built from any OS.

```
go test ./...
bash scripts/build.sh            # all four platforms into dist/, versioned from the git tag
bash scripts/build.sh 2.3.0 out  # explicit version and output folder
go build -o quickspin .          # just this machine
```

The version shown by `quickspin version` and on the dashboard is set at build time with `-ldflags "-X main.appVersion=…"`.

### Releases (GitHub Actions)

`.github/workflows/release.yml`:

- **Every push and pull request:** runs `gofmt`, `go vet` and the tests on Windows, macOS and Linux, then builds all four targets. The binaries are attached to the run as an artifact.
- **Pushing a version tag** publishes a GitHub release with the binaries and checksums:

  ```
  git tag v2.3.0
  git push origin v2.3.0
  ```

- **Manually:** Actions → *Build QuickSpin* → *Run workflow*. Enter a version to publish a release without tagging locally, or leave it empty to only build.
