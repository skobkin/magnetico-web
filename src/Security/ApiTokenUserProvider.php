<?php

namespace App\Security;

use App\Entity\User;
use App\Repository\ApiTokenRepository;
use Symfony\Component\Security\Core\Exception\{UnsupportedUserException, UsernameNotFoundException};
use Symfony\Component\Security\Core\User\{UserInterface, UserProviderInterface};

class ApiTokenUserProvider implements UserProviderInterface
{
    /** @var ApiTokenRepository */
    private $userRepo;

    public function __construct(ApiTokenRepository $userRepo)
    {
        $this->userRepo = $userRepo;
    }

    public function loadUserByUsername($username): User
    {
        if (null === $user = $this->userRepo->findUserByTokenKey($username)) {
            throw new UsernameNotFoundException(sprintf('Token \'%s\' is not found.', $username));
        }

        return $user;
    }

    public function refreshUser(UserInterface $user)
    {
        throw new UnsupportedUserException();
    }

    public function supportsClass($class): bool
    {
        return User::class === $class;
    }

}