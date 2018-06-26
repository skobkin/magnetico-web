<?php

namespace App\Repository;

use App\Entity\{ApiToken, User};
use Doctrine\Bundle\DoctrineBundle\Repository\ServiceEntityRepository;
use Doctrine\ORM\Query\Expr\Join;
use Symfony\Bridge\Doctrine\RegistryInterface;

class ApiTokenRepository extends ServiceEntityRepository
{
    public function __construct(RegistryInterface $registry)
    {
        parent::__construct($registry, ApiToken::class);
    }

    public function add(ApiToken $token): void
    {
        $this->getEntityManager()->persist($token);
    }

    public function findUserByTokenKey(string $tokenKey): ?User
    {
        $qb = $this->getEntityManager()->createQueryBuilder();
        $qb
            ->select('u')
            ->from(User::class, 'u')
            ->innerJoin(ApiToken::class, 'at', Join::WITH, 'at.user = u')
            ->where('at.key = :tokenKey')
            ->setParameter('tokenKey', $tokenKey)
        ;

        return $qb->getQuery()->getOneOrNullResult();
    }
}