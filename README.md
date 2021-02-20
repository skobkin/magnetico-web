[![Codeship Status for skobkin/magnetico-web](https://app.codeship.com/projects/9da4d3e0-57cf-0136-9885-5644a850740d/status?branch=master)](https://app.codeship.com/projects/295041)
[![Total Downloads](https://poser.pugx.org/skobkin/magnetico-web/downloads)](https://packagist.org/packages/skobkin/magnetico-web)
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
