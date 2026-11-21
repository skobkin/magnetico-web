# https://hub.docker.com/r/dunglas/frankenphp
# https://frankenphp.dev/docs/docker/
FROM dunglas/frankenphp:1-php8.5-alpine

# PHP 8.5.13 (2026-11-19 security batch): refuse to build on unpatched PHP.
# Fixes CVE-2026-109618 / CVE-2026-109619 (2026-10-21) and multiple critical
# UAF/RCE advisories (GHSA-gr4f-wgx5-jpfv, GHSA-8mjj-2r7p-4hg2, GHSA-2fjv-p558-g82m,
# GHSA-g3vh-838c-65v7, GHSA-fm2m-fjf5-v3cp).
# Raise/remove deliberately when superseded by a newer security floor.
RUN php -r 'if (version_compare(PHP_VERSION, "8.5.13", "<")) { fwrite(STDERR, "base image ships vulnerable PHP ".PHP_VERSION." (< 8.5.13, 2026-11-19 security batch)\n"); exit(1); }'

# FrankenPHP < 1.13.6: refuse to build. All earlier releases embed a PHP without
# the security fixes published 2026-11-19 (1.13.6 ships PHP 8.5.13); see the
# php/frankenphp v1.13.6 release notes.
# Raise/remove deliberately when superseded by a newer security floor.
RUN FRANKENPHP_DETECTED_VERSION="$(frankenphp -v | sed -En 's/^FrankenPHP v([0-9]+\.[0-9]+\.[0-9]+).*/\1/p')" && \
    php -r 'if (version_compare($argv[1] ?? "", "1.13.6", "<")) { fwrite(STDERR, "base image ships vulnerable FrankenPHP ".($argv[1] !== "" ? $argv[1] : "of unknown version")." (< 1.13.6)\n"); exit(1); }' "$FRANKENPHP_DETECTED_VERSION"

ENV PHP_TIMEZONE Europe/Moscow
ENV APP_ENV=prod
# FrankenPHP worker mode is detected natively by symfony/runtime (>= 7.4),
# no APP_RUNTIME override needed.
ENV FRANKENPHP_CONFIG="worker ./public/index.php"
# Disable HTTPS
# https://frankenphp.dev/docs/production/#preparing-your-app
ENV SERVER_NAME=:80

WORKDIR /app

COPY . /app/

RUN apk update && \
    # linux-headers for this: https://github.com/php/php-src/issues/8681
    apk add autoconf build-base git icu libpq linux-headers postgresql-dev && \
    docker-php-ext-configure intl && \
    docker-php-ext-configure pdo_pgsql && \
    docker-php-ext-configure sockets && \
    docker-php-ext-install -j$(nproc) intl && \
    docker-php-ext-install -j$(nproc) pdo_pgsql && \
    docker-php-ext-install -j$(nproc) sockets && \
    # Pinned to the release PECL currently serves for PHP 8.5 (an RC marked
    # stable upstream); bump deliberately after checking PHP 8.5 compatibility.
    pecl install igbinary-3.2.17RC1 && \
    docker-php-ext-enable igbinary && \
    docker-php-ext-enable intl && \
    docker-php-ext-enable pdo_pgsql && \
    # Enable PHP production settings
    mv "$PHP_INI_DIR/php.ini-production" "$PHP_INI_DIR/php.ini" && \
    echo "date.timezone = $PHP_TIMEZONE" > $PHP_INI_DIR/conf.d/timezone.ini && \
    mkdir -p /usr/local/bin && \
    wget -O /usr/local/bin/composer https://getcomposer.org/download/latest-stable/composer.phar && \
    chmod +x /usr/local/bin/composer && \
    chmod +x /app/bin/console && \
    /usr/local/bin/composer install --no-dev --no-progress --no-interaction --optimize-autoloader && \
    apk del autoconf build-base git linux-headers postgresql-dev

EXPOSE 80/tcp

VOLUME /var/log
# Deliberately no VOLUME for /app/var: the compiled prod cache lives there and
# must not outlive the image — an anonymous volume survives container recreation
# (docker compose default) and would keep a cache compiled by an older Symfony.

HEALTHCHECK --retries=3 --timeout=10s CMD curl http://localhost || exit 1
