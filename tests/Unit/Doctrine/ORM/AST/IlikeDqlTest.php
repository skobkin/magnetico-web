<?php

declare(strict_types=1);

namespace App\Tests\Unit\Doctrine\ORM\AST;

use App\Doctrine\ORM\AST\Ilike;
use Doctrine\DBAL\DriverManager;
use Doctrine\ORM\EntityManager;
use Doctrine\ORM\ORMSetup;
use PHPUnit\Framework\TestCase;

/**
 * Covers the ILIKE DQL function (App\Doctrine\ORM\AST\BaseFunction subclass)
 * end to end: DQL parsing and SQL generation, without touching a database.
 */
class IlikeDqlTest extends TestCase
{
    public function testIlikeCompilesToPostgreSqlIlikeOperator(): void
    {
        $config = ORMSetup::createAttributeMetadataConfiguration(
            [dirname(__DIR__, 4).'/src/Magnetico/Entity'],
            true,
        );
        $config->addCustomStringFunction('ILIKE', Ilike::class);
        $config->enableNativeLazyObjects(true);

        $em = new EntityManager(
            DriverManager::getConnection(['driver' => 'pdo_pgsql', 'serverVersion' => '16']),
            $config,
        );

        // Same DQL shape TorrentSearcher produces
        $sql = $em->createQuery(
            'SELECT t FROM App\Magnetico\Entity\Torrent t WHERE ILIKE(t.name, :pattern) = TRUE'
        )->getSQL();

        self::assertStringContainsString('t0_.name ilike ?', $sql);
    }

    public function testIlikeCompilesBothArguments(): void
    {
        $config = ORMSetup::createAttributeMetadataConfiguration(
            [dirname(__DIR__, 4).'/src/Magnetico/Entity'],
            true,
        );
        $config->addCustomStringFunction('ILIKE', Ilike::class);
        $config->enableNativeLazyObjects(true);

        $em = new EntityManager(
            DriverManager::getConnection(['driver' => 'pdo_pgsql', 'serverVersion' => '16']),
            $config,
        );

        $sql = $em->createQuery(
            'SELECT t FROM App\Magnetico\Entity\Torrent t WHERE ILIKE(t.name, t.infoHash) = TRUE'
        )->getSQL();

        self::assertStringContainsString('t0_.name ilike t0_.info_hash', $sql);
    }
}
