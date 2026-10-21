[![Build Status](https://ci.skobk.in/api/badges/skobkin/magnetico-web/status.svg)](https://ci.skobk.in/skobkin/magnetico-web)

# Magnetico Web PHP

Magnetico Web is a simple web search interface for [magneticod](https://github.com/boramalper/magnetico) database.

## Requirements

- PHP **8.5+** with `intl`, `pdo_pgsql`, `sockets` extensions (igbinary recommended for cache)
- Composer 2
- PostgreSQL (two databases: the application database and the magneticod database)
- Redis (cache)
- [FrankenPHP](https://frankenphp.dev) or any PHP-capable web server for serving

The application runs on Symfony 8.1. When deployed in Docker, it uses the
official [FrankenPHP image](https://hub.docker.com/r/dunglas/frankenphp) in
[worker mode](https://frankenphp.dev/docs/worker/), which is detected natively
by `symfony/runtime` — no special runtime package or `APP_RUNTIME` variable needed.

# Installation

Application setup is quite simple:

## Getting the source code

### Using Git
```bash
git clone https://git.skobk.in/skobkin/magnetico-web.git
cd magnetico-web
```

### Using Composer
```bash
composer create-project skobkin/magnetico-web -s dev
cd magnetico-web
```

## Setting file access privileges
Set up appropriate [write permissions](https://symfony.com/doc/current/setup/file_permissions.html) for `var/cache` and `var/log`.

## Installing dependencies (not needed after installation via Composer)

```bash
# In developer environment:
composer install

# In production environment
# You should tell the app that it's running in the production environment.
# You can use environment variables or set it in the .env.local file like that:
echo 'APP_ENV=prod' > ./.env.local
composer install --no-dev --optimize-autoloader
```

After dependencies installation you may need to create `.env.local` file (see `.env` for reference) 
or set appropriate [environment variables](https://en.wikipedia.org/wiki/Environment_variable)
for production usage.

Check [Symfony documentation](https://symfony.com/doc/current/configuration.html#overriding-environment-values-via-env-local) for more details about `.env` files.

## Database configuration

See [Symfony database configuration](https://symfony.com/doc/current/doctrine.html#configuring-the-database)
documentation for more details.

You **must** set environment variables for both databases: magneticod's and magnetico-web's PostgreSQL.

### Schema considerations

Make sure that `magnetico-web` and `magneticod` are using the same schema for storing torrents in the PostgreSQL database.
Check `magneticod` docs [here](https://github.com/boramalper/magnetico/tree/master/pkg#postgresql-database-engine-only-magneticod-part-implemented)
and make sure that `schema` parameter either not set or set to `magneticod` (default value).

`magnetico-web` uses `magneticod` schema to search for torrents so if you set `magneticod` to use another schema search **will not work**.

## Database schema migration

```bash
# Only for 'default' EntityManager (Application entities)
php bin/console doctrine:migrations:migrate --em=default
```

## Web assets installation

```bash
php bin/console assets:install public --symlink
```

## User creation

```bash
# see --help for more info
# If you don't specify the password it'll be requested from you in the command line
php bin/console user:add <your_username> <your_email> [your_password] [--invites=10]
```

## Giving invites to the user

```bash
# see --help for more info
php bin/console invite:add <username> <number-of-invites>
```

## Running locally

### Using the PHP built-in web server (development)

```bash
symfony server:start -d
# or without the Symfony CLI:
php -S 127.0.0.1:8080 -t public/
```

### Using FrankenPHP (worker mode)

```bash
# Requires a FrankenPHP binary and a Caddyfile with `php_server` + worker directive,
# see https://frankenphp.dev/docs/symfony/
FRANKENPHP_CONFIG="worker ./public/index.php" frankenphp run
```

## Enabling dev mode

```shell
echo 'APP_ENV=dev' > .env.local
```

## Running in Docker

A ready-to-use Docker Compose stack (with `.env` reference) lives in the
[docker-stacks](https://git.skobk.in/skobkin/docker-stacks/src/branch/master/magnetico-web)
repository.

The Docker image is based on `dunglas/frankenphp` running the application in
worker mode. When running in Docker **DO NOT FORGET** to use Nginx, Caddy or
another reverse-proxy in front (FrankenPHP can also serve TLS itself) and
properly set the `TRUSTED_PROXIES` environment variable. You can read more
about it [here](https://symfony.com/doc/current/deployment/proxies.html#but-what-if-the-ip-of-my-reverse-proxy-changes-constantly).

### Environment variables

```dotenv
APP_SECRET=change-me

APP_DATABASE_URL=postgres://magnetico-web:password@host.docker.internal:5432/magnetico-web?application_name=magnetico_web
MAGNETICOD_DATABASE_URL=postgres://magneticod:password@host.docker.internal:5432/magneticod?application_name=magnetico_web

REDIS_DSN=redis://host.docker.internal:6379/0

# BE CAREFUL WITH 'REMOTE_ADDR'. Use ONLY with trusted reverse-proxy
TRUSTED_PROXIES=127.0.0.1,REMOTE_ADDR

###> sentry/sentry-symfony ###
SENTRY_DSN=https://abcabcdaefdaef@sentry.io/123456
###< sentry/sentry-symfony ###

###> symfony/mailer ###
MAILER_DSN=smtp://mail@domain.tld:password@smtp.domain.tld:587
MAILER_FROM=no-reply@domain.tld
###< symfony/mailer ###

###> karser/karser-recaptcha3-bundle ###
# reCAPTCHA v3 keys, see https://www.google.com/recaptcha/admin/create
RECAPTCHA3_KEY=key
RECAPTCHA3_SECRET=secret
###< karser/karser-recaptcha3-bundle ###
```

## Upgrading

Check [UPGRADE.md](UPGRADE.md) for user-facing and breaking changes before
deploying a new version.
