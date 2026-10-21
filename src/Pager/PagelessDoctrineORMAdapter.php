<?php

declare(strict_types=1);

namespace App\Pager;

use Doctrine\ORM\Query;
use Doctrine\ORM\QueryBuilder;
use Pagerfanta\Doctrine\ORM\QueryAdapter;

/**
 * This adapter never makes COUNT() queries: the total result count of a search
 * over the magneticod index is unknown and too expensive to compute.
 *
 * QueryAdapter::getSlice() routes through ORM's OffsetPaginator since ORM 3.7,
 * and OffsetPaginator::paginate() resolves the total count on every call, so
 * the slice is taken directly from the query instead.
 *
 * @template T
 *
 * @extends QueryAdapter<T>
 */
class PagelessDoctrineORMAdapter extends QueryAdapter
{
    public function __construct(
        private readonly Query|QueryBuilder $query,
        bool $fetchJoinCollection = true,
        ?bool $useOutputWalkers = null,
    ) {
        parent::__construct($query, $fetchJoinCollection, $useOutputWalkers);
    }

    public function getNbResults(): int
    {
        return 1;
    }

    public function getSlice(int $offset, int $length): \Traversable
    {
        if (0 === $length) {
            return new \EmptyIterator();
        }

        $query = $this->query instanceof QueryBuilder ? $this->query->getQuery() : $this->query;
        $result = $query
            ->setFirstResult($offset)
            ->setMaxResults($length)
            ->execute();
        assert(is_iterable($result));

        // Object hydration yields a plain array, but consumers like ListPage
        // require a Traversable — the same contract QueryAdapter documents.
        return $result instanceof \Traversable ? $result : new \ArrayIterator($result);
    }
}
