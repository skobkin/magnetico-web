<?php

declare(strict_types=1);

namespace App\Doctrine\ORM;

use Doctrine\ORM\Query\TokenType;

/**
 * @internal
 * @see https://github.com/martin-georgiev/postgresql-for-doctrine
 */
final class DoctrineOrm
{
    public static function isPre219(): bool
    {
        return !\class_exists(TokenType::class);
    }
}
