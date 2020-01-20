<?php

namespace App\Repository;

use Doctrine\Persistence\ManagerRegistry;
use App\Entity\{Invite, User};
use Doctrine\Bundle\DoctrineBundle\Repository\ServiceEntityRepository;

class InviteRepository extends ServiceEntityRepository
{
    public function __construct(ManagerRegistry $registry)
    {
        parent::__construct($registry, Invite::class);
    }

    public function add(Invite $invite): void
    {
        $this->getEntityManager()->persist($invite);
    }

    /** @return Invite[] */
    public function findInvitesByUser(User $user): iterable
    {
        $qb = $this->createQueryBuilder('i');
        $qb
            ->select(['i', 'uu'])
            ->leftJoin('i.usedBy', 'uu')
            ->where('i.user = :user')
            ->orderBy('i.id', 'asc')
            ->setParameter('user', $user->getId())
        ;

        return $qb->getQuery()->getResult();
    }
}