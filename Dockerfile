# https://github.com/roadrunner-server/roadrunner/pkgs/container/roadrunner
FROM ghcr.io/roadrunner-server/roadrunner:latest AS roadrunner
FROM php:8.1-alpine

ENV PHP_TIMEZONE Europe/Moscow
ENV APP_ENV=prod

WORKDIR /app

COPY --from=roadrunner /usr/bin/rr /app/bin/rr
COPY . /app/

RUN apk update && \
    apk add autoconf build-base icu libpq postgresql-dev && \
    docker-php-ext-configure intl && \
    docker-php-ext-configure pdo_pgsql && \
    docker-php-ext-configure sockets && \
    docker-php-ext-install -j$(nproc) intl && \
    docker-php-ext-install -j$(nproc) pdo_pgsql && \
    docker-php-ext-install -j$(nproc) sockets && \
    pecl install igbinary-3.2.7 && \
    pecl install redis-5.3.7 && \
    docker-php-ext-enable igbinary && \
    docker-php-ext-enable intl && \
    docker-php-ext-enable pdo_pgsql && \
    docker-php-ext-enable redis && \
    apk del autoconf build-base postgresql-dev && \
    mv "$PHP_INI_DIR/php.ini-production" "$PHP_INI_DIR/php.ini" && \
    echo "date.timezone = $PHP_TIMEZONE" > $PHP_INI_DIR/conf.d/timezone.ini && \
    mkdir -p /usr/local/bin && \
    wget -O /usr/local/bin/composer https://getcomposer.org/download/latest-stable/composer.phar && \
    chmod +x /usr/local/bin/composer && \
    ls -la /app && ls -la /app/bin && \
    chmod +x /app/bin/console && \
    /usr/local/bin/composer install --no-dev --no-progress --no-interaction --optimize-autoloader

VOLUME /var/log

CMD ["/app/bin/rr", "serve"]
