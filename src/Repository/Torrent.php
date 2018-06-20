<?php

namespace App\Repository;

use Doctrine\Bundle\DoctrineBundle\Repository\ServiceEntityRepository;
use Symfony\Bridge\Doctrine\RegistryInterface;

class Torrent extends ServiceEntityRepository
{
    public function __construct(RegistryInterface $registry)
    {
        parent::__construct($registry, \App\Entity\Torrent::class);
    }

    public function getLastTorrents(int $number): array
    {
        $qb = $this->createQueryBuilder('t')
            ->orderBy('t.discoveredOn', 'DESC')
            ->setMaxResults($number)
        ;

        return $qb->getQuery()->getResult();
    }
}