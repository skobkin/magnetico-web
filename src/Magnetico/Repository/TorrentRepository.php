<?php

namespace App\Magnetico\Repository;

use Doctrine\Bundle\DoctrineBundle\Repository\ServiceEntityRepository;
use Doctrine\ORM\QueryBuilder;
use Symfony\Bridge\Doctrine\RegistryInterface;

class TorrentRepository extends ServiceEntityRepository
{
    public function __construct(RegistryInterface $registry)
    {
        parent::__construct($registry, \App\Magnetico\Entity\Torrent::class);
    }

    public function getTorrentsTotalCount(): int
    {
        $qb = $this->createQueryBuilder('t')
            ->select('COUNT(t.id)')
        ;

        try {
            return (int) $qb->getQuery()->getSingleScalarResult();
        } catch (\Exception $ex) {
            return 0;
        }
    }

    public function createFindLikeQueryBuilder(string $query): QueryBuilder
    {
        $qb = $this->createQueryBuilder('t');

        $where = $qb->expr()->andX();

        $query = trim($query);
        $query = preg_replace('/\s+/', ' ', $query);

        $parts = explode(' ', $query);

        foreach ($parts as $idx => $part) {
            $where->add($qb->expr()->like('LOWER(t.name)', ':part_'.$idx));
            $qb->setParameter('part_'.$idx, '%'.strtolower($part).'%');
        }

        $qb->where($where);

        return $qb;
    }
}