<?php

namespace App\Repository;

use App\Entity\Invite;
use Doctrine\Bundle\DoctrineBundle\Repository\ServiceEntityRepository;
use Symfony\Bridge\Doctrine\RegistryInterface;

class InviteRepository extends ServiceEntityRepository
{
    public function __construct(RegistryInterface $registry)
    {
        parent::__construct($registry, Invite::class);
    }

    public function add(Invite $invite): void
    {
        $this->getEntityManager()->persist($invite);
    }
}