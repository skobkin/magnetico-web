# https://github.com/roadrunner-server/roadrunner/pkgs/container/roadrunner
FROM ghcr.io/roadrunner-server/roadrunner:latest AS roadrunner
FROM php:8.2-alpine

ENV PHP_TIMEZONE Europe/Moscow
ENV APP_ENV=prod

WORKDIR /app

COPY --from=roadrunner /usr/bin/rr /app/bin/rr
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
    pecl install igbinary-3.2.7 && \
    docker-php-ext-enable igbinary && \
    docker-php-ext-enable intl && \
    docker-php-ext-enable pdo_pgsql && \
    mv "$PHP_INI_DIR/php.ini-production" "$PHP_INI_DIR/php.ini" && \
    echo "date.timezone = $PHP_TIMEZONE" > $PHP_INI_DIR/conf.d/timezone.ini && \
    mkdir -p /usr/local/bin && \
    wget -O /usr/local/bin/composer https://getcomposer.org/download/latest-stable/composer.phar && \
    chmod +x /usr/local/bin/composer && \
    ls -la /app && ls -la /app/bin && \
    chmod +x /app/bin/console && \
    /usr/local/bin/composer install --no-dev --no-progress --no-interaction --optimize-autoloader && \
    apk del autoconf build-base git linux-headers postgresql-dev

EXPOSE 8080/tcp

VOLUME /var/log

HEALTHCHECK --retries=3 --timeout=10s CMD curl http://localhost:8080 || exit 1

CMD rr serve -c .rr.yaml
