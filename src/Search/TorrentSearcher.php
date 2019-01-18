<?php

namespace App\Search;

use App\Magnetico\Entity\Torrent;
use App\Magnetico\Repository\TorrentRepository;
use Doctrine\ORM\{EntityManagerInterface, QueryBuilder};

class TorrentSearcher
{
    private const ORDER_DISABLED_FIELDS = ['infoHash'];

    /** @var TorrentRepository */
    private $torrentRepo;

    /** @var EntityManagerInterface */
    private $em;

    public function __construct(TorrentRepository $torrentRepo, EntityManagerInterface $em)
    {
        $this->torrentRepo = $torrentRepo;
        $this->em = $em;
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

        $where = $qb->expr()->andX();

        foreach ($this->splitQueryToParts($query) as $idx => $part) {
            $where->add($qb->expr()->like('LOWER(t.name)', ':part_'.$idx));
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
            && $this->em->getClassMetadata(Torrent::class)->hasField($orderBy)
        );
    }

    /** @return string[] */
    private function splitQueryToParts(string $query): array
    {
        $query = trim($query);
        $query = preg_replace('/\s+/', ' ', $query);

        return explode(' ', $query);
    }
}