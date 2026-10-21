<?php

declare(strict_types=1);

namespace App\Tests\Unit\Pager;

use App\Api\V1\DTO\ListPage;
use App\Pager\PagelessDoctrineORMAdapter;
use Doctrine\ORM\Query;
use Doctrine\ORM\QueryBuilder;
use Pagerfanta\Pagerfanta;
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
        // Query::execute() hydrates objects into a plain array.
        $query->expects(self::once())->method('execute')->willReturn(['torrent']);

        $slice = (new PagelessDoctrineORMAdapter($query))->getSlice(20, 10);

        self::assertInstanceOf(\ArrayIterator::class, $slice);
        self::assertSame(['torrent'], iterator_to_array($slice));
    }

    public function testGetSlicePassesTraversablesThrough(): void
    {
        $query = $this->createQuery();
        $query->method('setFirstResult')->willReturnSelf();
        $query->method('setMaxResults')->willReturnSelf();
        $iterator = new \ArrayIterator(['torrent']);
        $query->expects(self::once())->method('execute')->willReturn($iterator);

        self::assertSame($iterator, (new PagelessDoctrineORMAdapter($query))->getSlice(0, 10));
    }

    public function testGetSliceAcceptsQueryBuilder(): void
    {
        $query = $this->createQuery();
        $query->expects(self::once())->method('setFirstResult')->with(0)->willReturnSelf();
        $query->expects(self::once())->method('setMaxResults')->with(10)->willReturnSelf();
        $query->expects(self::once())->method('execute')->willReturn(['torrent']);

        $builder = $this->createMock(QueryBuilder::class);
        $builder->expects(self::once())->method('getQuery')->willReturn($query);

        $slice = (new PagelessDoctrineORMAdapter($builder))->getSlice(0, 10);

        self::assertInstanceOf(\ArrayIterator::class, $slice);
        self::assertSame(['torrent'], iterator_to_array($slice));
    }

    public function testSliceCanBeConsumedByListPageDto(): void
    {
        $query = $this->createQuery();
        $query->method('setFirstResult')->willReturnSelf();
        $query->method('setMaxResults')->willReturnSelf();
        $query->expects(self::once())->method('execute')->willReturn(['torrent']);

        $pager = new Pagerfanta(new PagelessDoctrineORMAdapter($query));
        $pager->setMaxPerPage(10);
        $pager->setCurrentPage(1);

        $page = ListPage::createFromPager($pager);

        self::assertSame(['torrent'], iterator_to_array($page->getItems()));
        self::assertSame(1, $page->getNumberOfResults());
    }

    private function createQuery(): MockObject&Query
    {
        return $this->createMock(Query::class);
    }
}
