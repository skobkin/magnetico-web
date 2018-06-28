<?php

namespace App\User;

use App\Entity\{Invite, User};
use App\Repository\InviteRepository;

class InviteManager
{
    /** @var InviteRepository */
    private $inviteRepo;

    /** @var int Which amount of invites we need to give to the new user */
    private $newUserInvites;

    public function __construct(InviteRepository $inviteRepo, int $newUserInvites = 0)
    {
        $this->inviteRepo = $inviteRepo;
        $this->newUserInvites = $newUserInvites;
    }

    /**
     * @return Invite[]
     */
    public function createInvitesForUser(User $user, int $forceAmount = null): iterable
    {
        if (!in_array('ROLE_USER', $user->getRoles(), true)) {
            return [];
        }

        $amount = (null !== $forceAmount) ? $forceAmount : $this->newUserInvites;

        $invites = [];

        for ($i = 0; $i < $amount; $i++) {
            $invite = new Invite($user);
            $this->inviteRepo->add($invite);
            $invites[] = $invite;
        }

        return $invites;
    }
}