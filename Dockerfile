FROM php:8.1-fpm-alpine

ENV PHP_TIMEZONE Europe/Moscow

RUN apk update && \
    apk add postgresql-dev libpq && \
    docker-php-ext-configure igbinary && \
    docker-php-ext-configure intl && \
    docker-php-ext-configure pdo_pgsql && \
    docker-php-ext-install -j$(nproc) igbinary && \
    docker-php-ext-install -j$(nproc) instl && \
    docker-php-ext-install -j$(nproc) pdo_pgsql && \
    pecl install redis-5.3.7 && \
    docker-php-ext-enable redis && \
    apk del postgresql-dev && \
    mv "$PHP_INI_DIR/php.ini-production" "$PHP_INI_DIR/php.ini" && \
    echo "date.timezone = $PHP_TIMEZONE" > $PHP_INI_DIR/conf.d/timezone.ini
