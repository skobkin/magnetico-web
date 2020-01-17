<?php

namespace App\Magnetico\Repository;

use App\Magnetico\Entity\Torrent;
use Doctrine\Bundle\DoctrineBundle\Repository\ServiceEntityRepository;
use Doctrine\Persistence\ManagerRegistry;

class TorrentRepository extends ServiceEntityRepository
{
    public function __construct(ManagerRegistry $registry)
    {
        parent::__construct($registry, Torrent::class);
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
}