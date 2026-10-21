<?php

declare(strict_types=1);

namespace App\Repository;

use App\Entity\Invite;
use App\Entity\User;
use Doctrine\Bundle\DoctrineBundle\Repository\ServiceEntityRepository;
use Doctrine\Persistence\ManagerRegistry;

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
            ->orderBy('i.id', \SortDirection::Ascending)
            ->setParameter('user', $user->getId())
        ;

        return $qb->getQuery()->getResult();
    }
}
