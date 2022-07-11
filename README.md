[![Build Status](https://ci.skobk.in/api/badges/skobkin/magnetico-web/status.svg)](https://ci.skobk.in/skobkin/magnetico-web)
[![License](https://poser.pugx.org/skobkin/magnetico-web/license)](https://packagist.org/packages/skobkin/magnetico-web)

# Magnetico Web PHP

Magnetico Web is a simple web search interface for [magneticod](https://github.com/boramalper/magnetico) database.

# Installation

Application setup is quite simple:

## Getting the source code

### Using Git
```bash
git clone https://skobkin@bitbucket.org/skobkin/magnetico-web.git
cd magnetico-web
```

### Using Composer
```bash
composer create-project skobkin/magnetico-web -s dev
cd magnetico-web
```

## Setting file access privileges
Set up appropriate [write permissions](https://symfony.com/doc/current/setup/file_permissions.html) for `var/cache` and `var/logs`.

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

Check [Symfony documentation](https://symfony.com/doc/5.1/configuration.html#overriding-environment-values-via-env-local) for more details about `.env` files.

You can also check [this post](https://symfony.com/doc/5.1/configuration/dot-env-changes.html) about `.env` changes in Symfony if you're updating from an 
old version of the project.

## Database configuration

See [Symfony database configuration](https://symfony.com/doc/current/doctrine.html#configuring-the-database)
documentation for more details.

You **must** set environment variables for both databases: magneticod's and magnetico-web's PostgreSQL.

### Schema considerations

Make sure that `magnetico-web` and `magneticod` are using the same schema for storing torrents in the PostgreSQL database.
Check `magneticod` docs [here](https://github.com/boramalper/magnetico/tree/master/pkg#postgresql-database-engine-only-magneticod-part-implemented)
and make sure that `schema` parameter either not set or set to `magneticod` (default value).

`magnetico-web` uses `magneticod` schema to search for torrents so if you set `magenticod` to use another schema search **will not work**.

## Database schema migration

```bash
# Only for 'default' EntityManager (Application entities)
php bin/console doc:mig:mig --em=default
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

## Enabling dev mode

```shell
echo 'APP_ENV=dev > .env.local'
```

## Running using [RoadRunner](https://roadrunner.dev) instead of [PHP-FPM](https://www.php.net/manual/en/install.fpm.php)

```shell
# First time only:
./vendor/bin/rr get --location bin/

# Running the server:
./bin/rr serve

# Running the server in dev mode (watching enabled)
bin/rr serve -c .rr.dev.yaml
```

Read more [here](https://github.com/baldinof/roadrunner-bundle) and [here](https://github.com/roadrunner-server/roadrunner).

### Trusted proxies

If you're running the app in RoadRunner and experiencing problems with proper URL generation (HTTP instead of HTTPS),
check beginning of the section about running in Docker below.

## Running in Docker

### Docker Compose example:

When running in Docker **DO NOT FORGET** to use Nginx or other reverse-proxy server and properly set `TRUSTED_PROXIES`
environment variable. You can read more about it [here](https://symfony.com/doc/current/deployment/proxies.html#but-what-if-the-ip-of-my-reverse-proxy-changes-constantly).

```yaml
version: '3.7'

services:
    magnetico-web:
        image: skobkin/magnetico-web
        container_name: magnetico-web
        hostname: magnetico-web
        extra_hosts:
            - 'host.docker.internal:host-gateway'
        ports:
            - "127.0.0.1:${EXT_HTTP_PORT:-8080}:8080/tcp"
        restart: unless-stopped
        user: "$UID"
        volumes:
            - "${LOG_PATH:-./var/log}:/app/var/log"
        env_file: .env
        logging:
            driver: "json-file"
            options:
                max-size: "${LOG_MAX_SIZE:-5m}"
                max-file: "${LOG_MAX_FILE:-5}"
```

Use dotenv file to configure this stack:

```dotenv
# Example with some useful parameters
APP_SECRET=qwerty

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

###> excelwebzone/recaptcha-bundle ###
EWZ_RECAPTCHA_SITE_KEY=key
EWZ_RECAPTCHA_SECRET=secret
###< excelwebzone/recaptcha-bundle ###
```
