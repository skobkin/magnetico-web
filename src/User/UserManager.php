<?php
declare(strict_types=1);

namespace App\User;

use App\Entity\{Invite, User};
use App\Repository\UserRepository;
use App\User\Exception\InvalidInviteException;
use Symfony\Component\PasswordHasher\Hasher\PasswordHasherFactoryInterface;

class UserManager
{
    private const DEFAULT_ROLES = ['ROLE_USER'];

    public function __construct(
        private readonly PasswordHasherFactoryInterface $hasherFactory,
        private readonly UserRepository                 $userRepo,
    ) {

    }

    public function createUser(string $username, string $password, string $email, array $roles = self::DEFAULT_ROLES): User
    {
        $user = new User(
            $username,
            $this->hasherFactory->getPasswordHasher(User::class),
            $password,
            $email,
            $roles
        );

        $this->userRepo->add($user);

        return $user;
    }

    public function createUserByInvite(string $username, string $password, string $email, Invite $invite, array $roles = self::DEFAULT_ROLES): User
    {
        if (null !== $invite->getUsedBy()) {
            throw new InvalidInviteException();
        }

        $user = $this->createUser($username, $password, $email, $roles);

        $invite->use($user);

        return $user;
    }
}
