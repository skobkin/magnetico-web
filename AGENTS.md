# AGENTS.md

This is a Symfony project. Check `composer.json` for the exact Symfony/PHP version
in use, and read `symfony.lock` to see which recipes ran. Don't assume Doctrine,
Twig, API Platform, Messenger, or Lock are installed unless one of those says so.

## Project overview

`magnetico-web` is a small torrent-index web interface for a
[magneticod](https://github.com/boramalper/magnetico) PostgreSQL database:

- **Two entity managers** (`config/packages/doctrine.yaml`): `default`
  (application schema, entities in `src/Entity`, migrations in
  `src/Migrations`) and `magneticod` (read-mostly torrent index, entities in
  `src/Magnetico/Entity`, no migrations of its own — magneticod owns that
  schema). Never point application writes at `magneticod`, and never change
  its schema from this codebase.
- The `ILIKE` DQL function (`App\Doctrine\ORM\AST\Ilike`, based on
  martin-georgiev/postgresql-for-doctrine) is registered on the `magneticod`
  entity manager and used by `App\Search\TorrentSearcher`.
- Deployment: Docker image built on `dunglas/frankenphp` (PHP 8.5, alpine)
  running Symfony in FrankenPHP **worker mode**. Worker mode is detected
  natively by `symfony/runtime` — there is no `APP_RUNTIME` variable and no
  runtime package; don't reintroduce either.
- The production Docker Compose stack lives separately in
  [skobkin/docker-stacks](https://git.skobk.in/skobkin/docker-stacks/src/branch/master/magnetico-web).
- CI: Woodpecker (`.woodpecker/ci.yaml`) — composer install, Symfony lints,
  php-cs-fixer, phpstan, phpunit, Docker publish on tags.

## No PHP on the host: use Docker for everything

The development host has no PHP/Composer installed. Run all PHP tooling in
containers:

```bash
docker run --rm -v "$PWD:/app" -w /app php:8.5-cli-alpine sh -c '
  apk add --no-cache unzip git &&
  curl -sS https://getcomposer.org/download/latest-stable/composer.phar -o /usr/local/bin/composer &&
  chmod +x /usr/local/bin/composer &&
  composer install && composer <script>'
```

Watch out for container processes (running as root) creating root-owned files
in `.git/objects` or `vendor/` — fix with
`docker run --rm -v "$PWD:/app" php:8.5-cli-alpine chown -R <uid>:<gid> /app`.

Composer scripts available (run inside the container): `composer lint`
(container/twig/yaml lints), `composer cs-check` / `composer cs-fix`,
`composer stan` (PHPStan), `composer test` (PHPUnit).

## Data safety rules

- The `magneticod` database is user data. Schema changes to `src/Entity` /
  `src/Magnetico/Entity` must always go through generated migrations
  (`make:migration --em=default`), reviewed for destructive operations before
  commit.
- Never ship `doctrine:schema:update`, hand-written DDL in controllers, or
  config that silently changes generated DDL (e.g. naming strategy,
  identity generation preferences).
- Every user-facing/breaking change must be recorded in `UPGRADE.md`
  (see below).

## UPGRADE.md maintenance (required)

`UPGRADE.md` is the changelog operators read before deploying. Whenever a
change requires action from the person deploying (renamed environment
variables, new required services, invalidated sessions/cookies, schema
changes, backup recommendations), add a brief, concrete note **grouped under
a dated version header** (`## YYYY-MM-DD <version>`; create the header for
the upcoming release if it doesn't exist yet). Include short migration
instructions — what to copy where, what to run, what behavior changes. Do not
record purely internal refactors there.

## Known constraints

- `predis/predis` is deliberately held on **2.x**: `symfony/cache`'s Redis
  adapter (used by the `magneticod.cache` pool) doesn't declare predis 3
  support yet. Don't bump to 3.x unless symfony/cache allows it.
- `config/packages/security.yaml` uses a sodium password hasher and an API
  token authenticator (`App\Security\ApiTokenAuthenticator`, header
  `api-token`) alongside form login — keep both paths working.
- reCAPTCHA v3 (`karser/karser-recaptcha3-bundle`) guards registration and
  password-reset request forms; it's disabled in `dev`/`test` environments
  via config, so local flows work without keys.

## Ask before generating

If the task doesn't specify, ask rather than guess:

- Persistence: Doctrine ORM, Doctrine ODM, or none?
- Interface: server-rendered (Twig), API (Serializer, maybe API Platform), or both?
- Auth: SecurityBundle, and which authenticator?

If you can't ask (no interactive channel), state the assumption you're making and
pick the smallest option (e.g. no persistence layer) rather than scaffolding a
full stack nobody asked for.

## Adding features: Flex, not hand-wiring

Install new capabilities with `composer require <package>` (e.g. `symfony/lock`,
`symfony/messenger`, `orm-pack`) and let the Flex recipe register the bundle and
generate its config. Don't hand-edit `config/bundles.php` or hand-write a bundle's
base config; that's what the recipe is for. Don't skip a good-fit component just
because it isn't installed yet; installing it is one command.

## Conventions

Follow https://symfony.com/doc/current/best_practices.html to write idiomatic
Symfony:

- Use PHP attributes for framework metadata, and not only on controllers:
  `#[Route]`, `#[MapRequestPayload]`, `#[IsGranted]` on actions, `#[Assert\...]`
  on properties, `#[AsCommand]`, `#[AsEventListener]`, `#[AsMessageHandler]`, and
  `#[AsAlias]` / `#[AsTaggedItem]` / `#[Autoconfigure]` on services. No YAML or
  XML routing.
- Rely on autowiring and autoconfiguration. Type-hint constructor arguments and
  let the container resolve them. Where a type-hint can't express it, stay in the
  class with `#[Autowire]` (parameters, env vars, expressions) or `#[Target]` (one
  of several implementations of an interface). A YAML service definition is the
  last resort, not the first.
- Controllers extend `AbstractController`, stay thin, and delegate to services.
- Use the framework for what it already does: Form for server-rendered forms,
  Validator for validation, Serializer for JSON, Messenger for async work,
  Security (voters, authenticators) for access control, Twig `path()`/`url()`
  instead of hardcoded URLs.
- Before hand-writing infrastructure (locks, queues, caches, HTTP clients,
  mailers, schedulers) or reaching for a third-party library, check whether a
  Symfony component covers it. It usually does.

Three specifics worth spelling out, because they are easy to get wrong:

- Bind request data with `#[MapRequestPayload]` / `#[MapQueryString]` on action
  arguments, which wires up Serializer and Validator for you, instead of calling
  `json_decode()` or `SerializerInterface` by hand. If neither package is
  installed yet, `composer require` them rather than falling back to manual
  parsing.
- Use constructor property promotion, and `readonly` for DTOs and value objects.
  Don't mark a service `readonly` if it might become `lazy: true`: a lazy proxy
  can't extend a `readonly` class.
- Use `symfony/lock` (`LockFactory`) for mutual exclusion. A hand-built flag or
  lock file looks fine in review and is usually wrong under concurrency.

## Everyday workflow

- Run the app locally with the PHP built-in server (`php -S 127.0.0.1:8080 -t public/`),
  the Symfony CLI (`symfony serve -d`), or the FrankenPHP container. Console
  commands run via `php bin/console ...` (in a container on this host — see above).
- When something fails, read `var/log/dev.log` and the web profiler
  (`/_profiler`) before changing code.
- Schema changes go through migrations
  (`bin/console make:migration --em=default`, then
  `doctrine:migrations:migrate --em=default`), never `doctrine:schema:update`
  or hand-written SQL.
- `.env` is committed and holds defaults only. Real secrets belong in `.env.local`
  (git-ignored) or the secrets vault (`bin/console secrets:set`), read via
  `%env(...)%`.

## Testing

`symfony/test-pack` and PHPUnit are installed. Functional tests extend
`KernelTestCase`/`WebTestCase`; unit tests extend `TestCase` directly
(`tests/Unit`, `tests/Functional`). Run `composer test` (or
`vendor/bin/phpunit`). The suite runs without Postgres/Redis: kernel boot
never opens connections, so keep it that way — a test needing a live service
must provision it (or be marked/skipped explicitly), never silently require
one. A feature isn't done until it has a test that exercises it the way a
caller would.

## Code style

The `@Symfony` php-cs-fixer ruleset (`.php-cs-fixer.dist.php`) and PHPStan
level 6 (`phpstan.neon.dist` with a baseline). Run `composer cs-fix` and
`composer stan`; the Woodpecker pipeline fails on both, so run them before
committing. Keep the phpstan baseline shrinking, not growing: fix new
violations instead of extending the baseline.

## Discover, don't guess

Framework APIs change between versions and your training data may be stale. Look
things up in the project instead of relying on memory:

- `bin/console about`: versions, environment, paths.
- `bin/console debug:router`, `debug:container`, `debug:autowiring <name>`,
  `debug:config <bundle>`, `config:dump-reference <bundle>`: what exists and how
  it is configured.
- `bin/console lint:container`, plus `lint:twig templates/` and
  `lint:yaml config/` where those packages are installed: validate before running.
- Read the installed source and docblocks under `vendor/`.
- Docs: https://symfony.com/doc/current/ (switch to the version matching
  `composer.json` if it differs).
