<?php

declare(strict_types=1);

namespace App\Tests\Unit\Api\V1\DTO;

use App\Api\V1\DTO\ListPage;
use Pagerfanta\Adapter\AdapterInterface;
use Pagerfanta\Pagerfanta;
use PHPUnit\Framework\TestCase;

class ListPageTest extends TestCase
{
    public function testCreatesFromPagerWithPagerMetadata(): void
    {
        $items = new \ArrayIterator(['a', 'b', 'c']);
        $adapter = $this->createStub(AdapterInterface::class);
        $adapter->method('getNbResults')->willReturn(42);
        $adapter->method('getSlice')->willReturn($items);

        $pager = new Pagerfanta($adapter);
        $pager->setMaxPerPage(10);
        $pager->setCurrentPage(3);

        $page = ListPage::createFromPager($pager);

        self::assertSame(42, $page->getNumberOfResults());
        self::assertSame(10, $page->getMaxPerPage());
        self::assertSame(3, $page->getCurrentPage());
        self::assertSame($items, $page->getItems());
    }
}
