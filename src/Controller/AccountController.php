<?php

namespace App\Controller;

use App\Entity\User;
use App\Repository\InviteRepository;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\HttpFoundation\Response;

class AccountController extends AbstractController
{
    public function invites(InviteRepository $inviteRepo): Response
    {
        /** @var User $user */
        if (null === $user = $this->getUser()) {
            throw $this->createAccessDeniedException('User not found exception');
        }

        return $this->render('Account/invites.html.twig', [
            'invites' => $inviteRepo->findInvitesByUser($user),
        ]);
    }
}