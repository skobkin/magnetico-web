<?php
declare(strict_types=1);

namespace App\Controller;

use App\Entity\{ApiToken, User};
use App\Repository\{ApiTokenRepository, InviteRepository};
use App\Form\Data\PasswordChangeData;
use App\Form\PasswordChangeType;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\Form\Extension\Core\Type\SubmitType;
use Symfony\Component\Form\FormInterface;
use Symfony\Component\HttpFoundation\{Request, Response};
use Symfony\Component\PasswordHasher\Hasher\PasswordHasherFactoryInterface;

class AccountController extends AbstractController
{
    public function account(
        InviteRepository $inviteRepo,
        ApiTokenRepository $apiTokenRepo
    ): Response {
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

    public function changePassword(
        Request $request,
        EntityManagerInterface $em,
        PasswordHasherFactoryInterface $hasherFactory,
    ): Response {
        $data = new PasswordChangeData();
        $form = $this->createChangePasswordForm($data);
        $form->handleRequest($request);

        if ($form->isSubmitted() && $form->isValid()) {
            /** @var User $user */
            $user = $this->getUser();
            $hasher = $hasherFactory->getPasswordHasher($user);

            $user->changePassword($hasher, $data->newPassword);
            $em->flush();
            $this->addFlash('success', 'Password changed.');

            return $this->redirectToRoute('user_account');
        }

        return $this->renderForm('Account/password.html.twig', [
            'form' => $form,
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

    private function createChangePasswordForm(PasswordChangeData $data): FormInterface
    {
        return $this
            ->createForm(PasswordChangeType::class, $data, [
                'action' => $this->generateUrl('user_account_password_change'),
            ])
            ->add('submit', SubmitType::class);
    }
}
