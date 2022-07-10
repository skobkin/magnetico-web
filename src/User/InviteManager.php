<?php
declare(strict_types=1);

namespace App\User;

use App\Entity\{Invite, User};
use App\Repository\InviteRepository;

class InviteManager
{
    public function __construct(
        private readonly InviteRepository $inviteRepo,
        private readonly int $newUserInvites = 0
    ) {

    }

    /**
     * @return Invite[]
     */
    public function createInvitesForUser(User $user, int $forceAmount = null): iterable
    {
        if (!in_array('ROLE_USER', $user->getRoles(), true)) {
            return [];
        }

        $amount = $forceAmount ?? $this->newUserInvites;

        $invites = [];

        for ($i = 0; $i < $amount; $i++) {
            $invite = new Invite($user);
            $this->inviteRepo->add($invite);
            $invites[] = $invite;
        }

        return $invites;
    }
}
