<?php

declare(strict_types=1);

namespace App\Tests\Unit\Search;

use App\Magnetico\Repository\TorrentRepository;
use App\Search\TorrentSearcher;
use Doctrine\ORM\EntityManagerInterface;
use Doctrine\ORM\Mapping\ClassMetadata;
use Doctrine\ORM\QueryBuilder;
use PHPUnit\Framework\Attributes\DataProvider;
use PHPUnit\Framework\TestCase;

class TorrentSearcherTest extends TestCase
{
    /**
     * QueryBuilder::orderBy() deprecates string sort directions since ORM 3.7
     * (https://github.com/doctrine/orm/issues/11313) — SortDirection instances
     * must be passed instead.
     */
    #[DataProvider('provideSortDirections')]
    public function testOrderingPassesSortDirectionEnumWithoutTriggeringDeprecation(string $order, string $expectedDql): void
    {
        \set_error_handler(static function (int $errno, string $errstr): bool {
            if (\E_USER_DEPRECATED === $errno) {
                throw new \UnexpectedValueException('Deprecation triggered: '.$errstr);
            }

            return false;
        });

        try {
            $queryBuilder = $this->createSearcher()->createSearchQueryBuilder('ubuntu', 'id', $order);
        } finally {
            \restore_error_handler();
        }

        self::assertStringContainsString($expectedDql, $queryBuilder->getDQL());
    }

    public static function provideSortDirections(): iterable
    {
        yield 'ascending' => ['asc', 'ORDER BY t.id ASC'];
        yield 'descending' => ['desc', 'ORDER BY t.id DESC'];
    }

    public function testInvalidSortOrderIsRejected(): void
    {
        $this->expectException(\InvalidArgumentException::class);
        $this->expectExceptionMessage('Invalid sort order');

        $this->createSearcher()->createSearchQueryBuilder('ubuntu', 'id', 'sideways');
    }

    private function createSearcher(): TorrentSearcher
    {
        $metadata = $this->createStub(ClassMetadata::class);
        $metadata->method('hasField')->willReturn(true);

        return new TorrentSearcher($this->createTorrentRepository(), $metadata);
    }

    private function createTorrentRepository(): TorrentRepository
    {
        $queryBuilder = new QueryBuilder($this->createStub(EntityManagerInterface::class));
        $repository = $this->createStub(TorrentRepository::class);
        $repository->method('createQueryBuilder')->willReturn($queryBuilder);

        return $repository;
    }
}
