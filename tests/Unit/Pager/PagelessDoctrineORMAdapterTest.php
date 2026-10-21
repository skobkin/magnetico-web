<?php

declare(strict_types=1);

namespace App\Tests\Unit\Pager;

use App\Pager\PagelessDoctrineORMAdapter;
use Doctrine\ORM\Query;
use Doctrine\ORM\QueryBuilder;
use PHPUnit\Framework\MockObject\MockObject;
use PHPUnit\Framework\TestCase;

class PagelessDoctrineORMAdapterTest extends TestCase
{
    public function testGetNbResultsReturnsStubWithoutExecutingAnything(): void
    {
        $query = $this->createQuery();
        $query->expects(self::never())->method('execute');

        self::assertSame(1, (new PagelessDoctrineORMAdapter($query))->getNbResults());
    }

    public function testGetSliceLimitsTheQueryDirectlyWithSingleExecution(): void
    {
        $query = $this->createQuery();
        $query->expects(self::once())->method('setFirstResult')->with(20)->willReturnSelf();
        $query->expects(self::once())->method('setMaxResults')->with(10)->willReturnSelf();
        $query->expects(self::once())->method('execute')->willReturn(['torrent']);

        self::assertSame(['torrent'], (new PagelessDoctrineORMAdapter($query))->getSlice(20, 10));
    }

    public function testGetSliceAcceptsQueryBuilder(): void
    {
        $query = $this->createQuery();
        $query->expects(self::once())->method('setFirstResult')->with(0)->willReturnSelf();
        $query->expects(self::once())->method('setMaxResults')->with(10)->willReturnSelf();
        $query->expects(self::once())->method('execute')->willReturn(['torrent']);

        $builder = $this->createMock(QueryBuilder::class);
        $builder->expects(self::once())->method('getQuery')->willReturn($query);

        self::assertSame(['torrent'], (new PagelessDoctrineORMAdapter($builder))->getSlice(0, 10));
    }

    private function createQuery(): MockObject&Query
    {
        return $this->createMock(Query::class);
    }
}
