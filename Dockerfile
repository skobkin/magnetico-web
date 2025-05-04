# https://hub.docker.com/r/dunglas/frankenphp
# https://frankenphp.dev/docs/docker/
FROM dunglas/frankenphp:1-php8.4-alpine

ENV PHP_TIMEZONE Europe/Moscow
ENV APP_ENV=prod
ENV APP_RUNTIME=Runtime\\FrankenPhpSymfony\\Runtime
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
    pecl install igbinary-3.2.16 && \
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
VOLUME /app/var

HEALTHCHECK --retries=3 --timeout=10s CMD curl http://localhost || exit 1
