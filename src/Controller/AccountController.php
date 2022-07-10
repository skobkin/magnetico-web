<?php
declare(strict_types=1);

namespace App\Controller;

use App\Entity\{ApiToken, User};
use App\Repository\{ApiTokenRepository, InviteRepository};
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\HttpFoundation\Response;

class AccountController extends AbstractController
{
    public function account(InviteRepository $inviteRepo, ApiTokenRepository $apiTokenRepo): Response
    {
        /** @var User $user */
        if (null === $user = $this->getUser()) {
            throw $this->createAccessDeniedException('User not found exception');
        }

        return $this->render(
            'Account/account.html.twig', [
            'invites' => $inviteRepo->findInvitesByUser($user),
            'tokens' => $apiTokenRepo->findBy(['user' => $user->getId()]),
            'user' => $user,
        ]);
    }

    public function addApiToken(EntityManagerInterface $em): Response
    {
        /** @var User|null $user */
        if (null === $user = $this->getUser()) {
            throw $this->createAccessDeniedException();
        }

        $token = new ApiToken($user);
        $em->persist($token);
        $em->flush();

        $this->addFlash('success', 'API token created.');

        return $this->redirectToRoute('user_account');
    }

    public function revokeApiToken(string $key, ApiTokenRepository $repo, EntityManagerInterface $em): Response
    {
        $token = $repo->findOneBy(['key' => $key]);

        if (null === $token || $token->getUser() !== $this->getUser()) {
            throw $this->createNotFoundException('Token not found');
        }

        $em->remove($token);
        $em->flush();

        $this->addFlash('success', 'API token removed.');

        return $this->redirectToRoute('user_account');
    }
}
