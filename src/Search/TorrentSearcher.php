<?php
declare(strict_types=1);

namespace App\Search;

use App\Magnetico\Repository\TorrentRepository;
use Doctrine\ORM\{Mapping\ClassMetadata, QueryBuilder};

class TorrentSearcher
{
    /** Minimal word length to be used when searching in the database. */
    public const MIN_PART_LENGTH = 3;

    private const ORDER_DISABLED_FIELDS = ['infoHash'];

    /** @var TorrentRepository */
    private $torrentRepo;

    /** @var ClassMetadata */
    private $classMetadata;

    public function __construct(TorrentRepository $torrentRepo, ClassMetadata $classMetadata)
    {
        $this->torrentRepo = $torrentRepo;
        $this->classMetadata = $classMetadata;
    }

    public function createSearchQueryBuilder(string $query, string $orderBy = null, string $order = 'asc'): QueryBuilder
    {
        $qb = $this->createFindLikeSplitPartsQueryBuilder($query);

        if ($orderBy) {
            $this->addOrder($qb, $orderBy, $order);
        }

        return $qb;
    }

    private function createFindLikeSplitPartsQueryBuilder(string $query): QueryBuilder
    {
        $qb = $this->torrentRepo->createQueryBuilder('t');

        $queryParts = $this->splitQueryToParts($query);

        if (count($queryParts) < 1) {
            return $qb;
        }

        $where = $qb->expr()->andX();

        foreach ($queryParts as $idx => $part) {
            $where->add('ILIKE(t.name , :part_'.$idx.') = TRUE');
            $qb->setParameter('part_'.$idx, '%'.strtolower($part).'%');
        }

        $qb->where($where);

        return $qb;
    }

    private function addOrder(QueryBuilder $qb, string $orderBy, string $order): void
    {
        if (!\in_array(strtolower($order), ['asc', 'desc'])) {
            throw new \InvalidArgumentException('Invalid sort order');
        }

        if ($this->canOrderBy($orderBy)) {
            $qb->orderBy('t.'.$orderBy, $order);
        }
    }

    private function canOrderBy(string $orderBy): bool
    {
        return (
            !\in_array($orderBy, self::ORDER_DISABLED_FIELDS, true)
            && $this->classMetadata->hasField($orderBy)
        );
    }

    /** @return string[] */
    private function splitQueryToParts(string $query): array
    {
        $query = trim($query);
        $query = preg_replace('/\s+/', ' ', $query);

        $parts = explode(' ', $query);

        return $this->removeShortParts($parts);
    }

    /**
     * @param string[] $words
     *
     * @return string[]
     */
    private function removeShortParts(array $words): array
    {
        return array_filter($words, function (string $word) {
            return mb_strlen($word) >= self::MIN_PART_LENGTH;
        });
    }
}
