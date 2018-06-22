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
composer install --no-dev --optimize-autoloader
```

After dependencies installation you may be needed to create `.env` file (see `.env.dist`) 
or set appropriate [environment variables](https://en.wikipedia.org/wiki/Environment_variable)
for production usage.

## Database configuration

See [Symfony database configuration](https://symfony.com/doc/current/doctrine.html#configuring-the-database)
documentation for more details.

## Web assets installation

```bash
php app/console assets:install public --symlink
```
