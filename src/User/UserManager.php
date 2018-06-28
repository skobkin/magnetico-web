<?php

namespace App\User;

use App\Entity\{Invite, User};
use App\Repository\{InviteRepository, UserRepository};
use App\User\Exception\InvalidInviteException;
use Symfony\Component\Security\Core\Encoder\EncoderFactoryInterface;

class UserManager
{
    private const DEFAULT_ROLES = ['ROLE_USER'];

    /** @var UserRepository */
    private $userRepo;

    /** @var InviteRepository */
    private $inviteRepo;

    /** @var EncoderFactoryInterface */
    private $encoderFactory;

    public function __construct(EncoderFactoryInterface $encoderFactory, UserRepository $userRepo, InviteRepository $inviteRepo)
    {
        $this->userRepo = $userRepo;
        $this->inviteRepo = $inviteRepo;
        $this->encoderFactory = $encoderFactory;
    }

    public function createUser(string $username, string $password, string $email, array $roles = self::DEFAULT_ROLES): User
    {
        $encodedPassword = $this->encoderFactory->getEncoder(User::class)->encodePassword($password, null);

        $user = new User(
            $username,
            $encodedPassword,
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

        $user = $this->createUser($username, $password, $email,$roles);

        $invite->use($user);

        return $user;
    }
}